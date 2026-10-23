package github

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func pkcs1PEM(t *testing.T) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey(t))})
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

// fakeAPI mints sequential tokens tok-1, tok-2, ... valid for tokenTTL and
// routes everything else to handler.
type fakeAPI struct {
	t        *testing.T
	clock    *fakeClock
	tokenTTL time.Duration
	mu       sync.Mutex
	mints    int
	jwts     []string
	handler  http.HandlerFunc
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens" {
		f.mu.Lock()
		f.mints++
		n := f.mints
		f.jwts = append(f.jwts, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("tok-%d", n),
			"expires_at": f.clock.Now().Add(f.tokenTTL).UTC().Format(time.RFC3339),
		})
		return
	}
	f.handler(w, r)
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *fakeAPI, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	api := &fakeAPI{t: t, clock: clock, tokenTTL: time.Hour, handler: h}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	c, err := New(7, 42, pkcs1PEM(t), Options{BaseURL: srv.URL, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c, api, clock
}

func TestParsePrivateKey(t *testing.T) {
	k := rsaKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ec8, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		pem     []byte
		wantErr bool
	}{
		{"pkcs1", pkcs1PEM(t), false},
		{"pkcs8", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), false},
		{"not pem", []byte("hello"), true},
		{"garbage block", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("nope")}), true},
		{"ecdsa", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ec8}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePrivateKey(tt.pem)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil || !got.Equal(k) {
				t.Fatalf("ParsePrivateKey = %v, %v", got, err)
			}
		})
	}
}

func TestAppJWT(t *testing.T) {
	c, _, clock := newTestClient(t, nil)
	tok, err := c.appJWT(clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	enc := base64.RawURLEncoding
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&rsaKey(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var hdr struct{ Alg string }
	var claims struct {
		IAT, EXP int64
		ISS      string
	}
	h, _ := enc.DecodeString(parts[0])
	cl, _ := enc.DecodeString(parts[1])
	if err := json.Unmarshal(h, &hdr); err != nil || hdr.Alg != "RS256" {
		t.Fatalf("header %s: %v", h, err)
	}
	if err := json.Unmarshal(cl, &claims); err != nil {
		t.Fatal(err)
	}
	now := clock.Now().Unix()
	if claims.ISS != "7" || claims.IAT != now-60 || claims.EXP > now+600 || claims.EXP <= now {
		t.Fatalf("claims = %+v (now %d)", claims, now)
	}
}

func TestTokenRefresh(t *testing.T) {
	tests := []struct {
		name      string
		advance   time.Duration
		wantMints int
	}{
		{name: "fresh token reused", advance: 30 * time.Minute, wantMints: 1},
		{name: "near expiry re-minted", advance: 51 * time.Minute, wantMints: 2},
		{name: "expired re-minted", advance: 2 * time.Hour, wantMints: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen []string
			c, api, clock := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				seen = append(seen, r.Header.Get("Authorization"))
				_, _ = w.Write([]byte(`{}`))
			})
			ctx := context.Background()
			if _, err := c.Get(ctx, "/x"); err != nil {
				t.Fatal(err)
			}
			clock.Add(tt.advance)
			if _, err := c.Get(ctx, "/x"); err != nil {
				t.Fatal(err)
			}
			if api.mints != tt.wantMints {
				t.Fatalf("mints = %d, want %d", api.mints, tt.wantMints)
			}
			want := fmt.Sprintf("Bearer tok-%d", tt.wantMints)
			if seen[1] != want {
				t.Fatalf("second request auth = %q, want %q", seen[1], want)
			}
		})
	}
}

func TestUnauthorizedRetry(t *testing.T) {
	t.Run("retries once with a fresh token", func(t *testing.T) {
		c, api, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "Bearer tok-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		res, err := c.Get(context.Background(), "/x")
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Body) != `{"ok":true}` || api.mints != 2 {
			t.Fatalf("body %s mints %d", res.Body, api.mints)
		}
	})
	t.Run("persistent 401 is an auth error", func(t *testing.T) {
		calls := 0
		c, api, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		})
		_, err := c.Get(context.Background(), "/x")
		if KindOf(err) != KindAuth || calls != 2 || api.mints != 2 {
			t.Fatalf("err %v kind %s calls %d mints %d", err, KindOf(err), calls, api.mints)
		}
	})
}

