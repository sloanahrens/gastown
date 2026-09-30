package cmd

import (
	"errors"
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

// TestHandleMergeCompletedRespectsCloseBlocks: merge_completed must not
// close a source issue the refinery itself would not close on merge
// (no_merge, review_only, merge_strategy local).
func TestHandleMergeCompletedRespectsCloseBlocks(t *testing.T) {
	t.Parallel()
	for field, reason := range map[string]string{
		"no_merge: true":        "no_merge",
		"review_only: true":     "review_only",
		"merge_strategy: local": "merge_strategy:local",
	} {
		bd := beadsfake.New()
		bd.Seed(beads.Issue{ID: "gt-src", Title: "the work", Description: field})
		msg := &mail.Message{
			Subject: "Merge Request Completed: polecat/nux/gt-src",
			Body:    "MR: gt-mr1\nSource: gt-src\nCommit: abc123\n",
		}
		action, err := handleMergeCompletedWith(bd, t.TempDir(), msg, false)
		if err != nil {
			t.Fatalf("%s: %v", field, err)
		}
		if got, _ := bd.Show("gt-src"); got.Status != "open" {
			t.Errorf("%s: source issue status %q, want it left open", field, got.Status)
		}
		if !strings.Contains(action, reason) {
			t.Errorf("%s: action %q, want it to name %q", field, action, reason)
		}
	}
}

// showFailsClient is a beads.Client whose Show fails, and which records
// whether anything tried to close an issue.
type showFailsClient struct {
	beads.Client
	showErr error
	closed  []string
}

func (c *showFailsClient) Show(string) (*beads.Issue, error) { return nil, c.showErr }

func (c *showFailsClient) CloseWithReason(_ string, ids ...string) error {
	c.closed = append(c.closed, ids...)
	return nil
}

// TestHandleMergeCompletedLeavesIssueOpenWhenShowFails: the no_merge,
// review_only and local guard reads the source issue. When that read errors
// the guard cannot run, so the issue must stay open instead of being closed
// unchecked.
func TestHandleMergeCompletedLeavesIssueOpenWhenShowFails(t *testing.T) {
	t.Parallel()
	bd := &showFailsClient{Client: beadsfake.New(), showErr: errors.New("dolt unavailable")}
	msg := &mail.Message{
		Subject: "Merge Request Completed: polecat/nux/gt-src",
		Body:    "MR: gt-mr1\nSource: gt-src\nCommit: abc123\n",
	}

	action, err := handleMergeCompletedWith(bd, t.TempDir(), msg, false)
	if err != nil {
		t.Fatalf("handleMergeCompletedWith: %v", err)
	}
	if len(bd.closed) != 0 {
		t.Errorf("closed %v after a failed Show, want nothing closed", bd.closed)
	}
	if !strings.Contains(action, "not closing gt-src") || !strings.Contains(action, "dolt unavailable") {
		t.Errorf("action = %q, want it to say gt-src was not closed and why", action)
	}
}
