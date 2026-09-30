package convoy

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// twoConvoyBdStub answers bd for a town with two open convoys: hq-cv-done
// tracks one closed issue, hq-cv-open tracks one open issue.
const twoConvoyBdStub = `
case "$*" in
  "--allow-stale version")
    exit 0
    ;;
  *list*--label=gt:convoy*)
    echo '[{"id":"hq-cv-done","title":"Done","status":"open","issue_type":"convoy","labels":["gt:convoy"]},{"id":"hq-cv-open","title":"Open","status":"open","issue_type":"convoy","labels":["gt:convoy"]}]'
    ;;
  *list*)
    echo '[]'
    ;;
  *sql*"issue_id = 'hq-cv-done'"*)
    echo '[{"depends_on_id":"hq-done"}]'
    ;;
  *sql*"issue_id = 'hq-cv-open'"*)
    echo '[{"depends_on_id":"hq-open"}]'
    ;;
  *show*hq-done*)
    echo '[{"id":"hq-done","title":"Done issue","status":"closed","issue_type":"task"}]'
    ;;
  *show*hq-open*)
    echo '[{"id":"hq-open","title":"Open issue","status":"open","issue_type":"task"}]'
    ;;
  *)
    echo "unexpected bd args: $*" >&2
    exit 1
    ;;
esac
`

func TestCheckAll_DryRunNamesOnlyTheCompleteConvoy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}
	townRoot, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	chdirExternalTrackingTest(t, townRoot)
	writeExternalTrackingBdStub(t, twoConvoyBdStub)

	var out bytes.Buffer
	closed, err := Town{Root: townBeads, Out: &out}.CheckAll(context.Background(), true)
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
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}
	townRoot, townBeads, _ := makeExternalTrackingTownWorkspace(t)
	chdirExternalTrackingTest(t, townRoot)
	writeExternalTrackingBdStub(t, twoConvoyBdStub)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closed, err := Town{Root: townBeads}.CheckAll(ctx, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckAll on a cancelled context: err = %v, want context.Canceled", err)
	}
	if len(closed) != 0 {
		t.Errorf("CheckAll closed %+v after cancellation", closed)
	}
}

func TestChecker_CancelledContextRunsNoCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The town does not exist: a check that ran would fail with "not found",
	// not with the context's error.
	err := Town{Root: t.TempDir()}.Checker()(ctx, "hq-cv-x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Checker on a cancelled context: err = %v, want context.Canceled", err)
	}
}
