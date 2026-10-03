package beads

import (
	"slices"
	"strings"
	"testing"
)

func TestHandoffBeadTitle(t *testing.T) {
	tests := []struct {
		role string
		want string
	}{
		{"mayor", "mayor Handoff"},
		{"deacon", "deacon Handoff"},
		{"gastown/witness", "gastown/witness Handoff"},
		{"gastown/crew/joe", "gastown/crew/joe Handoff"},
		{"", " Handoff"},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			got := HandoffBeadTitle(tt.role)
			if got != tt.want {
				t.Errorf("HandoffBeadTitle(%q) = %q, want %q", tt.role, got, tt.want)
			}
		})
	}
}

func TestStatusConstants(t *testing.T) {
	// Verify the status constants haven't changed (these are used in protocol)
	if StatusPinned != "pinned" {
		t.Errorf("StatusPinned = %q, want %q", StatusPinned, "pinned")
	}
	if StatusHooked != "hooked" {
		t.Errorf("StatusHooked = %q, want %q", StatusHooked, "hooked")
	}
}

func TestCurrentTimestamp(t *testing.T) {
	ts := currentTimestamp()
	if ts == "" {
		t.Fatal("currentTimestamp() returned empty string")
	}
	// Should be RFC3339 format
	if len(ts) < 20 {
		t.Errorf("timestamp too short: %q (expected RFC3339)", ts)
	}
	// Should contain T separator and Z suffix (UTC)
	found := false
	for _, c := range ts {
		if c == 'T' {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("timestamp missing T separator: %q", ts)
	}
}

func TestClearMailResultZeroValues(t *testing.T) {
	// Verify zero-value struct is safe to use
	result := &ClearMailResult{}
	if result.Closed != 0 || result.Cleared != 0 {
		t.Errorf("expected zero values, got Closed=%d Cleared=%d", result.Closed, result.Cleared)
	}
}

// TestCloseStaleHookedMailBeads pins the bd conversation the sweep runs
// (GH#3859). Which beads are stale is bd's answer, not gastown's:
// CloseStaleHookedMailBeads asks for one agent's hooked gt:message beads and
// closes exactly the page it gets back, so the filter — not a client-side
// scan of every hooked bead — is what keeps another agent's mail, and
// gt:task beads, out of the sweep.
func TestCloseStaleHookedMailBeads(t *testing.T) {
	t.Parallel()

	// page is the list answer; every other call (a close) succeeds silently.
	answer := func(page string) func([]string) reply {
		return func(args []string) reply {
			if len(args) > 0 && args[0] == "list" {
				return reply{stdout: page}
			}
			return reply{}
		}
	}

	t.Run("asks bd for this agent's hooked mail beads", func(t *testing.T) {
		t.Parallel()
		r := newRecorder(answer(`[]`))
		b := newRecordedBeads(t.TempDir(), r)

		n, err := b.CloseStaleHookedMailBeads("gastown/mayor")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 0 {
			t.Errorf("want 0 closed, got %d", n)
		}
		want := "list --json --status=hooked --label=gt:message --assignee=gastown/mayor --limit=0 --flat"
		if got := r.argvs(); len(got) != 1 || got[0] != want {
			t.Errorf("bd calls = %q, want exactly [%q]", got, want)
		}
	})

	t.Run("closes every bead the page named", func(t *testing.T) {
		t.Parallel()
		r := newRecorder(answer(`[{"id":"test-hm-1"},{"id":"test-hm-2"}]`))
		b := newRecordedBeads(t.TempDir(), r)

		n, err := b.CloseStaleHookedMailBeads("gastown/mayor")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Errorf("want 2 closed, got %d", n)
		}
		closeArgs := closeCall(t, r)
		for _, want := range []string{
			"test-hm-1",
			"test-hm-2",
			"--force",
			"--reason=handoff: superseded by new session",
		} {
			if !slices.Contains(closeArgs, want) {
				t.Errorf("close argv %q lacks %q", strings.Join(closeArgs, " "), want)
			}
		}
	})

	t.Run("closes nothing when the page is empty", func(t *testing.T) {
		t.Parallel()
		r := newRecorder(answer(`[]`))
		b := newRecordedBeads(t.TempDir(), r)

		if _, err := b.CloseStaleHookedMailBeads("gastown/mayor"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, args := range r.calls() {
			if len(args.args) > 0 && args.args[0] == "close" {
				t.Fatalf("an empty page still ran %q", strings.Join(args.args, " "))
			}
		}
	})
}

// closeCall returns the argv of the one close call r saw, failing t if the
// number of close calls is not one.
func closeCall(t *testing.T, r *recorder) []string {
	t.Helper()
	var closes [][]string
	for _, call := range r.calls() {
		if len(call.args) > 0 && call.args[0] == "close" {
			closes = append(closes, call.args)
		}
	}
	if len(closes) != 1 {
		t.Fatalf("bd close calls = %d, want 1 (argvs: %q)", len(closes), r.argvs())
	}
	return closes[0]
}
