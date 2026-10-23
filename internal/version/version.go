// Package version provides build-time version stamping for gh-workflow-exporter.
package version

import (
	"runtime/debug"
	"sync"
)

// Build-time variables set via -ldflags -X.
var (
	Version string // Semver tag, e.g. "0.1.0"
	Commit  string // Git short SHA, e.g. "a1b2c3d"
	Date    string // ISO 8601 build timestamp
)

var once sync.Once

// ensureVCS populates Commit from debug.ReadBuildInfo if it was not injected
// via ldflags. Appends "-dirty" when the working tree was modified.
func ensureVCS() {
	once.Do(func() {
		if Commit != "" {
			return
		}
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		var dirty bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) >= 7 {
					Commit = s.Value[:7]
				} else {
					Commit = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if dirty && Commit != "" {
			Commit += "-dirty"
		}
	})
}

// Short returns the version string, commit hash, or "dev".
func Short() string {
	ensureVCS()
	if Version != "" {
		return Version
	}
	if Commit != "" {
		return Commit
	}
	return "dev"
}

// Info returns a human-readable "VERSION (COMMIT)" string.
func Info() string {
	ensureVCS()
	ver := Version
	if ver == "" {
		ver = "dev"
	}
	if Commit != "" {
		return ver + " (" + Commit + ")"
	}
	return ver
}
