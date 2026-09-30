package convoy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// twoConvoyBd answers bd for a town with two open convoys: hq-cv-done
// tracks one closed issue, hq-cv-open tracks one open issue.
func twoConvoyBd() *bdScript {
	return &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		line := strings.Join(c.Args, " ")
		switch {
		case line == "--allow-stale version":
			return "", "", 0
		case strings.Contains(line, "list") && strings.Contains(line, "--label=gt:convoy"):
			return `[{"id":"hq-cv-done","title":"Done","status":"open","issue_type":"convoy","labels":["gt:convoy"]},{"id":"hq-cv-open","title":"Open","status":"open","issue_type":"convoy","labels":["gt:convoy"]}]`, "", 0
		case strings.Contains(line, "list"):
			return "[]", "", 0
		case strings.Contains(line, "sql") && strings.Contains(line, "issue_id = 'hq-cv-done'"):
			return `[{"depends_on_id":"hq-done"}]`, "", 0
		case strings.Contains(line, "sql") && strings.Contains(line, "issue_id = 'hq-cv-open'"):
			return `[{"depends_on_id":"hq-open"}]`, "", 0
		case strings.Contains(line, "show") && strings.Contains(line, "hq-done"):
			return `[{"id":"hq-done","title":"Done issue","status":"closed","issue_type":"task"}]`, "", 0
		case strings.Contains(line, "show") && strings.Contains(line, "hq-open"):
			return `[{"id":"hq-open","title":"Open issue","status":"open","issue_type":"task"}]`, "", 0
		}
		return "", "unexpected bd args: " + line, 1
	}}
}

func TestCheckAll_DryRunNamesOnlyTheCompleteConvoy(t *testing.T) {
	t.Parallel()
	_, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	town := testTown(townBeads, twoConvoyBd(), &gtScript{})

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
}

func TestCheckAll_CancelledContextStopsBeforeTheFirstConvoy(t *testing.T) {
	t.Parallel()
	_, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	town := testTown(townBeads, twoConvoyBd(), &gtScript{})

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
	// The town does not exist: a check that ran would fail with "not found",
	// not with the context's error.
	err := testTown(t.TempDir(), &bdScript{}, &gtScript{}).Checker()(ctx, "hq-cv-x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Checker on a cancelled context: err = %v, want context.Canceled", err)
	}
}
