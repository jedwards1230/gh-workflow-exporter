package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestParseOptionsPrecedence(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		env          map[string]string
		wantConfig   string
		wantListen   string
		wantSecret   string
		wantLevel    slog.Level
		wantExplicit bool
		wantErr      bool
	}{
		{name: "defaults", wantConfig: defaultConfigPath, wantListen: defaultListen, wantSecret: defaultSecretDir, wantLevel: slog.LevelInfo},
		{
			name:       "env over defaults",
			env:        map[string]string{"GHWE_CONFIG": "/env.yaml", "GHWE_LISTEN": ":1", "GHWE_SECRET_DIR": "/env", "GHWE_LOG_LEVEL": "debug"},
			wantConfig: "/env.yaml", wantListen: ":1", wantSecret: "/env", wantLevel: slog.LevelDebug, wantExplicit: true,
		},
		{
			name:       "flags over env",
			args:       []string{"--config", "/flag.yaml", "--listen", ":2", "--secret-dir", "/flag", "--log-level", "warn"},
			env:        map[string]string{"GHWE_CONFIG": "/env.yaml", "GHWE_LISTEN": ":1", "GHWE_SECRET_DIR": "/env", "GHWE_LOG_LEVEL": "debug"},
			wantConfig: "/flag.yaml", wantListen: ":2", wantSecret: "/flag", wantLevel: slog.LevelWarn, wantExplicit: true,
		},
		{name: "bad level", args: []string{"--log-level", "loud"}, wantErr: true},
		{name: "stray argument", args: []string{"extra"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := parseOptions(tt.args, func(k string) string { return tt.env[k] }, io.Discard)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if o.configPath != tt.wantConfig || o.listen != tt.wantListen || o.secretDir != tt.wantSecret ||
				o.logLevel != tt.wantLevel || o.configExplicit != tt.wantExplicit {
				t.Fatalf("got %+v", o)
			}
		})
	}
}

func TestLoadConfigMissing(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, err := loadConfig(&options{configPath: missing}, log); err != nil {
		t.Fatalf("missing default config should fall back to defaults: %v", err)
	}
	if _, err := loadConfig(&options{configPath: missing, configExplicit: true}, log); err == nil {
		t.Fatal("missing explicit config should fail")
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("poll_interval: -1s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(&options{configPath: bad}, log); err == nil {
		t.Fatal("invalid config should fail even at the default path")
	}
}

func TestEndpoints(t *testing.T) {
	var ready atomic.Bool
	srv := httptest.NewServer(newMux(prometheus.NewRegistry(), ready.Load))
	defer srv.Close()
	status := func(path string) int {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if s := status("/healthz"); s != 200 {
		t.Errorf("/healthz = %d", s)
	}
	if s := status("/readyz"); s != 503 {
		t.Errorf("/readyz before ready = %d", s)
	}
	ready.Store(true)
	if s := status("/readyz"); s != 200 {
		t.Errorf("/readyz after ready = %d", s)
	}
	if s := status("/metrics"); s != 200 {
		t.Errorf("/metrics = %d", s)
	}
}
