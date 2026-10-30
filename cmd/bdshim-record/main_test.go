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
	var stdout, stderr bytes.Buffer
	args := []string{"-fixture", writeFixture(t, fixtureIDs...), "-scratch", ""}
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Errorf("run with an empty -scratch = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "make scratch directory") {
		t.Errorf("stderr = %q, want the scratch directory failure reported", stderr.String())
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

// TestRunRefusesAFixtureFileThatIsNotThere: a -fixture that names no file stops the run before any
// database is made.
func TestRunRefusesAFixtureFileThatIsNotThere(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	if code := run([]string{"-fixture", missing, "-scratch", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Errorf("run with a missing fixture file = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "open the fixture") {
		t.Errorf("stderr = %q, want the fixture failure reported", stderr.String())
	}
}

// TestRunRefusesAFixtureMissingABead: a fixture file that lacks a bead the table names stops the
// run before any database is made, naming the bead.
func TestRunRefusesAFixtureMissingABead(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	fixture := writeFixture(t, fixtureID, fixtureParent)
	if code := run([]string{"-fixture", fixture, "-scratch", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Errorf("run over a fixture missing %s = %d, want 1", fixtureDepID, code)
	}
	if !strings.Contains(stderr.String(), fixtureDepID) {
		t.Errorf("stderr = %q, want the missing bead named", stderr.String())
	}
}
