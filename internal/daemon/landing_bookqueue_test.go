package daemon

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// TestDeferredBookkeepingWiring: a listed rig's Lander gets the deferred flag
// and its Worker the queue, while an unlisted or unconfigured rig gets neither
// (gt-jz0tp6).
func TestDeferredBookkeepingWiring(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		rig    string
		cfg    *config.LandingWorkerConfig
		wantOn bool
	}{
		{
			name: "rig listed",
			rig:  "gastown",
			cfg: &config.LandingWorkerConfig{DeferredBookkeeping: &config.DeferredBookkeepingConfig{
				Rigs: []string{"gastown", "longeye"},
			}},
			wantOn: true,
		},
		{
			name: "rig not listed",
			rig:  "gastown",
			cfg: &config.LandingWorkerConfig{DeferredBookkeeping: &config.DeferredBookkeepingConfig{
				Rigs: []string{"longeye"},
			}},
		},
		{
			name: "no deferred_bookkeeping block",
			rig:  "gastown",
			cfg:  &config.LandingWorkerConfig{},
		},
		{
			name: "no config",
			rig:  "gastown",
			cfg:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := &land.Lander{Repo: "/sentinel/repo"}
			worker := &landworker.Worker{Rig: "gastown"}

			d.wireBookQueue(tc.rig, tc.cfg, lander, worker)

			if !tc.wantOn {
				if lander.DeferBookkeeping {
					t.Fatalf("an off rig's Lander was given DeferBookkeeping; want the inline path")
				}
				if worker.Books != nil {
					t.Fatalf("an off rig's Worker was given a BookQueue; want none")
				}
				if got := logs.String(); got != "" {
					t.Fatalf("an off rig logged %q; want nothing", got)
				}
				return
			}
			if !lander.DeferBookkeeping {
				t.Fatalf("the listed rig's Lander was not given DeferBookkeeping")
			}
			if worker.Books == nil {
				t.Fatalf("the listed rig's Worker was given no BookQueue")
			}
			if worker.Books.Logf == nil {
				t.Fatalf("the listed rig's BookQueue has no Logf, so a panicked job would be silent")
			}
			worker.Books.Logf("the bookkeeping job for %s panicked", "gt-abc")
			if got := logs.String(); !strings.Contains(got, "the bookkeeping job for gt-abc panicked") {
				t.Fatalf("the BookQueue's Logf wrote %q to the daemon log; want the job's message", got)
			}
		})
	}
}

// builtWorker runs the daemon's production construction (newRigLandingWorker)
// for rig "gastown" with deferred bookkeeping listed for rigs, and returns the
// worker it built.
func builtWorker(t *testing.T, rigs ...string) *landworker.Worker {
	t.Helper()
	townRoot := t.TempDir()
	promoteRigConfig(t, townRoot, "gastown", filepath.Join(t.TempDir(), "promote-gastown.key"))
	d := newForgejoPromoteDaemon(t, townRoot, "")
	d.patrolConfig.Patrols.LandingWorker.DeferredBookkeeping = &config.DeferredBookkeepingConfig{Rigs: rigs}
	w, err := d.newRigLandingWorker("gastown")
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	return w
}

// TestDeferredBookkeepingBuilt: the worker the daemon builds for a rig the
// switch lists carries the queue and a Lander that defers, while a rig it does
// not list carries neither (gt-libhdq). It drives newRigLandingWorker, the
// production construction path, so deleting the wiring turns the listed case
// red.
func TestDeferredBookkeepingBuilt(t *testing.T) {
	t.Parallel()
	landerOf := func(t *testing.T, w *landworker.Worker) *land.Lander {
		t.Helper()
		lander, ok := w.Lander.(*land.Lander)
		if !ok {
			t.Fatalf("Lander = %T, want *land.Lander", w.Lander)
		}
		return lander
	}

	t.Run("a listed rig's built worker has the queue", func(t *testing.T) {
		t.Parallel()
		w := builtWorker(t, "gastown", "longeye")
		if w.Books == nil {
			t.Fatal("the worker the daemon built for a listed rig has no BookQueue; its bookkeeping runs inline")
		}
		if !landerOf(t, w).DeferBookkeeping {
			t.Error("the listed rig's Lander keeps the inline path; the queue would never be fed")
		}
	})

	t.Run("an unlisted rig's built worker has none", func(t *testing.T) {
		t.Parallel()
		w := builtWorker(t, "longeye")
		if w.Books != nil {
			t.Fatal("the worker the daemon built for an unlisted rig carries a BookQueue; want the inline path")
		}
		if landerOf(t, w).DeferBookkeeping {
			t.Error("the unlisted rig's Lander was given DeferBookkeeping")
		}
	})
}

// wiredBookQueue builds a daemon whose listed rig has its landing worker wired
// the way the daemon wires it, with the daemon's log discarded.
func wiredBookQueue(t *testing.T, cfg *config.LandingWorkerConfig) (*Daemon, *landworker.Worker) {
	t.Helper()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	worker := &landworker.Worker{}
	d.wireBookQueue("gastown", cfg, &land.Lander{}, worker)
	if worker.Books == nil {
		t.Fatal("the listed rig's Worker was given no BookQueue")
	}
	return d, worker
}

// TestDeferredBookkeepingHold: the queue's first job takes a landing pass slot
// and its last job gives it back, so a hold taken while a queued job runs waits
// for it to end, and a job added under a hold is counted as a pass in flight so
// the hold waits for that job too (gt-k7glkk).
func TestDeferredBookkeepingHold(t *testing.T) {
	t.Parallel()
	cfg := &config.LandingWorkerConfig{DeferredBookkeeping: &config.DeferredBookkeepingConfig{
		Rigs: []string{"gastown"},
	}}

	t.Run("a hold taken while a job runs is idle only after it ends", func(t *testing.T) {
		t.Parallel()
		d, worker := wiredBookQueue(t, cfg)

		ctx := t.Context()
		started, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseJob := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseJob) // unblock the drain however the test ends
		worker.Books.Add("gt-abc", func() {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		waitForSignal(t, started, "the queued job starting")

		if got := d.landingPasses.Load(); got != 1 {
			t.Fatalf("in-flight count = %d while a queued job runs, want 1", got)
		}
		idle := d.landingHolds.hold(landingHoldUpgradeRestart)
		select {
		case <-idle:
			t.Fatal("the hold woke with the queued job still running")
		default:
		}

		releaseJob()
		waitForSignal(t, idle, "the hold waking after the queued job ended")
		if got := d.landingPasses.Load(); got != 0 {
			t.Errorf("in-flight count = %d after the queue drained, want 0", got)
		}
	})

	t.Run("a job added under a hold is counted", func(t *testing.T) {
		t.Parallel()
		d, worker := wiredBookQueue(t, cfg)

		idle := d.landingHolds.hold(landingHoldRebuildOM)
		ctx := t.Context()
		started, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseJob := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseJob) // unblock the drain however the test ends
		worker.Books.Add("gt-abc", func() {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		waitForSignal(t, started, "the queued job starting under a hold")

		if got := d.landingPasses.Load(); got != 1 {
			t.Fatalf("in-flight count = %d for a job added under a hold, want 1", got)
		}
		select {
		case <-idle:
			t.Fatal("the hold woke with the queued job still running")
		default:
		}

		releaseJob()
		waitForSignal(t, idle, "the hold waking after the queued job ended")
	})
}
