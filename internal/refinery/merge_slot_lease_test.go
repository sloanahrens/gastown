package refinery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// slotDouble models the bead-backed merge slot (beads.MergeSlotRelease): a
// release is refused unless it names the current holder, so a caller that
// releases an identity it did not acquire cannot clear the slot.
type slotDouble struct {
	holder   string
	releases []string
}

func (s *slotDouble) acquire(holder string, _ bool) (*beads.MergeSlotStatus, error) {
	if s.holder != "" && s.holder != holder {
		return &beads.MergeSlotStatus{ID: "merge-slot", Holder: s.holder}, nil
	}
	s.holder = holder
	return &beads.MergeSlotStatus{ID: "merge-slot", Holder: holder}, nil
}

func (s *slotDouble) release(holder string) error {
	s.releases = append(s.releases, holder)
	if s.holder == "" {
		return nil
	}
	if holder != "" && s.holder != holder {
		return fmt.Errorf("%w: held by %q, not %q", beads.ErrMergeSlotNotHolder, s.holder, holder)
	}
	s.holder = ""
	return nil
}

func newSlotEngineer(t *testing.T, rigName string) (*Engineer, *slotDouble, *bytes.Buffer) {
	t.Helper()
	slot := &slotDouble{}
	e := &Engineer{
		rig:                   &rig.Rig{Name: rigName},
		mergeSlotEnsureExists: func() (string, error) { return "merge-slot", nil },
		mergeSlotAcquire:      slot.acquire,
		mergeSlotRelease:      slot.release,
		mergeSlotMaxRetries:   0,
		mergeSlotRetryBackoff: time.Millisecond,
	}
	var buf bytes.Buffer
	e.output = &buf
	return e, slot, &buf
}

func TestPushLeaseAcquiredAt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 21, 3, 11, 22, 283920000, time.UTC)
	valid := fmt.Sprintf("testrig/refinery/push/%d-1", at.UnixNano())

	for _, tc := range []struct {
		name   string
		holder string
		wantOK bool
	}{
		{"per-push lease", valid, true},
		{"bare conflict lease", "testrig/refinery", false},
		{"another rig's push lease", fmt.Sprintf("otherrig/refinery/push/%d-1", at.UnixNano()), false},
		{"missing sequence", "testrig/refinery/push/1789960282283920000", false},
		{"non-numeric timestamp", "testrig/refinery/push/abc-1", false},
		{"zero timestamp", "testrig/refinery/push/0-1", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pushLeaseAcquiredAt(tc.holder, "testrig")
			if ok != tc.wantOK {
				t.Fatalf("pushLeaseAcquiredAt(%q) ok = %v, want %v", tc.holder, ok, tc.wantOK)
			}
			if ok && !got.Equal(at) {
				t.Errorf("pushLeaseAcquiredAt(%q) = %v, want %v", tc.holder, got, at)
			}
		})
	}
}

func TestAcquireMainPushSlot_ReclaimsStalePushLease(t *testing.T) {
	t.Parallel()
	e, slot, buf := newSlotEngineer(t, "testrig")
	stale := fmt.Sprintf("testrig/refinery/push/%d-1", time.Now().Add(-time.Hour).UnixNano())
	slot.holder = stale
	e.mergeSlotStaleAfter = 10 * time.Minute

	holder, err := e.acquireMainPushSlot(context.Background())
	if err != nil {
		t.Fatalf("expected the abandoned lease to be reclaimed, got: %v", err)
	}
	if holder == "" {
		t.Fatal("expected a push holder after reclaiming the stale lease")
	}
	if slot.holder != holder {
		t.Errorf("slot held by %q, want the newly acquired %q", slot.holder, holder)
	}
	if len(slot.releases) != 1 || slot.releases[0] != stale {
		t.Errorf("releases = %v, want exactly the stale lease %q", slot.releases, stale)
	}
	if !strings.Contains(buf.String(), "is stale") {
		t.Errorf("reclaim was not reported; output:\n%s", buf.String())
	}
}

func TestLeaseStaleAfter_IgnoresStaleClaimTimeout(t *testing.T) {
	t.Parallel()
	// StaleClaimTimeout bounds an untouched MR claim (legitimately long); the
	// push-lease TTL bounds a git push (always short). A rig that tunes the
	// former down must not also shrink the latter, or a live push lease gets
	// reclaimed out from under it (om major on gt-wisp-np1, gt-vyjc).
	e, slot, _ := newSlotEngineer(t, "testrig")
	e.config = &MergeQueueConfig{StaleClaimTimeout: time.Minute}
	live := fmt.Sprintf("testrig/refinery/push/%d-1", time.Now().Add(-5*time.Minute).UnixNano())
	slot.holder = live

	_, err := e.acquireMainPushSlot(context.Background())
	if !errors.Is(err, errMergeSlotTimeout) {
		t.Fatalf("expected contention against a lease inside the push-lease window, got: %v", err)
	}
	if len(slot.releases) != 0 {
		t.Errorf("a live lease was reclaimed using the shorter StaleClaimTimeout: releases = %v", slot.releases)
	}
}

