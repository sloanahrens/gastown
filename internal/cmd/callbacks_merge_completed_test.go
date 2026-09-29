package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/mail"
)

// TestHandleMergeCompletedClosesSourceIssue: a merge_completed callback
// closes its source issue, recording the merge commit as the reason. It
// used to pass the reason to Close as a second issue ID, so the batch failed
// as not found and the source issue stayed open.
func TestHandleMergeCompletedClosesSourceIssue(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	bd.Seed(beads.Issue{ID: "gt-src", Title: "the work"})
	msg := &mail.Message{
		Subject: "Merge Request Completed: polecat/nux/gt-src",
		Body:    "MR: gt-mr1\nSource: gt-src\nCommit: abc123\n",
	}

	action, err := handleMergeCompletedWith(bd, t.TempDir(), msg, false)
	if err != nil {
		t.Fatalf("handleMergeCompletedWith: %v", err)
	}
	got, err := bd.Show("gt-src")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || got.CloseReason != "Merged in abc123" {
		t.Errorf("source issue: status %q reason %q, want closed \"Merged in abc123\" (action %q)", got.Status, got.CloseReason, action)
	}
	if !strings.Contains(action, "closed gt-src") {
		t.Errorf("action = %q, want it to report closing gt-src", action)
	}
}
