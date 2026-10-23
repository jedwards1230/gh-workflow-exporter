package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `
poll_interval: 2m
default_grace_factor: 2
include:
  - repo: octo-org/deploy
    workflow: apply.yml
exclude:
  - repo: octo-org/tools
    workflow: codeql.yml
overrides:
  - repo: octo-org/nightly
    workflow: build.yml
    branch: release
    grace_factor: 3
  - repo: octo-org/nightly
    workflow: lint.yml
    branch: develop
`

func TestParse(t *testing.T) {
	cfg, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Interval(); got != 2*time.Minute {
		t.Errorf("Interval = %v, want 2m", got)
	}
	tests := []struct {
		name               string
		repo, wf           string
		included, excluded bool
		branch             string
		grace              float64
	}{
		{name: "included", repo: "octo-org/deploy", wf: "apply.yml", included: true, branch: "main", grace: 2},
		{name: "included case-insensitive", repo: "Octo-Org/Deploy", wf: "APPLY.yml", included: true, branch: "main", grace: 2},
		{name: "excluded", repo: "octo-org/tools", wf: "codeql.yml", excluded: true, branch: "main", grace: 2},
		{name: "override branch and grace", repo: "octo-org/nightly", wf: "build.yml", branch: "release", grace: 3},
		{name: "override branch only", repo: "octo-org/nightly", wf: "lint.yml", branch: "develop", grace: 2},
		{name: "no override", repo: "octo-org/nightly", wf: "other.yml", branch: "main", grace: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.Included(tt.repo, tt.wf); got != tt.included {
				t.Errorf("Included = %v, want %v", got, tt.included)
			}
			if got := cfg.Excluded(tt.repo, tt.wf); got != tt.excluded {
				t.Errorf("Excluded = %v, want %v", got, tt.excluded)
			}
			if got := cfg.Branch(tt.repo, tt.wf, "main"); got != tt.branch {
				t.Errorf("Branch = %q, want %q", got, tt.branch)
			}
			if got := cfg.GraceFactor(tt.repo, tt.wf); got != tt.grace {
				t.Errorf("GraceFactor = %v, want %v", got, tt.grace)
			}
		})
	}
}

func TestParseDefaults(t *testing.T) {
	for _, doc := range []string{"", "include: []\n"} {
		cfg, err := Parse(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("Parse(%q): %v", doc, err)
		}
		if cfg.Interval() != DefaultPollInterval || cfg.DefaultGraceFactor != DefaultGraceFactor {
			t.Errorf("Parse(%q) defaults = %v/%v", doc, cfg.Interval(), cfg.DefaultGraceFactor)
		}
	}
	if d := Default(); d.Interval() != DefaultPollInterval || d.GraceFactor("a/b", "c.yml") != DefaultGraceFactor {
		t.Errorf("Default() = %+v", d)
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name, doc, wantErr string
	}{
		{"negative interval", "poll_interval: -1m", "poll_interval must be at least 1m"},
		{"interval below minimum", "poll_interval: 30s", "poll_interval must be at least 1m"},
		{"bad duration", "poll_interval: soon", "duration"},
		{"negative grace", "default_grace_factor: -1", "default_grace_factor must be positive"},
		{"unknown key", "poll_intervall: 5m", "not found"},
		{"repo without owner", "include:\n  - repo: nightly\n    workflow: a.yml", "owner/name"},
		{"repo with extra slash", "exclude:\n  - repo: a/b/c\n    workflow: a.yml", "owner/name"},
		{"workflow missing", "include:\n  - repo: a/b", "workflow must be set"},
		{"workflow is a path", "include:\n  - repo: a/b\n    workflow: .github/workflows/a.yml", "basename"},
		{"override grace zero", "overrides:\n  - repo: a/b\n    workflow: a.yml\n    grace_factor: 0", "grace_factor must be positive"},
		{"duplicate override", "overrides:\n  - repo: a/b\n    workflow: a.yml\n  - repo: A/B\n    workflow: a.yml", "duplicate override"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Parse error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("Load(missing) succeeded")
	}
}

func TestLoadCredentials(t *testing.T) {
	const pem = "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----"
	dir := t.TempDir()
	for name, v := range map[string]string{FileAppID: "123\n", FileInstallationID: " 456 ", FilePrivateKey: pem + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	tests := []struct {
		name     string
		dir      string
		env      map[string]string
		wantApp  int64
		wantInst int64
		wantKey  string
		wantErr  string
	}{
		{name: "files only", dir: dir, wantApp: 123, wantInst: 456, wantKey: pem + "\n"},
		{
			name: "env wins over files", dir: dir,
			env:     map[string]string{EnvAppID: "9", EnvPrivateKey: strings.ReplaceAll(pem, "\n", `\n`)},
			wantApp: 9, wantInst: 456, wantKey: pem + "\n",
		},
		{
			name: "env only", dir: "",
			env:     map[string]string{EnvAppID: "1", EnvInstallationID: "2", EnvPrivateKey: pem},
			wantApp: 1, wantInst: 2, wantKey: pem + "\n",
		},
		{name: "missing everything", dir: t.TempDir(), wantErr: "reading"},
		{
			name: "non-numeric id", dir: dir,
			env:     map[string]string{EnvAppID: "abc"},
			wantErr: "app_id must be a positive integer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := LoadCredentials(tt.dir, env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadCredentials: %v", err)
			}
			if c.AppID != tt.wantApp || c.InstallationID != tt.wantInst || string(c.PrivateKeyPEM) != tt.wantKey {
				t.Fatalf("got app=%d inst=%d key=%q", c.AppID, c.InstallationID, c.PrivateKeyPEM)
			}
		})
	}
}
