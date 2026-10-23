package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	// jwtBackdate absorbs clock skew between us and GitHub, as GitHub's docs
	// recommend.
	jwtBackdate = 60 * time.Second
	// jwtLifetime keeps exp under GitHub's 10-minute ceiling even with the
	// backdated iat.
	jwtLifetime = 9 * time.Minute
	// tokenRefreshMargin re-mints the installation token (valid 1h) once it
	// has less than this left, so a token never expires mid-poll.
	tokenRefreshMargin = 10 * time.Minute
)

// ParsePrivateKey decodes a PEM-encoded RSA private key in either PKCS#1
// ("RSA PRIVATE KEY", what GitHub hands out) or PKCS#8 ("PRIVATE KEY") form.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private key: no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("private key: not a PKCS#1 or PKCS#8 RSA key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key: PKCS#8 key is %T, want RSA", k)
	}
	return rk, nil
}

// appJWT builds the RS256 JWT that authenticates as the App itself.
func (c *Client) appJWT(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{
		IAT: now.Add(-jwtBackdate).Unix(),
		EXP: now.Add(jwtLifetime).Unix(),
		ISS: strconv.FormatInt(c.appID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("encoding JWT claims: %w", err)
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// tokenLocked returns a valid installation token, minting a new one when
// none is cached or the cached one is within tokenRefreshMargin of expiry.
// c.mu must be held.
func (c *Client) tokenLocked(ctx context.Context) (string, error) {
	now := c.now()
	if c.token != "" && now.Add(tokenRefreshMargin).Before(c.tokenExp) {
		return c.token, nil
	}
	tok, exp, err := c.mintLocked(ctx, now)
	if err != nil {
		return "", err
	}
	c.token, c.tokenExp = tok, exp
	return tok, nil
}

// invalidateTokenLocked drops the cached token so the next request re-mints.
func (c *Client) invalidateTokenLocked() {
	c.token, c.tokenExp = "", time.Time{}
}

func (c *Client) mintLocked(ctx context.Context, now time.Time) (string, time.Time, error) {
	jwt, err := c.appJWT(now)
	if err != nil {
		return "", time.Time{}, &Error{Kind: KindAuth, Op: "minting installation token", Err: err, mint: true}
	}
	u := c.base.JoinPath("app", "installations", strconv.FormatInt(c.installationID, 10), "access_tokens")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return "", time.Time{}, &Error{Kind: KindAuth, Op: "minting installation token", Err: err, mint: true}
	}
	c.setHeaders(req, jwt)
	c.stats.Requests++
	res, err := c.hc.Do(req)
	if err != nil {
		return "", time.Time{}, &Error{Kind: KindNetwork, Op: "minting installation token", Err: err, mint: true}
	}
	defer res.Body.Close() //nolint:errcheck // body fully read below
	body, err := readBody(res.Body)
	if err != nil {
		return "", time.Time{}, &Error{Kind: KindNetwork, Op: "minting installation token", Err: err, mint: true}
	}
	// The access_tokens endpoint is billed to the App, not the installation,
	// so its rate-limit headers are deliberately not recorded.
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return "", time.Time{}, &Error{Kind: mintKind(res.StatusCode), Status: res.StatusCode, Op: "minting installation token", Err: errors.New(apiMessage(body)), mint: true}
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", time.Time{}, &Error{Kind: KindAuth, Op: "minting installation token", Err: errors.New("malformed access token response"), mint: true}
	}
	return out.Token, localExpiry(now, out.ExpiresAt, res.Header.Get("Date")), nil
}

// mintKind classifies a failed token mint: server trouble is "5xx", rate
// limiting "403", and anything else (bad App ID, key, or installation) "auth".
func mintKind(status int) string {
	switch {
	case status >= 500:
		return KindServer
	case status == http.StatusTooManyRequests:
		return KindForbidden
	default:
		return KindAuth
	}
}

// localExpiry converts GitHub's expires_at into our clock. The token's
// lifetime is measured against the server's own Date header, so local clock
// skew neither expires a fresh token early (re-minting on every request) nor
// keeps a dead one. Without a usable Date it falls back to expires_at, capped
// at the documented one-hour lifetime.
func localExpiry(localNow, expiresAt time.Time, date string) time.Time {
	if expiresAt.IsZero() {
		return localNow.Add(time.Hour)
	}
	if serverNow, err := http.ParseTime(date); err == nil {
		if ttl := expiresAt.Sub(serverNow); ttl > 0 && ttl <= 2*time.Hour {
			return localNow.Add(ttl)
		}
	}
	if capAt := localNow.Add(time.Hour); expiresAt.After(capAt) {
		return capAt
	}
	return expiresAt
}
