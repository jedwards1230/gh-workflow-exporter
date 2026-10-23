package version

import (
	"os"
	"testing"
)

// TestMain primes ensureVCS with a non-empty Commit before any test runs.
// ensureVCS only populates Commit from debug.ReadBuildInfo once (sync.Once)
// and only when Commit is still empty at that point — priming it here keeps
// the table-driven cases below deterministic regardless of this module's
// actual VCS state.
func TestMain(m *testing.M) {
	Commit = "seed0000"
	Short()
	os.Exit(m.Run())
}

func TestShortAndInfo(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		commit      string
		wantShort   string
		wantInfo    string
		description string
	}{
		{
			name:      "version and commit set",
			version:   "1.2.3",
			commit:    "abcdef1",
			wantShort: "1.2.3",
			wantInfo:  "1.2.3 (abcdef1)",
		},
		{
			name:      "only commit set",
			version:   "",
			commit:    "abcdef1",
			wantShort: "abcdef1",
			wantInfo:  "dev (abcdef1)",
		},
		{
			name:      "neither set",
			version:   "",
			commit:    "",
			wantShort: "dev",
			wantInfo:  "dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origVersion, origCommit := Version, Commit
			defer func() { Version, Commit = origVersion, origCommit }()

			Version, Commit = tt.version, tt.commit

			if got := Short(); got != tt.wantShort {
				t.Errorf("Short() = %q, want %q", got, tt.wantShort)
			}
			if got := Info(); got != tt.wantInfo {
				t.Errorf("Info() = %q, want %q", got, tt.wantInfo)
			}
		})
	}
}