func TestAcquireMainPushSlot_KeepsLivePushLease(t *testing.T) {
	t.Parallel()
	e, slot, _ := newSlotEngineer(t, "testrig")
	live := fmt.Sprintf("testrig/refinery/push/%d-1", time.Now().UnixNano())
	slot.holder = live
	e.mergeSlotStaleAfter = 10 * time.Minute

	_, err := e.acquireMainPushSlot(context.Background())
	if !errors.Is(err, errMergeSlotTimeout) {
		t.Fatalf("expected contention against a live lease, got: %v", err)
	}
	if len(slot.releases) != 0 {
		t.Errorf("a live lease was reclaimed: releases = %v", slot.releases)
	}
	if slot.holder != live {
		t.Errorf("slot held by %q, want the live lease %q", slot.holder, live)
	}
}

func TestAcquireMainPushSlot_KeepsLiveLeaseAtTheBoundary(t *testing.T) {
	t.Parallel()
	// A batch holds its lease across every member's post-merge cleanup, so a
	// holder still inside the window is live even when it is far from fresh.
	e, slot, _ := newSlotEngineer(t, "testrig")
	e.mergeSlotStaleAfter = 10 * time.Minute
	live := fmt.Sprintf("testrig/refinery/push/%d-1", time.Now().Add(-e.mergeSlotStaleAfter+2*time.Second).UnixNano())
	slot.holder = live

	_, err := e.acquireMainPushSlot(context.Background())
	if !errors.Is(err, errMergeSlotTimeout) {
		t.Fatalf("expected contention against a lease inside the window, got: %v", err)
	}
	if len(slot.releases) != 0 {
		t.Errorf("a live lease was reclaimed: releases = %v", slot.releases)
	}
}

func TestAcquireMainPushSlot_ReclaimsLeaseAtTheBoundary(t *testing.T) {
	t.Parallel()
	e, slot, _ := newSlotEngineer(t, "testrig")
	e.mergeSlotStaleAfter = 10 * time.Minute
	expired := fmt.Sprintf("testrig/refinery/push/%d-1", time.Now().Add(-e.mergeSlotStaleAfter-time.Nanosecond).UnixNano())
	slot.holder = expired

	if _, err := e.acquireMainPushSlot(context.Background()); err != nil {
		t.Fatalf("expected a lease at the window's edge to be reclaimed, got: %v", err)
	}
	if len(slot.releases) != 1 || slot.releases[0] != expired {
		t.Errorf("releases = %v, want exactly the expired lease %q", slot.releases, expired)
	}
}

func TestAcquireMainPushSlot_ReportsFutureDatedLease(t *testing.T) {
	t.Parallel()
	// A clock step can date a live lease ahead of this reader. Stealing on a
	// negative age would take a live lease, so the lease is kept and the
	// refusal is reported rather than passing as ordinary contention.
	e, slot, buf := newSlotEngineer(t, "testrig")
	e.mergeSlotStaleAfter = 10 * time.Minute
	slot.holder = fmt.Sprintf("testrig/refinery/push/%d-1", time.Now().Add(time.Hour).UnixNano())

	_, err := e.acquireMainPushSlot(context.Background())
	if !errors.Is(err, errMergeSlotTimeout) {
		t.Fatalf("expected contention against a future-dated lease, got: %v", err)
	}
	if len(slot.releases) != 0 {
		t.Errorf("a future-dated lease was reclaimed: releases = %v", slot.releases)
	}
	if out := buf.String(); !strings.Contains(out, "in the future") {
		t.Errorf("future-dated lease was not reported; output:\n%s", out)
	}
}

func TestAcquireMainPushSlot_DoesNotReclaimConflictLease(t *testing.T) {
	t.Parallel()
	// The conflict-resolution identity is held across a dispatched task, so age
	// is no signal that it is abandoned — the push path proceeds alongside it.
	e, slot, _ := newSlotEngineer(t, "testrig")
	slot.holder = "testrig/refinery"

	holder, err := e.acquireMainPushSlot(context.Background())
	if err != nil {
		t.Fatalf("expected the conflict-resolution bypass, got: %v", err)
	}
	if holder != "" {
		t.Errorf("holder = %q, want empty (conflict resolution owns the slot)", holder)
	}
	if len(slot.releases) != 0 {
		t.Errorf("conflict lease was reclaimed: releases = %v", slot.releases)
	}
}

func TestAcquireMainPushSlot_DoesNotReclaimForeignLease(t *testing.T) {
	t.Parallel()
	e, slot, _ := newSlotEngineer(t, "testrig")
	foreign := fmt.Sprintf("otherrig/refinery/push/%d-1", time.Now().Add(-time.Hour).UnixNano())
	slot.holder = foreign

	_, err := e.acquireMainPushSlot(context.Background())
	if !errors.Is(err, errMergeSlotTimeout) {
		t.Fatalf("expected contention against another rig's lease, got: %v", err)
	}
	if len(slot.releases) != 0 {
		t.Errorf("another rig's lease was reclaimed: releases = %v", slot.releases)
	}
}

