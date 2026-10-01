package convoy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// twoConvoyDB is a town with two open convoys: hq-cv-done tracks one closed
// issue, hq-cv-open tracks one open issue.
func twoConvoyDB(t *testing.T) *beadsfake.Fake {
	db := townDB()
	seedConvoy(t, db, beads.Issue{ID: "hq-cv-done", Title: "Done"},
		beads.Issue{ID: "hq-done", Title: "Done issue", Status: "closed", Type: "task"})
	seedConvoy(t, db, beads.Issue{ID: "hq-cv-open", Title: "Open"},
		beads.Issue{ID: "hq-open", Title: "Open issue", Type: "task"})
	return db
}

func TestCheckAll_DryRunNamesOnlyTheCompleteConvoy(t *testing.T) {
	t.Parallel()
	_, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	db := twoConvoyDB(t)
	town := testTown(townBeads, db, &gtScript{})

	var out bytes.Buffer
	town.Out = &out
	closed, err := town.CheckAll(context.Background(), true)
	if err != nil {
		t.Fatalf("CheckAll: %v", err)
	}
	if len(closed) != 1 || closed[0] != (Ref{ID: "hq-cv-done", Title: "Done"}) {
		t.Fatalf("CheckAll = %+v, want only hq-cv-done", closed)
	}
	if !strings.Contains(out.String(), "Would auto-close convoy") || !strings.Contains(out.String(), "1 open issue(s) remaining") {
		t.Errorf("progress output = %q, want the dry-run line and the open-issue count", out.String())
	}
	if got, _ := db.Show("hq-cv-done"); got.Status != "open" {
		t.Errorf("dry run left hq-cv-done %s, want open", got.Status)
	}
}

func TestCheckAll_CancelledContextStopsBeforeTheFirstConvoy(t *testing.T) {
	t.Parallel()
	_, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	town := testTown(townBeads, twoConvoyDB(t), &gtScript{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closed, err := town.CheckAll(ctx, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckAll on a cancelled context: err = %v, want context.Canceled", err)
	}
	if len(closed) != 0 {
		t.Errorf("CheckAll closed %+v after cancellation", closed)
	}
}

func TestChecker_CancelledContextRunsNoCheck(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The town has no convoy: a check that ran would fail with "not found",
	// not with the context's error.
	err := testTown(t.TempDir(), townDB(), &gtScript{}).Checker()(ctx, "hq-cv-x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Checker on a cancelled context: err = %v, want context.Canceled", err)
	}
}
