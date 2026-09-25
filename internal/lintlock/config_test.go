package lintlock

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRepoConfigKeepsContentionVisible is the regression the om review of the
// lint-lock work asked for (gt-kqwu): this repo's .golangci.yml must not set
// run.allow-serial-runners.
//
// The setting makes golangci-lint take the module lock on a deadline-less
// context, so a contended lint blocks silently rather than exiting in 5s with
// Marker, and its own run.timeout clock starts only after the lock is held, so
// the blocked lint prints TimeoutSentinel even less. Both signals gone, Retry
// never sees Unfinished for this rig's lint command, and a gate verdict falls
// through to a guess about what the lint was doing. The default fail-fast keeps
// the evidence; the waiting belongs to the callers, which retry on it.
func TestRepoConfigKeepsContentionVisible(t *testing.T) {
	t.Parallel()

	path := repoFile(t, ".golangci.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var cfg struct {
		Run struct {
			AllowSerialRunners *bool `yaml:"allow-serial-runners"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	if cfg.Run.AllowSerialRunners != nil && *cfg.Run.AllowSerialRunners {
		t.Errorf("%s sets run.allow-serial-runners, which makes a contended golangci-lint block on the module lock instead of exiting in 5s with %q. That line is the evidence Contended and Unfinished read, so with it suppressed a gate can only guess whether the lock or the lint itself spent the budget. Remove the setting: the gate and gt done wait the lock out and retry (gt-kqwu).", path, Marker)
	}
}

// repoFile returns name's path in the checkout this package's test runs in, so
// the test finds the repo's own files from the package directory `go test`
// gives it (the repo root via `go test ./internal/lintlock/...`).
func repoFile(t *testing.T, name string) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, name)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no %s at or above %s: the test cannot see the checkout it is gating", name, dir)
		}
		dir = parent
	}
}
