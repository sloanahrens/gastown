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

// TestCompleteWithoutCodeCriteria runs the unchecked-criteria gate through the
// real no-code submit path: unchecked boxes are refused before anything is
// closed or written, every box ticked still closes, and merge_strategy=local
// keeps the bead open with the reason recorded and no error.
func TestCompleteWithoutCodeCriteria(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		criteria    string
		description string
		wantErr     string
		wantClosed  bool
		wantComment string
	}{
		{name: "unchecked refuses", criteria: criteriaOneTicked, wantErr: "1 unchecked acceptance criteria"},
		{name: "all ticked closes", criteria: criteriaAllTicked, wantClosed: true},
		{name: "merge_strategy=local keeps open", criteria: "- [ ] unit tests pass",
			description: "merge_strategy: local\n", wantComment: "merge_strategy=local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			h.repo.ahead = 0
			h.repo.origin[doneTestBranch] = "feature1"
			h.repo.reachable["main"] = map[string]bool{"feature1": true}
			h.r.opts.cleanupStatus = "clean"
			h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task",
				Status: string(beads.StatusHooked), AcceptanceCriteria: tc.criteria, Description: tc.description})

			err := h.submit()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("submit: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("submit = %v, want an error containing %q", err, tc.wantErr)
			}
			if closed := h.source(t).Status == string(beads.StatusClosed); closed != tc.wantClosed {
				t.Errorf("closed = %v, want %v", closed, tc.wantClosed)
			}
			comments, cerr := h.bd.Comments("bd-source")
			if cerr != nil {
				t.Fatalf("Comments: %v", cerr)
			}
			if tc.wantComment == "" && len(comments) != 0 {
				t.Errorf("wrote comments %+v, want none", comments)
			}
			if tc.wantComment != "" && (len(comments) != 1 || !strings.Contains(comments[0].Text, tc.wantComment)) {
				t.Errorf("comments %+v, want one naming %q", comments, tc.wantComment)
			}
		})
	}
}

// TestCompleteWithoutCodeSurvivesAFailedComment: a comment the store refuses
// warns and never fails gt done.
func TestCompleteWithoutCodeSurvivesAFailedComment(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.ahead = 0
	h.r.opts.cleanupStatus = "clean"
	h.client = commentFailingClient{Client: h.bd, err: errors.New("dolt unavailable")}
	h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task",
		Status: string(beads.StatusHooked), Description: "merge_strategy: local\n"})

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
