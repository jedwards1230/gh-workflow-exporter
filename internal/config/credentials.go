package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Environment variables that carry the GitHub App credentials directly. Each
// one, when set and non-empty, wins over the matching file in the secret dir.
const (
	EnvAppID          = "GHWE_APP_ID"
	EnvInstallationID = "GHWE_INSTALLATION_ID"
	EnvPrivateKey     = "GHWE_PRIVATE_KEY"
)

// File names read from the secret directory.
const (
	FileAppID          = "app_id"
	FileInstallationID = "installation_id"
	FilePrivateKey     = "private_key"
)

// Credentials identify the GitHub App installation the exporter acts as.
type Credentials struct {
	AppID          int64
	InstallationID int64
	// PrivateKeyPEM is the App's private key. Never log it.
	PrivateKeyPEM []byte
}

// LoadCredentials resolves the App credentials from the environment (via
// getenv) and, for any value not set there, from files in secretDir.
func LoadCredentials(secretDir string, getenv func(string) string) (*Credentials, error) {
	read := func(env, file string) (string, error) {
		if v := getenv(env); v != "" {
			return v, nil
		}
		if secretDir == "" {
			return "", fmt.Errorf("%s not set and no secret dir configured", env)
		}
		p := filepath.Join(secretDir, file)
		b, err := os.ReadFile(p) //nolint:gosec // operator-supplied secret dir
		if err != nil {
			return "", fmt.Errorf("%s not set and reading %s: %w", env, p, err)
		}
		return string(b), nil
	}

	var errs []error
	appID, err := read(EnvAppID, FileAppID)
	if err != nil {
		errs = append(errs, err)
	}
	instID, err := read(EnvInstallationID, FileInstallationID)
	if err != nil {
		errs = append(errs, err)
	}
	key, err := read(EnvPrivateKey, FilePrivateKey)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	c := &Credentials{}
	if c.AppID, err = parseID("app_id", appID); err != nil {
		errs = append(errs, err)
	}
	if c.InstallationID, err = parseID("installation_id", instID); err != nil {
		errs = append(errs, err)
	}
	key = strings.TrimSpace(key)
	// A PEM squeezed into a single-line env var often carries literal "\n".
	if !strings.Contains(key, "\n") && strings.Contains(key, `\n`) {
		key = strings.ReplaceAll(key, `\n`, "\n")
	}
	if key == "" {
		errs = append(errs, errors.New("private_key is empty"))
	}
	c.PrivateKeyPEM = []byte(key + "\n")
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func parseID(name, v string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}
