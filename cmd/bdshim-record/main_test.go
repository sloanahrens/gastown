package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// The recorder runs bd only through an injected Runner, so the hermetic harness keeps the test
// binary off the live town's state, as every package that can reach it must.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// TestRunRefusesAScratchDirectoryItCannotMake: fixture copies need a scratch directory, and when it
// cannot be made the command stops with the failure reported, before bd runs.
func TestRunRefusesAScratchDirectoryItCannotMake(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("a file\n"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"-fixture", t.TempDir(), "-scratch", filepath.Join(blocker, "scratch")}
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Errorf("run with an unmakeable -scratch = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a directory") {
		t.Errorf("stderr = %q, want the mkdir failure reported", stderr.String())
	}
}

// TestRunRefusesAMissingFixture: without -fixture there is nothing for bd to run in, so the command
// stops before it runs anything.
func TestRunRefusesAMissingFixture(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Errorf("run with no -fixture = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-fixture is required") {
		t.Errorf("stderr = %q, want the missing -fixture named", stderr.String())
	}
}
