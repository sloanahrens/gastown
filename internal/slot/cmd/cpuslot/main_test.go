package main

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// recorder is a host whose every collaborator records the call it received, so
// a case asserts which one runFrom chose.
type recorder struct {
	getwd  func() (string, error)
	find   func(string) (string, error)
	ran    [][]string
	queued [][]string
	warned strings.Builder
}

func (r *recorder) host() host {
	return host{
		getwd: r.getwd,
		find:  r.find,
		run: func(args []string) (int, error) {
			r.ran = append(r.ran, args)
			return 42, nil
		},
		queued: func(townRoot, role string, args []string) (int, error) {
			r.queued = append(r.queued, append([]string{townRoot, role}, args...))
			return 43, nil
		},
		warn: &r.warned,
	}
}

// runErr calls runFrom with no working directory and a find that must not be
// reached, so each refusal case reads as the input it supplies.
func runErr(t *testing.T, args []string, getwd func() (string, error)) string {
	t.Helper()
	r := &recorder{getwd: getwd, find: notReached(t)}
	_, err := runFrom(args, r.host())
	if err == nil {
		t.Fatal("runFrom returned no error, want a refusal")
	}
	return err.Error()
}

// TestCPUClassWrapperRefusesNoCommand: an invocation naming no command is
// refused before any working directory or town is read.
// Mutation: drop the length check, and the refusal becomes a start of "".
func TestCPUClassWrapperRefusesNoCommand(t *testing.T) {
	t.Parallel()
	r := &recorder{getwd: notRead(t), find: notReached(t)}
	if _, err := runFrom(nil, r.host()); err == nil || !strings.Contains(err.Error(), "no command given") {
		t.Fatalf("error = %v, want it to name the missing command", err)
	}
}

// TestCPUClassWrapperRefusesUnreadableWorkingDirectory: a working directory
// that cannot be read refuses rather than running the command from a tree it
// cannot name.
// Mutation: run the command unslotted on this error, and the refusal becomes a
// build that never took a slot.
func TestCPUClassWrapperRefusesUnreadableWorkingDirectory(t *testing.T) {
	t.Parallel()
	getwd := func() (string, error) { return "", errors.New("cwd is gone") }
	got := runErr(t, []string{"go", "build"}, getwd)
	if !strings.Contains(got, "reading the working directory") {
		t.Fatalf("error = %q, want it to name the working directory", got)
	}
}

// TestCPUClassWrapperRunsUnslottedWhenTheTownLookupFails: a lookup that errors
// is not a crew checkout, so the command runs unchanged rather than failing a
// build over the class's own books. The notice names the failed lookup.
// Mutation: refuse on this error, and the landing worker's gate and CI fail
// whenever the tree cannot be walked.
func TestCPUClassWrapperRunsUnslottedWhenTheTownLookupFails(t *testing.T) {
	t.Parallel()
	r := &recorder{
		getwd: func() (string, error) { return "/town/gastown/crew/olive", nil },
		find:  func(string) (string, error) { return "", errors.New("town.json unreadable") },
	}
	code, err := runFrom([]string{"go", "build", "./..."}, r.host())
	if err != nil || code != 42 {
		t.Fatalf("runFrom = (%d, %v), want the unchanged command's (42, nil)", code, err)
	}
	if want := [][]string{{"go", "build", "./..."}}; !slices.EqualFunc(r.ran, want, slices.Equal) {
		t.Fatalf("commands run unchanged = %v, want %v", r.ran, want)
	}
	if len(r.queued) != 0 {
		t.Fatalf("a command queued on a failed lookup: %v", r.queued)
	}
	for _, want := range []string{"town.json unreadable", "without a CPU-class slot"} {
		if !strings.Contains(r.warned.String(), want) {
			t.Errorf("notice = %q, want it to name %q", r.warned.String(), want)
		}
	}
}

// TestCPUClassWrapperRunsUnslottedOutsideACrewCheckout: a tree that is not a
// crew worktree — no town, a polecat checkout, the crew directory itself — runs
// the command unchanged, so polecats and the landing gate keep their path.
// Mutation: queue anything inside a town, and a polecat's build waits behind
// the crew class.
func TestCPUClassWrapperRunsUnslottedOutsideACrewCheckout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := []struct {
		name string
		cwd  string
		root string
	}{
		{name: "no town", cwd: t.TempDir()},
		{name: "polecat checkout", cwd: town + "/gastown/polecats/dag/gastown", root: town},
		{name: "the crew directory itself", cwd: town + "/gastown/crew", root: town},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{
				getwd: func() (string, error) { return tc.cwd, nil },
				find:  func(string) (string, error) { return tc.root, nil },
			}
			code, err := runFrom([]string{"go", "vet", "./..."}, r.host())
			if err != nil || code != 42 {
				t.Fatalf("runFrom = (%d, %v), want the unchanged command's (42, nil)", code, err)
			}
			if len(r.queued) != 0 {
				t.Fatalf("a command outside a crew checkout queued: %v", r.queued)
			}
			if len(r.ran) != 1 {
				t.Fatalf("commands run unchanged = %v, want exactly one", r.ran)
			}
		})
	}
}

// TestCPUClassWrapperQueuesFromACrewCheckout: a crew checkout's command runs
// under the class, tagged with the crew role.
// Mutation: run the command unslotted in a crew checkout, and a burst of crew
// builds piles onto the host beside its neighbours.
func TestCPUClassWrapperQueuesFromACrewCheckout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cwd := town + "/gastown/crew/olive/internal/slot"
	r := &recorder{
		getwd: func() (string, error) { return cwd, nil },
		find:  func(string) (string, error) { return town, nil },
	}
	code, err := runFrom([]string{"GT_TEST_DOCKER=0", "go", "test", "./..."}, r.host())
	if err != nil || code != 43 {
		t.Fatalf("runFrom = (%d, %v), want the queued command's (43, nil)", code, err)
	}
	want := [][]string{{town, "gastown/crew/olive", "GT_TEST_DOCKER=0", "go", "test", "./..."}}
	if !slices.EqualFunc(r.queued, want, slices.Equal) {
		t.Fatalf("queued = %v, want %v", r.queued, want)
	}
	if len(r.ran) != 0 {
		t.Fatalf("a crew checkout's command ran unslotted: %v", r.ran)
	}
}

// notRead is the working directory a case must not read.
func notRead(t *testing.T) func() (string, error) {
	t.Helper()
	return func() (string, error) {
		t.Error("the case read the working directory it must not reach")
		return "", nil
	}
}

// notReached is the town lookup a case must not run; reaching it fails the
// test rather than letting the case start its command.
func notReached(t *testing.T) func(string) (string, error) {
	t.Helper()
	return func(string) (string, error) {
		t.Error("the case read a collaborator it must not reach")
		return "", nil
	}
}
