package schedulerrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/installpending"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// writeJSONFile writes v as indented JSON at path, creating parent dirs.
func writeJSONFile(t *testing.T, path string, v interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal JSON for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func captureTo(write func(w io.Writer)) string {
	var b strings.Builder
	write(&b)
	return b.String()
}

func TestRunReportsHeldLock(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	runtimeDir := filepath.Join(townRoot, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	lockFile := filepath.Join(runtimeDir, "scheduler-dispatch.lock")
	lock := flock.New(lockFile)
	locked, err := lock.TryLock()
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if !locked {
		t.Fatal("test could not acquire scheduler dispatch lock")
	}
	t.Cleanup(func() { _ = lock.Unlock() })

	_, err = Run(t.Context(), Options{TownRoot: townRoot, Actor: "test", BatchOverride: 1}, Deps{})
	if err == nil {
		t.Fatal("Run succeeded with held scheduler lock")
	}
	if !strings.Contains(err.Error(), "scheduler dispatch already in progress") || !strings.Contains(err.Error(), lockFile) {
		t.Fatalf("error = %q, want explicit held lock reason with path", err.Error())
	}
}

// gt-ifijm: `gt scheduler run` is reached from the daemon heartbeat, the
// witness on SLOT_OPEN, and by hand; Run is the one place all three pass
// through, so the operator hold is enforced there.

// With the hold in place nothing is read or slung: the dispatch lock is never
// taken, so the gate stops the run before the planner.
func TestRun_OperatorHold_DispatchesNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "seat-refill.hold"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(t.Context(), Options{TownRoot: townRoot, Actor: "test", BatchOverride: 1}, Deps{})
	if err != nil || report.Dispatched != 0 {
		t.Fatalf("Run under a hold = (%+v, %v), want no dispatch and no error", report, err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".runtime", "scheduler-dispatch.lock")); err == nil {
		t.Error("the dispatch lock was taken during a hold: the gate must come first")
	}
}

// gt-c0nvv: a queue dispatch cycle holds while a gt install is pending, as gt
// sling and the spec dispatcher do (gt-2rp64), and holds before it plans — no
// seat read, no lock, nothing dispatched or failed — so no polecat is spawned
// from a binary an install is replacing.
func TestRun_HoldsWhileAnInstallIsPending(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name   string
		at     time.Time
		broken bool
	}{
		{name: "live marker", at: now.Add(-time.Minute)},
		{name: "unreadable marker", broken: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			store := installpending.StoreIn(filepath.Join(t.TempDir(), installpending.FileName))
			if !tc.at.IsZero() {
				if err := store.Write(tc.at, os.Getpid()); err != nil {
					t.Fatalf("writing the install-pending marker: %v", err)
				}
			}
			if tc.broken {
				if err := os.WriteFile(store.Path(), []byte("{not a marker"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			seatsRead := 0
			report, err := Run(t.Context(), Options{
				TownRoot:      townRoot,
				Actor:         "test",
				BatchOverride: 1,
				Out:           io.Discard,
				ErrOut:        io.Discard,
			}, Deps{
				PendingInstall: func(string) (*installpending.Marker, error) { return store.Pending(now) },
				Seats: func(string, bool) (Seats, error) {
					seatsRead++
					return Seats{}, nil
				},
			})

			if tc.broken {
				if err == nil || !strings.Contains(err.Error(), "pending-install marker") {
					t.Fatalf("Run = (%+v, %v), want the unreadable marker reported", report, err)
				}
			} else {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if report.Reason != ReasonInstallPending || report.Dispatched != 0 || report.Failed != 0 {
					t.Fatalf("report = %+v, want a queued hold that dispatched and failed nothing", report)
				}
			}
			if seatsRead != 0 {
				t.Errorf("the run read seats %d time(s): the hold must come before the plan", seatsRead)
			}
			if _, statErr := os.Stat(filepath.Join(townRoot, ".runtime", "scheduler-dispatch.lock")); statErr == nil {
				t.Error("the dispatch lock was taken under the hold: the hold must come first")
			}
		})
	}
}

// The hold both the run's gate and each planned sling read (the second one as a
// Deferral, so a context that reached its sling stays queued instead of closing
// as circuit-broken): a live marker names the install, and a marker that has
// aged out or none holds nothing (gt-c0nvv).
func TestInstallPendingHold_HoldsOnlyWhileAnInstallIsPending(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name   string
		at     time.Time
		broken bool
		want   bool
	}{
		{name: "live marker", at: now.Add(-time.Minute), want: true},
		{name: "stale marker", at: now.Add(-installpending.Ceiling)},
		{name: "no marker"},
		{name: "unreadable marker", broken: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			store := installpending.StoreIn(filepath.Join(t.TempDir(), installpending.FileName))
			if !tc.at.IsZero() {
				if err := store.Write(tc.at, os.Getpid()); err != nil {
					t.Fatalf("writing the install-pending marker: %v", err)
				}
			}
			if tc.broken {
				if err := os.WriteFile(store.Path(), []byte("{not a marker"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			slung := 0
			r := &runner{deps: Deps{
				PendingInstall: func(string) (*installpending.Marker, error) { return store.Pending(now) },
				Sling: func(context.Context, string, capacity.PendingBead) (SlingOutcome, error) {
					slung++
					return SlingOutcome{PolecatName: "furiosa"}, nil
				},
			}}

			hold, err := r.installPendingHold(townRoot)
			deferral := r.installPendingDeferral(townRoot)
			if !tc.want {
				if err != nil || hold != "" || deferral != nil {
					t.Fatalf("hold = (%q, %v), deferral = %v, want the sling left to run", hold, err, deferral)
				}
				// The one planned sling runs: the marker that does not stand
				// holds nothing on the dispatch path either.
				b := capacity.PendingBead{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"}
				polecatNames, successfulRigs := map[string]string{}, map[string]bool{}
				if err := r.dispatchReady(t.Context(), townRoot, b, polecatNames, successfulRigs); err != nil {
					t.Fatalf("dispatchReady: %v", err)
				}
				if slung != 1 || polecatNames[b.ID] != "furiosa" || !successfulRigs[b.TargetRig] {
					t.Fatalf("slung %d, names %v, rigs %v, want the bead dispatched and recorded", slung, polecatNames, successfulRigs)
				}
				return
			}
			if deferral == nil {
				t.Fatalf("hold = (%q, %v), want a deferral holding the sling", hold, err)
			}
			if _, deferred := asDeferral(deferral); !deferred {
				t.Fatal("the hold is not a schedulerrun deferral: the context would count a failure")
			}
			if deferral.Why != "Install pending" {
				t.Errorf("Why = %q, want %q", deferral.Why, "Install pending")
			}
			// The planned sling is held as the same deferral, so the context
			// stays queued and nothing is spawned under the install.
			b := capacity.PendingBead{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"}
			slingErr := r.dispatchReady(t.Context(), townRoot, b, map[string]string{}, map[string]bool{})
			if _, deferred := asDeferral(slingErr); !deferred {
				t.Errorf("dispatchReady = %v, want the sling held as a deferral", slingErr)
			}
			if slung != 0 {
				t.Errorf("the sling ran %d time(s) under a pending install", slung)
			}
			if tc.broken {
				if err == nil || !strings.Contains(deferral.Error(), "pending-install marker") {
					t.Errorf("hold = (%q, %v), deferral = %q, want the unreadable marker named", hold, err, deferral)
				}
				return
			}
			if want := fmt.Sprintf("daemon pid %d", os.Getpid()); !strings.Contains(hold, "an install of gt is pending") ||
				!strings.Contains(hold, want) || deferral.Error() != hold {
				t.Errorf("hold = %q, deferral = %q, want both to name the install by %q", hold, deferral, want)
			}
		})
	}
}

// A sling that fails for its own reason reaches the cycle as that failure, not
// as a pending-install deferral: only a marker that stands keeps the context
// queued (gt-c0nvv).
func TestDispatchReady_ReturnsTheSlingFailure(t *testing.T) {
	t.Parallel()
	r := &runner{deps: Deps{
		PendingInstall: func(string) (*installpending.Marker, error) { return nil, nil },
		Sling: func(context.Context, string, capacity.PendingBead) (SlingOutcome, error) {
			return SlingOutcome{}, errors.New("spawn failed")
		},
	}}

	err := r.dispatchReady(t.Context(), t.TempDir(), capacity.PendingBead{ID: "ctx-1", WorkBeadID: "gt-a"},
		map[string]string{}, map[string]bool{})
	if err == nil || err.Error() != "spawn failed" {
		t.Fatalf("dispatchReady = %v, want the sling's own failure", err)
	}
	if _, deferred := asDeferral(err); deferred {
		t.Error("the sling failure was reported as a deferral, which hides it from the circuit breaker")
	}
}

// A run whose context has ended slings nothing: the check comes before the
// install read and the sling, and its refusal is the context's own error.
func TestDispatchReady_CancelledRunSlingsNothing(t *testing.T) {
	t.Parallel()
	slung := 0
	r := &runner{deps: Deps{
		PendingInstall: func(string) (*installpending.Marker, error) { return nil, nil },
		Sling: func(context.Context, string, capacity.PendingBead) (SlingOutcome, error) {
			slung++
			return SlingOutcome{}, nil
		},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := r.dispatchReady(ctx, t.TempDir(), capacity.PendingBead{ID: "ctx-1", WorkBeadID: "gt-a"},
		map[string]string{}, map[string]bool{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dispatchReady = %v, want the run's context error", err)
	}
	if slung != 0 {
		t.Errorf("the sling ran %d time(s) after the run's context ended", slung)
	}
}

// gt-c0nvv: a sling that meets an install mid-cycle leaves its context queued
// through the cycle a run executes: the deferral is named as the install hold,
// the context is not counted toward the circuit breaker, and nothing is slung
// under the install.
func TestDispatchCycle_InstallDeferralLeavesContextQueued(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := installpending.StoreIn(filepath.Join(t.TempDir(), installpending.FileName))
	if err := store.Write(now.Add(-time.Minute), os.Getpid()); err != nil {
		t.Fatalf("writing the install-pending marker: %v", err)
	}
	slung := 0
	var errOut strings.Builder
	r := &runner{opts: Options{Out: io.Discard, ErrOut: &errOut}, deps: Deps{
		PendingInstall: func(string) (*installpending.Marker, error) { return store.Pending(now) },
		Sling: func(context.Context, string, capacity.PendingBead) (SlingOutcome, error) {
			slung++
			return SlingOutcome{}, nil
		},
	}}
	cycle := r.dispatchCycle(t.Context(), t.TempDir(), 0, map[string]string{}, map[string]bool{})

	if _, err := cycle.RunPlan(capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "gt-a"}},
	}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if slung != 0 {
		t.Errorf("the sling ran %d time(s) under a pending install", slung)
	}
	if want := "leaving context queued"; !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr = %q, want the deferred context to be left queued (%q)", errOut.String(), want)
	}
	if !strings.Contains(errOut.String(), "an install of gt is pending") {
		t.Errorf("stderr = %q, want the deferral to name the install", errOut.String())
	}
}

// gt-c0nvv: the daemon heartbeat holds for an install like an operator run
// does, and its log is the only place a queue that does not move says why, so
// the hold is narrated there too.
func TestRun_DaemonNarratesAnInstallHold(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := installpending.StoreIn(filepath.Join(t.TempDir(), installpending.FileName))
	if err := store.Write(now.Add(-time.Minute), os.Getpid()); err != nil {
		t.Fatalf("writing the install-pending marker: %v", err)
	}
	var out strings.Builder
	report, err := Run(t.Context(), Options{
		TownRoot: t.TempDir(),
		Actor:    "test",
		Daemon:   true,
		Out:      &out,
		ErrOut:   io.Discard,
	}, Deps{
		PendingInstall: func(string) (*installpending.Marker, error) { return store.Pending(now) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Reason != ReasonInstallPending {
		t.Fatalf("report = %+v, want the install hold", report)
	}
	if want := "Scheduler dispatch held: an install of gt is pending"; !strings.Contains(out.String(), want) {
		t.Errorf("daemon output = %q, want the install hold narrated (%q)", out.String(), want)
	}
}

func TestDropRigHeldBeads_RigEstopRemovesOnlyThatRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	plan := capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{
			{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"},
			{ID: "ctx-2", WorkBeadID: "om-b", TargetRig: "om"},
		},
		Skipped: 1,
	}

	r := &runner{}
	got := r.dropRigHeldBeads(townRoot, plan)

	if len(got.ToDispatch) != 1 || got.ToDispatch[0].WorkBeadID != "om-b" {
		t.Errorf("ToDispatch = %+v, want only om-b", got.ToDispatch)
	}
	if got.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (held bead counted as skipped, not failed)", got.Skipped)
	}
}

func TestPlanSkipsRigHeldHeadOfQueueBeforeBatching(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-10-22T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ready := []capacity.PendingBead{
		{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"},
		{ID: "ctx-2", WorkBeadID: "om-b", TargetRig: "om"},
	}

	r := &runner{}
	plan := r.planDispatch(townRoot, 1, 1, ready)

	if len(plan.ToDispatch) != 1 || plan.ToDispatch[0].WorkBeadID != "om-b" {
		t.Fatalf("ToDispatch = %+v, want only om-b: a held head-of-queue bead must not use up the batch", plan.ToDispatch)
	}
	if plan.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the held bead)", plan.Skipped)
	}
}

// When the hold covers every ready bead, the plan must say so: "none" would
// tell the operator nothing is queued.
func TestPlanReportsRigHeldWhenEveryReadyBeadIsHeld(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-10-22T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ready := []capacity.PendingBead{
		{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"},
		{ID: "ctx-2", WorkBeadID: "gt-b", TargetRig: "gastown"},
	}

	r := &runner{}
	plan := r.planDispatch(townRoot, 2, 2, ready)

	if len(plan.ToDispatch) != 0 || plan.Skipped != 2 || plan.Reason != ReasonRigHeld {
		t.Fatalf("plan = %+v, want nothing dispatched, skipped=2, reason=%s", plan, ReasonRigHeld)
	}

	// No ready beads at all is still "none": the reason is for held beads only.
	if none := r.planDispatch(townRoot, 2, 2, nil); none.Reason != "none" || none.Skipped != 0 {
		t.Fatalf("empty plan = %+v, want reason=none, skipped=0", none)
	}
}

// A bead whose last sling failed at session start leaves the plan until its
// backoff elapses, and the beads behind it still dispatch: the resting bead is
// counted as skipped, not failed, so its sling context stays queued for the
// first run after the window (gt-811f6).
func TestPlanDropsBeadRestingAfterAFailedStartup(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := dispatch.RecordStartupFailure(townRoot, "gt-a", "startup blocked: trust dialog"); err != nil {
		t.Fatal(err)
	}
	ready := []capacity.PendingBead{
		{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"},
		{ID: "ctx-2", WorkBeadID: "gt-b", TargetRig: "gastown"},
	}

	r := &runner{}
	plan := r.planDispatch(townRoot, 2, 2, ready)

	if len(plan.ToDispatch) != 1 || plan.ToDispatch[0].WorkBeadID != "gt-b" {
		t.Fatalf("ToDispatch = %+v, want only gt-b: an unrelated bead is unaffected", plan.ToDispatch)
	}
	if plan.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the resting bead)", plan.Skipped)
	}

	// When every ready bead is resting, the plan says so: "none" would tell
	// the operator nothing is queued.
	only := r.planDispatch(townRoot, 2, 2, ready[:1])
	if len(only.ToDispatch) != 0 || only.Skipped != 1 || only.Reason != ReasonStartupBackoff {
		t.Fatalf("all-resting plan = %+v, want nothing dispatched, skipped=1, reason=%s", only, ReasonStartupBackoff)
	}
}

// A failure record nobody can read is dispatched as before: the backoff is a
// throttle, not a safety hold, so a corrupt record must not park a bead for
// good (gt-811f6).
func TestPlanDispatchesABeadWhoseStartupRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, ".runtime", "sling-startup-failures")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gt-a.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	ready := []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"}}

	plan := (&runner{}).planDispatch(townRoot, 1, 1, ready)

	if len(plan.ToDispatch) != 1 || plan.Skipped != 0 {
		t.Fatalf("plan = %+v, want the bead dispatched despite the unreadable record", plan)
	}
}

// A resting bead is skipped by the dispatcher rather than failed, so the
// operator first learns of it from the run's error stream: the line names the
// bead, the window and the failure behind it (gt-811f6).
func TestPlanNamesARestingBeadOnTheErrorStream(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := dispatch.RecordStartupFailure(townRoot, "gt-a", "startup blocked: trust dialog"); err != nil {
		t.Fatal(err)
	}
	ready := []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"}}

	out := captureTo(func(w io.Writer) {
		r := &runner{opts: Options{ErrOut: w}}
		r.planDispatch(townRoot, 1, 1, ready)
	})

	for _, want := range []string{"gt-a", "gastown", "startup backoff", "trust dialog"} {
		if !strings.Contains(out, want) {
			t.Errorf("skip line %q lacks %q", out, want)
		}
	}
}

func TestPrintDispatchNoOpReportsRigHeld(t *testing.T) {
	t.Parallel()
	r := &runner{}
	out := captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDispatchNoOp(capacity.DispatchReport{Reason: ReasonRigHeld, Skipped: 2}, Seats{})
	})
	if !strings.Contains(out, "2 ready bead(s) held by a rig ESTOP") {
		t.Fatalf("rig-held output = %q", out)
	}

	out = captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDryRunPlanFor(capacity.DispatchPlan{Skipped: 2, Reason: ReasonRigHeld}, Seats{Max: 2, Free: 2}, 5)
	})
	if !strings.Contains(out, "2 ready bead(s) held by a rig ESTOP") {
		t.Fatalf("dry-run rig-held output = %q", out)
	}
}

