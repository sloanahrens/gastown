package done

import (
	"bytes"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// A bead whose notes already carry the landing record has nothing left to
// preserve, so gt done pushes nothing for it on either the COMPLETED or the
// ESCALATED exit; the lease alone cannot tell origin's tip is the landed work
// (gt-ijimsb). A bead without the record pushes once, as before.
const alreadyLandedNotes = "LANDING RECORD\nlanded_commit: 5ada1a68\npatch_id: p1\ntarget: main"

// landedLine is the one line a skipped push prints.
const landedLine = "gt done: bd-source already landed at 5ada1a68; nothing pushed"

// runSubmitExit drives one exit against the harness bead: a COMPLETED submit,
// or an ESCALATED exit's sandbox push. It returns the stdout and the warnings.
func runSubmitExit(t *testing.T, h *submitHarness, exit string) (string, string) {
	t.Helper()
	out := new(bytes.Buffer)
	warn := new(bytes.Buffer)
	h.r.out = out
	switch exit {
	case "completed":
		if err := h.submit(); err != nil {
			t.Fatalf("submit: %v", err)
		}
	case "escalated":
		h.r.sessionBackend = backendOpenShell
		h.r.cleanupStatus = "unpushed"
		pushSandboxBranchBeforeDeferralTo(warn, h.r)
	default:
		t.Fatalf("unknown exit %q", exit)
	}
	return out.String(), warn.String()
}

// TestDoneSkipsThePushWhenTheBeadAlreadyLanded: with a landed_commit in the
// bead's notes, a COMPLETED and an ESCALATED run each record zero pushes and
// print the already-landed line. Origin keeps its tip.
func TestDoneSkipsThePushWhenTheBeadAlreadyLanded(t *testing.T) {
	t.Parallel()
	for _, exit := range []string{"completed", "escalated"} {
		t.Run(exit, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			h.repo.origin[doneTestBranch] = "landed-tip"
			h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusHooked), Notes: alreadyLandedNotes})

			out, warn := runSubmitExit(t, h, exit)

			if len(h.repo.pushes) != 0 {
				t.Errorf("pushes = %v, want none for a landed bead", h.repo.pushes)
			}
			if got := h.repo.origin[doneTestBranch]; got != "landed-tip" {
				t.Errorf("origin branch = %q, want the landed tip kept", got)
			}
			if !strings.Contains(out, landedLine) {
				t.Errorf("stdout %q does not carry the line %q", out, landedLine)
			}
			if warn != "" {
				t.Errorf("unexpected warning: %s", warn)
			}
		})
	}
}

// TestDonePushesWhenTheBeadHasNoLandedCommit: the same runs without the
// landing record push once, and print no already-landed line.
func TestDonePushesWhenTheBeadHasNoLandedCommit(t *testing.T) {
	t.Parallel()
	for _, exit := range []string{"completed", "escalated"} {
		t.Run(exit, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			h.repo.origin[doneTestBranch] = "old-tip"
			h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusHooked)})

			out, _ := runSubmitExit(t, h, exit)

			if len(h.repo.pushes) != 1 {
				t.Errorf("pushes = %v, want one push", h.repo.pushes)
			}
			if got := h.repo.origin[doneTestBranch]; got != h.repo.head {
				t.Errorf("origin branch = %q, want the pushed head %q", got, h.repo.head)
			}
			if strings.Contains(out, "already landed") {
				t.Errorf("stdout %q names a landing the bead does not record", out)
			}
		})
	}
}

// TestAlreadyLandedAt: a LANDING RECORD the landing worker wrote counts,
// wherever it sits in the notes. An empty landed_commit value, and a
// resubmission after the record (a READY TO LAND block that follows), leave
// the push to run as before.
func TestAlreadyLandedAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issue   *beads.Issue
		wantSHA string
		want    bool
	}{
		{name: "unreadable_source", issue: nil},
		{name: "no_notes", issue: &beads.Issue{}},
		{name: "landing_record", issue: &beads.Issue{Notes: alreadyLandedNotes}, wantSHA: "5ada1a68", want: true},
		{name: "record_after_other_text", issue: &beads.Issue{Notes: "see LANDING RECORD\nlanded_commit: 5ada1a68"}, wantSHA: "5ada1a68", want: true},
		{name: "empty_landed_commit", issue: &beads.Issue{Notes: "LANDING RECORD\nlanded_commit: \npatch_id: p1"}},
		{name: "resubmitted_after_landing", issue: &beads.Issue{Notes: alreadyLandedNotes + "\nREADY TO LAND\nsubmitted: now"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sha, got := alreadyLandedAt(tt.issue)
			if got != tt.want || sha != tt.wantSHA {
				t.Errorf("alreadyLandedAt() = (%q, %v), want (%q, %v)", sha, got, tt.wantSHA, tt.want)
			}
		})
	}
}
