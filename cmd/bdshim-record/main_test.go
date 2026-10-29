package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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
	args := []string{"-fixture", t.TempDir(), "-scratch", ""}
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Errorf("run with an empty -scratch = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "make scratch directory") {
		t.Errorf("stderr = %q, want the scratch directory failure reported", stderr.String())
	}
}

// TestRunRefusesAScratchInsideTheFixture: a scratch directory the fixture tree contains would hold
// copies the next copy walks, so the command refuses it before it makes anything.
func TestRunRefusesAScratchInsideTheFixture(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	tests := []struct {
		name    string
		scratch string
	}{
		{"scratch is the fixture", fixture},
		{"scratch under the fixture", filepath.Join(fixture, "scratch")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := fileNames(t, fixture)
			var stdout, stderr bytes.Buffer
			args := []string{"-fixture", fixture, "-scratch", tc.scratch}
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Errorf("run with -scratch %s = %d, want 2", tc.name, code)
			}
			if !strings.Contains(stderr.String(), "inside the fixture") {
				t.Errorf("stderr = %q, want the refusal reported", stderr.String())
			}
			if after := fileNames(t, fixture); !reflect.DeepEqual(before, after) {
				t.Errorf("fixture holds %v after the refusal, want it untouched at %v", after, before)
			}
		})
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