func TestValidateDryRunDropsRigHeldBeads(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-10-22T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &runner{}
	plan := r.validateDryRun(townRoot, capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"}},
		Reason:     "ready",
	})

	if len(plan.ToDispatch) != 0 || plan.Skipped != 1 {
		t.Fatalf("validated plan = %+v, want the held bead dropped and counted as skipped", plan)
	}
}

func TestValidateDryRunMarksAllInvalidAsValidation(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"testrig": {BeadsConfig: &config.BeadsConfig{Prefix: "gt"}},
		},
	})

	r := &runner{}
	plan := r.validateDryRun(townRoot, capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "hq-one", TargetRig: "testrig"}},
		Reason:     "ready",
	})

	if len(plan.ToDispatch) != 0 || plan.Skipped != 1 || plan.Reason != "validation" {
		t.Fatalf("validated plan = %+v, want no dispatch, skipped=1, reason=validation", plan)
	}
}

func TestPrintDryRunPlanUsesCapacitySnapshot(t *testing.T) {
	t.Parallel()
	r := &runner{opts: Options{}}
	out := captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDryRunPlanFor(capacity.DispatchPlan{
			ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "gt-one", TargetRig: "gastown"}},
			Skipped:    2,
			Reason:     "capacity",
		}, Seats{
			Max:             2,
			Working:         1,
			RecoveryBlocked: 1,
			Reservations:    0,
			ReusableIdle:    3,
			Parked:          4,
			PendingMR:       2,
			Free:            0,
		}, 5)
	})
	for _, want := range []string{"0 free of 2", "working: 1", "recovery_blocked: 1", "reusable_idle: 3", "parked: 4", "pending_mr: 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output %q missing %q", out, want)
		}
	}
}

func TestPrintDryRunPlanValidationReasonNotCapacity(t *testing.T) {
	t.Parallel()
	r := &runner{}
	out := captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDryRunPlanFor(capacity.DispatchPlan{
			Skipped: 2,
			Reason:  "validation",
		}, Seats{Max: 2, Free: 2}, 5)
	})
	if !strings.Contains(out, "validation failed for 2 candidate") {
		t.Fatalf("dry-run output %q missing validation reason", out)
	}
	if strings.Contains(out, "No capacity") {
		t.Fatalf("dry-run output %q should not report capacity for validation failures", out)
	}
}

func TestPrintDispatchNoOpReportsExplicitReason(t *testing.T) {
	t.Parallel()
	r := &runner{}
	out := captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDispatchNoOp(capacity.DispatchReport{Reason: "none"}, Seats{})
	})
	if !strings.Contains(out, "No ready beads scheduled for dispatch") {
		t.Fatalf("none output = %q", out)
	}

	out = captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDispatchNoOp(capacity.DispatchReport{Reason: "validation", Skipped: 1}, Seats{})
	})
	if !strings.Contains(out, "No dispatchable beads") || !strings.Contains(out, "validation") {
		t.Fatalf("validation output = %q", out)
	}
}
