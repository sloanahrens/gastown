package holdclaim

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestKubectlRefusesAnEmptyCommand drives the refusal Kubectl makes before it
// starts a process: an empty argv names nothing to run.
func TestKubectlRefusesAnEmptyCommand(t *testing.T) {
	t.Parallel()
	if _, err := Kubectl(context.Background(), nil, nil); err == nil {
		t.Fatal("Kubectl with no argv returned nil, want a refusal")
	}
}

// TestKubectlNamesTheProgramThatFailsAndNotItsOutput drives the failure branch
// with a program that does not exist, so no process runs: the error names the
// program and carries no captured output.
func TestKubectlNamesTheProgramThatFailsAndNotItsOutput(t *testing.T) {
	t.Parallel()
	const program = "holdclaim-no-such-program"
	_, err := Kubectl(context.Background(), []string{program, "arg"}, nil)
	if err == nil {
		t.Fatal("Kubectl of a missing program returned nil, want an error")
	}
	if !strings.HasPrefix(err.Error(), program+": ") {
		t.Fatalf("error = %q, want it to name the program %q", err, program)
	}
}

func TestNewRefusesAnEmptyTownRoot(t *testing.T) {
	t.Parallel()
	fake := newFakePod()
	if _, err := New("", testTarget, fake.exec); err == nil {
		t.Fatal("New with an empty town root returned nil, want a refusal")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("pod calls = %d, want none", len(fake.calls))
	}
}

// TestPodPathRefusesALocalPathThatCannotBeRelativised drives the Rel error:
// a relative town root cannot be compared with an absolute sentinel path.
func TestPodPathRefusesALocalPathThatCannotBeRelativised(t *testing.T) {
	t.Parallel()
	fake := newFakePod()
	w, err := New("relative-town", testTarget, fake.exec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	abs := filepath.Join(t.TempDir(), "seat-refill.hold")
	if err := w.Set(abs, nil); err == nil {
		t.Fatal("Set of an absolute path under a relative town root returned nil, want a refusal")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("pod calls = %d, want none", len(fake.calls))
	}
}
