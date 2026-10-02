package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/sling"
)

// TestIsSlingRefusal pins which sling failures stop being "the command was
// mistyped" and become "the command refused": the CLI drops cobra's usage block
// for exactly these, and a wrong answer either buries a refusal's reason in
// usage text or hides a real failure's help (gt-fudap).
func TestIsSlingRefusal(t *testing.T) {
	t.Parallel()

	_, blockedErr := sling.Blocked("/town", "gastown",
		func(string, string) (bool, error) { return true, nil },
		func(string, string) (bool, string) { return false, "" })

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"duplicate content", sling.DecideDuplicates("gt-x", []sling.DuplicateMatch{{
			Bead: sling.Duplicate{ID: "gt-y", Status: "open", IssueType: "task"}, SharedTests: []string{"TestFoo"},
		}}).Err(), true},
		{"pool full", &poolBackpressureError{Reason: "gastown pool at capacity"}, true},
		{"landing queue", &queueBackpressureError{Rig: "gastown", Ready: 3, Max: 1}, true},
		{"rig e-stopped", blockedErr, true},
		{"resling surviving work", &reslingRefusal{msg: dispatch.ReslingRefusalMarker + " gt-x: branch survives"}, true},
		{"a plain failure", errors.New("bead 'gt-x' not found"), false},
	}
	for _, tc := range cases {
		if got := isSlingRefusal(tc.err); got != tc.want {
			t.Errorf("%s: isSlingRefusal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSilenceUsageOnFailure pins the rule gt-thnbp set: cobra's usage block
// follows a sling failure only when the command was mistyped. A runtime failure
// (the store unreachable, the daemon restarting) and a refusal both print their
// one error line, so a caller that keeps only the last line reads the cause.
func TestSilenceUsageOnFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        error
		wantSilent bool
	}{
		{"nil", nil, false},
		{"runtime failure", errors.New("dial tcp 127.0.0.1:3307: connection refused"), true},
		{"wrapped runtime failure", fmt.Errorf("spawning polecat: %w", errors.New("daemon restarting")), true},
		{"refusal", &poolBackpressureError{Reason: "gastown pool at capacity"}, true},
		{"flag misuse", slingUsageErrorf("--branch and --pr are mutually exclusive"), false},
		{"wrapped flag misuse", fmt.Errorf("sling: %w", slingUsageErrorf("--crew requires a rig target")), false},
		{"merge flag misuse", &slingUsageError{err: errors.New("invalid --merge value")}, false},
	}
	for _, tc := range cases {
		cmd := &cobra.Command{Use: "sling"}
		silenceUsageOnFailure(cmd, tc.err)
		if cmd.SilenceUsage != tc.wantSilent {
			t.Errorf("%s: SilenceUsage = %v, want %v", tc.name, cmd.SilenceUsage, tc.wantSilent)
		}
	}

	// A nil command is a no-op, not a panic.
	silenceUsageOnFailure(nil, errors.New("boom"))
}

// TestSlingUsageErrorKeepsItsText pins that marking an error as misuse changes
// nothing the operator reads.
func TestSlingUsageErrorKeepsItsText(t *testing.T) {
	t.Parallel()

	err := slingUsageErrorf("--base-branch cannot be combined with %s", "--pr")
	if got, want := err.Error(), "--base-branch cannot be combined with --pr"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	inner := errors.New("inner")
	if !errors.Is(&slingUsageError{err: inner}, inner) {
		t.Error("slingUsageError does not unwrap to its cause")
	}
}