func TestHandleMRInfoSuccess_ReleasesConflictLease(t *testing.T) {
	// A successful merge nudges mayor (gt-i0ld) — fake gt on PATH so the
	// test never shells out to the real binary.
	fakeBDAndGt(t)
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	installNoPRGH(t)

	branch := "polecat/test/conflict-lease"
	createFeatureBranch(t, workDir, branch, "conflict.txt", "resolved\n")
	commit := run(t, workDir, "git", "rev-parse", branch)
	run(t, workDir, "git", "push", "origin", branch)
	run(t, workDir, "git", "checkout", "main")
	run(t, workDir, "git", "merge", "--ff-only", branch)
	run(t, workDir, "git", "push", "origin", "main")

	e := newTestEngineer(t, workDir, g)
	slot := &slotDouble{holder: "test-rig/refinery"}
	e.mergeSlotAcquire = slot.acquire
	e.mergeSlotRelease = slot.release

	if !e.HandleMRInfoSuccess(&MRInfo{
		ID:        "mr-conflict-lease",
		Branch:    branch,
		Target:    "main",
		CommitSHA: commit,
	}, ProcessResult{Success: true, MergeCommit: run(t, workDir, "git", "rev-parse", "main")}) {
		t.Fatal("HandleMRInfoSuccess failed")
	}
	if slot.holder != "" {
		t.Errorf("conflict lease still held by %q after a successful merge", slot.holder)
	}
}

func TestCreateConflictResolutionTask_ReclaimsOnlyStalePushLease(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()

	cases := []struct {
		name          string
		holder        string
		wantReclaimed bool
	}{
		{"stale push lease", fmt.Sprintf("test-rig/refinery/push/%d-1", time.Now().Add(-time.Hour).UnixNano()), true},
		{"live push lease", fmt.Sprintf("test-rig/refinery/push/%d-1", time.Now().UnixNano()), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A stale push lease defers every conflict task forever, and when no
			// push follows there is no other reclaimer, so this second
			// acquisition site must reclaim it too (gt-pp44).
			slot := &slotDouble{holder: tc.holder}
			e := newTestEngineer(t, workDir, g)
			e.mergeSlotAcquire = slot.acquire
			e.mergeSlotRelease = slot.release
			var buf bytes.Buffer
			e.SetOutput(&buf)

			// The rig has no beads database, so the task itself cannot land —
			// the slot gate is what this asserts.
			_, _ = e.createConflictResolutionTaskForMR(&MRInfo{
				ID:          "mr-conflict",
				Branch:      "polecat/test/conflict",
				Target:      "main",
				SourceIssue: "gt-conflict",
			}, ProcessResult{Success: false, Conflict: true})

			if deferred := strings.Contains(buf.String(), "deferring conflict resolution"); deferred == tc.wantReclaimed {
				t.Errorf("deferred = %v, want %v; output:\n%s", deferred, !tc.wantReclaimed, buf.String())
			}
			if reclaimed := len(slot.releases) > 0 && slot.releases[0] == tc.holder; reclaimed != tc.wantReclaimed {
				t.Errorf("reclaimed = %v, want %v; releases = %v", reclaimed, tc.wantReclaimed, slot.releases)
			}
		})
	}
}

func TestFastForwardBatch_LeavesNoLeaseHeld(t *testing.T) {
	workDir, g, cleanup := testGitRepo(t)
	defer cleanup()
	installNoPRGH(t)

	writeFile(t, workDir, "batch.txt", "batched\n")
	run(t, workDir, "git", "add", ".")
	run(t, workDir, "git", "commit", "-m", "batch result")
	tip := run(t, workDir, "git", "rev-parse", "HEAD")

	e := newTestEngineer(t, workDir, g)
	e.rig = &rig.Rig{Name: "test-rig", Path: workDir}
	e.testAllowSyntheticMRs = true
	slot := &slotDouble{}
	e.mergeSlotAcquire = slot.acquire
	e.mergeSlotRelease = slot.release
	var buf bytes.Buffer
	e.SetOutput(&buf)

	mr := &MRInfo{ID: "mr-batch-lease", Branch: "polecat/test/batch", Target: "main", CommitSHA: tip}
	result := e.fastForwardBatch(context.Background(), []*MRInfo{mr}, "main", &BatchResult{})
	if result.Error != nil {
		t.Fatalf("fastForwardBatch failed: %v\n%s", result.Error, buf.String())
	}
	if slot.holder != "" {
		t.Errorf("merge slot still held by %q after the batch", slot.holder)
	}
	// The conflict-release step runs while this batch holds its own push lease,
	// so it must not report a refusal: naming the live push lease as the reason
	// a release failed is what read as a leaked lease (gt-pp44).
	out := buf.String()
	for _, reported := range []string{"slot release failed", "Note: merge slot release", "could not release conflict-resolution"} {
		if strings.Contains(out, reported) {
			t.Errorf("batch reported a bogus merge-slot release failure (%q):\n%s", reported, out)
		}
	}
}
