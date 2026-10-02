package landworker

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// seedInstallNow seeds one ready bead carrying the operator's gt:install-now
// label as well as the one gt done sets.
func seedInstallNow(t *testing.T, h *harness, id string) {
	t.Helper()
	h.seedReady(t, id)
	if err := h.bd.Update(id, beads.UpdateOptions{AddLabels: []string{LabelInstallNow}}); err != nil {
		t.Fatal(err)
	}
}

// TestPassInstallNowLandedRequestsAnInstall: the label rides the bead through
// to the pass report, which is what lets the daemon arm the install for the
// pass that landed it (gt-3qmv4.2).
func TestPassInstallNowLandedRequestsAnInstall(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedInstallNow(t, h, "gt-abc")
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || !rep.InstallRequested {
		t.Fatalf("report %+v; want one landing requesting an install", rep)
	}
}

// TestPassLandingWithoutTheLabelRequestsNoInstall: the request belongs to the
// label, so an ordinary landing carries none.
func TestPassLandingWithoutTheLabelRequestsNoInstall(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || rep.InstallRequested {
		t.Fatalf("report %+v; want a landing with no install request", rep)
	}
}

// TestPassInstallNowRejectionRequestsNoInstall: a bead the gate sent back
// never reached main, so its label asks for nothing new to be installed
// (gt-3qmv4.2).
func TestPassInstallNowRejectionRequestsNoInstall(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedInstallNow(t, h, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectGate, Reason: "gate failed on the merged tree", Rework: true}
	}
	rep := h.w.Pass(context.Background())
	if rep.Rejected != 1 || rep.InstallRequested {
		t.Fatalf("report %+v; want a rejection requesting no install", rep)
	}
}

// TestPassInstallNowSkipRequestsNoInstall: a bead with no branch to land is
// left for a human, so its label asks for nothing (gt-3qmv4.2).
func TestPassInstallNowSkipRequestsNoInstall(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedInstallNow(t, h, "gt-abc")
	h.remote.tips = map[string]string{}
	rep := h.w.Pass(context.Background())
	if rep.Skipped != 1 || rep.InstallRequested {
		t.Fatalf("report %+v; want a skip requesting no install", rep)
	}
}

// TestPassInstallNowRepairRequestsNoInstall: finishing the record of an
// earlier landing is not a landing, so it does not re-request the install the
// landing's own pass already requested (gt-3qmv4.2).
func TestPassInstallNowRepairRequestsNoInstall(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedInstallNow(t, h, "gt-abc")
	h.lander.fn = func(n int, _ land.Work) (land.Result, error) {
		if n == 1 {
			res := land.Result{LandedCommit: "cccccccc"}
			return res, &land.RecordError{Result: res, Err: errors.New("close failed")}
		}
		return land.Result{LandedCommit: "cccccccc"}, nil
	}
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || !rep.InstallRequested {
		t.Fatalf("first pass %+v; want the landing to request the install", rep)
	}
	// The label is gone now (Land took it off before the close failed), so
	// only the pending repair can bring the bead back.
	if err := h.bd.Update("gt-abc", beads.UpdateOptions{RemoveLabels: []string{land.LabelReadyToLand}}); err != nil {
		t.Fatal(err)
	}
	h.remote.tips[branch] = "3333333333333333333333333333333333333333"
	rep = h.w.Pass(context.Background())
	if rep.Repaired != 1 || rep.InstallRequested {
		t.Fatalf("second pass %+v; want a repair requesting no install", rep)
	}
}
