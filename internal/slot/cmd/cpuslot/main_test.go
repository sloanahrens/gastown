package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// runErr calls runFrom with an injected working directory and town lookup and
// returns its error text, so each case reads as the input it supplies.
func runErr(t *testing.T, args []string, getwd func() (string, error), find func(string) (string, error)) string {
	t.Helper()
	_, err := runFrom(time.Second, "", args, getwd, find)
	if err == nil {
		t.Fatal("runFrom returned no error, want a refusal")
	}
	return err.Error()
}

// TestRunRefusesNoCommand: an invocation naming no command is refused before
// any working directory or town is read.
// Mutation: drop the length check, and the refusal becomes a start of "".
func TestRunRefusesNoCommand(t *testing.T) {
	t.Parallel()
	if got := runErr(t, nil, notRead(t), notReached(t)); !strings.Contains(got, "no command given") {
		t.Fatalf("error = %q, want it to name the missing command", got)
	}
}

// TestRunRefusesUnreadableWorkingDirectory: a working directory that cannot be
// read refuses rather than running the command from an unknown tree.
// Mutation: run the command unslotted on this error, and the refusal becomes a
// build that never took a slot.
func TestRunRefusesUnreadableWorkingDirectory(t *testing.T) {
	t.Parallel()
	getwd := func() (string, error) { return "", errors.New("cwd is gone") }
	if got := runErr(t, []string{"go", "build"}, getwd, notReached(t)); !strings.Contains(got, "reading the working directory") {
		t.Fatalf("error = %q, want it to name the working directory", got)
	}
}

// TestRunRefusesFailedTownLookup: a town lookup that errors refuses rather than
// running the command unqueued, since an unread tree might be a crew checkout.
// Mutation: run the command unslotted on this error, and a crew build whose
// lookup failed loads the host outside the class.
func TestRunRefusesFailedTownLookup(t *testing.T) {
	t.Parallel()
	getwd := func() (string, error) { return "/town/gastown/crew/olive", nil }
	find := func(string) (string, error) { return "", errors.New("town.json unreadable") }
	if got := runErr(t, []string{"go", "build"}, getwd, find); !strings.Contains(got, "town.json unreadable") {
		t.Fatalf("error = %q, want it to name the failed lookup", got)
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
