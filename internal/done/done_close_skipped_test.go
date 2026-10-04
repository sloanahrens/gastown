package done

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestNotifyDoneCloseSkippedCommentsTheReason covers both reasons gt done
// builds for a skipped close: unchecked acceptance criteria (internal/done)
// and commits the target lacks (internal/cmd's close-time invariant). Each
// lands as one comment on the skipped bead itself, carrying the reason
// verbatim, instead of a dead letter to the retired mayor/ role (gt-zx8t4).
func TestNotifyDoneCloseSkippedCommentsTheReason(t *testing.T) {
	t.Parallel()
	reasons := map[string]string{
		"unchecked criteria": "issue bd-skip has 2 unchecked acceptance criteria — skipping close",
		"commits not on target": "branch polecat/basalt/gt-6hmz+abc has 1 commit(s) not on main and gt-6hmz is not landed — " +
			"refusing close (gt-6hmz close-time invariant)",
	}
	for name, reason := range reasons {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bd := beadsfake.New(beadsfake.WithPrefix("bd"))
			bd.Seed(beads.Issue{ID: "bd-skip", Title: "the work", Type: "task"})

			NotifyDoneCloseSkipped(bd, "bd-skip", reason)

			comments, err := bd.Comments("bd-skip")
			if err != nil {
				t.Fatalf("Comments: %v", err)
			}
			if len(comments) != 1 {
				t.Fatalf("wrote %d comments, want exactly 1: %+v", len(comments), comments)
			}
			if !strings.Contains(comments[0].Text, "DONE_CLOSE_SKIPPED") || !strings.Contains(comments[0].Text, reason) {
				t.Errorf("comment %q does not carry the reason %q", comments[0].Text, reason)
			}
		})
	}
}

// TestNotifyDoneCloseSkippedWithoutAClientIsANoOp: the caller may have no
// routed client for the skipped bead. That must not panic — the skip is
// already reported on the terminal.
func TestNotifyDoneCloseSkippedWithoutAClientIsANoOp(t *testing.T) {
	t.Parallel()
	NotifyDoneCloseSkipped(nil, "bd-skip", "reason")
	NotifyDoneCloseSkipped(beadsfake.New(beadsfake.WithPrefix("bd")), "", "reason")
}

// commentFailingClient is a beads.Client whose comment write always fails, as
// a Dolt outage would make it.
type commentFailingClient struct {
	beads.Client
	err error
}

func (f commentFailingClient) AddComment(id, comment string) error { return f.err }

// TestCompleteWithoutCodeCommentsSkippedClose runs the refusal through the
// real no-code submit path: a bead whose acceptance criteria are unchecked is
// left open, the reason is recorded on it, and submit still succeeds (the
// skip is policy, not a failed submit).
func TestCompleteWithoutCodeCommentsSkippedClose(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.ahead = 0
	h.r.cleanupStatus = "clean"
	seedCriteria(h, "- [ ] unit tests pass")

	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	comments, err := h.bd.Comments("bd-source")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("wrote %d comments, want 1: %+v", len(comments), comments)
	}
	if !strings.Contains(comments[0].Text, "unchecked acceptance criteria") {
		t.Errorf("comment %q does not name the skip reason", comments[0].Text)
	}
	if issue := h.source(t); issue.Status == string(beads.StatusClosed) {
		t.Error("a skipped close closed the bead anyway")
	}
}

// TestCompleteWithoutCodeSurvivesAFailedComment: a comment the store refuses
// warns and never fails gt done.
func TestCompleteWithoutCodeSurvivesAFailedComment(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.ahead = 0
	h.r.cleanupStatus = "clean"
	h.client = commentFailingClient{Client: h.bd, err: errors.New("dolt unavailable")}
	seedCriteria(h, "- [ ] unit tests pass")

	if err := h.submit(); err != nil {
		t.Fatalf("submit failed on an unwritable comment: %v", err)
	}
	comments, err := h.bd.Comments("bd-source")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 0 {
		t.Errorf("comment write should have failed, got %+v", comments)
	}
}