func TestETagCache(t *testing.T) {
	var gotINM []string
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotINM = append(gotINM, r.Header.Get("If-None-Match"))
		w.Header().Set("X-RateLimit-Remaining", "4999")
		if r.Header.Get("If-None-Match") == `W/"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `W/"v1"`)
		w.Header().Set("Link", `<`+"http://"+r.Host+`/x?page=2>; rel="next", <http://`+r.Host+`/x?page=9>; rel="last"`)
		_, _ = w.Write([]byte(`{"v":1}`))
	})
	var remaining float64
	c.onRateLimit = func(v float64) { remaining = v }
	ctx := context.Background()

	c.BeginPoll()
	first, err := c.Get(ctx, "/x")
	if err != nil {
		t.Fatal(err)
	}
	c.EndPoll()
	if first.NotModified || !strings.HasSuffix(first.Next, "/x?page=2") {
		t.Fatalf("first = %+v", first)
	}

	c.BeginPoll()
	second, err := c.Get(ctx, "/x")
	if err != nil {
		t.Fatal(err)
	}
	c.EndPoll()
	if !second.NotModified || string(second.Body) != `{"v":1}` || second.Next != first.Next {
		t.Fatalf("second = %+v", second)
	}
	if gotINM[0] != "" || gotINM[1] != `W/"v1"` {
		t.Fatalf("If-None-Match = %q", gotINM)
	}
	if remaining != 4999 {
		t.Fatalf("rate limit remaining = %v", remaining)
	}
	// 3 requests: one token mint + two GETs, one of them a 304.
	if s := c.TakeStats(); s.Requests != 3 || s.NotModified != 1 {
		t.Fatalf("stats = %+v", s)
	}

	// An entry unused for a whole poll is evicted, so the next GET is
	// unconditional again.
	c.BeginPoll()
	c.EndPoll()
	if _, err := c.Get(ctx, "/x"); err != nil {
		t.Fatal(err)
	}
	if gotINM[2] != "" {
		t.Fatalf("evicted entry still sent If-None-Match %q", gotINM[2])
	}
}

func TestOffHostNextRejected(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	_, err := c.Get(context.Background(), "https://elsewhere.example/x")
	if err == nil || KindOf(err) != KindParse {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorKinds(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusForbidden, KindForbidden},
		{http.StatusTooManyRequests, KindForbidden},
		{http.StatusUnprocessableEntity, KindForbidden},
		{http.StatusNotFound, KindNotFound},
		{http.StatusGone, KindNotFound},
		{http.StatusInternalServerError, KindServer},
		{http.StatusBadGateway, KindServer},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
			})
			_, err := c.Get(context.Background(), "/x")
			if got := KindOf(err); got != tt.want {
				t.Fatalf("KindOf(%v) = %q, want %q", err, got, tt.want)
			}
			if !strings.Contains(err.Error(), "nope") {
				t.Fatalf("error lacks API message: %v", err)
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		c, err := New(7, 42, pkcs1PEM(t), Options{BaseURL: "http://127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Get(context.Background(), "/x")
		if KindOf(err) != KindNetwork {
			t.Fatalf("KindOf(%v) = %q", err, KindOf(err))
		}
	})
	t.Run("mint refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		c, err := New(7, 42, pkcs1PEM(t), Options{BaseURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Get(context.Background(), "/x")
		if KindOf(err) != KindAuth {
			t.Fatalf("KindOf(%v) = %q", err, KindOf(err))
		}
	})
}

func TestNextLink(t *testing.T) {
	tests := []struct{ in, want string }{
		{``, ``},
		{`<https://api.example/x?page=2>; rel="next", <https://api.example/x?page=5>; rel="last"`, `https://api.example/x?page=2`},
		{`<https://api.example/x?page=1>; rel="prev"`, ``},
		{`<https://api.example/x?page=3>;rel="next"`, `https://api.example/x?page=3`},
	}
	for _, tt := range tests {
		if got := nextLink(tt.in); got != tt.want {
			t.Errorf("nextLink(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
