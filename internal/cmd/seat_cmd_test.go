package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/taskmap"
)

// TestSeatCmdIsRegisteredUnderTheRoot: gt seat is a command of the tree, found
// by its name, so the ship subcommand has a parent to hang under.
func TestSeatCmdIsRegisteredUnderTheRoot(t *testing.T) {
	t.Parallel()
	got, _, err := rootCmd.Find([]string{"seat"})
	if err != nil {
		t.Fatalf("rootCmd.Find(seat) error = %v", err)
	}
	if got.Name() != "seat" || got.Parent() != rootCmd {
		t.Errorf("rootCmd.Find(seat) = %q under %v, want seat directly under the root", got.Name(), got.Parent())
	}
}

// TestSeatCmdBareRunIsAUsageError: a bare gt seat names its missing subcommand
// and exits 2, as every parent does (requireSubcommand), not help and exit 0.
func TestSeatCmdBareRunIsAUsageError(t *testing.T) {
	t.Parallel()
	err := seatCmd.RunE(seatCmd, nil)
	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("seatCmd.RunE(bare) = %v, want an ExitCodeError with code 2", err)
	}
	if !strings.Contains(err.Error(), "seat") {
		t.Errorf("seatCmd.RunE(bare) error = %q, want it to name the seat command", err)
	}
}

// TestSeatCmdHasItsTaskPlacement: the task map places gt seat in the seats
// group, so the map the command tree is checked against stays complete.
func TestSeatCmdHasItsTaskPlacement(t *testing.T) {
	t.Parallel()
	entry, ok := taskmap.Default().Lookup("seat")
	if !ok {
		t.Fatal("taskmap has no entry for seat")
	}
	if entry.Group != "seats" {
		t.Errorf("seat placed in group %q, want seats", entry.Group)
	}
	if !strings.HasPrefix(entry.Use, "Use this") {
		t.Errorf("seat placement use = %q, want a sentence that starts \"Use this\"", entry.Use)
	}
}
