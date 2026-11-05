package landworker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/land"
)

// PostLand is one commit on the target whose post-landing command should
// run: a landing, or a direct push the worker noticed.
type PostLand struct {
	// BeadID is the landed work bead; "" for a direct push.
	BeadID string
	Commit string
	Target string
	// Direct marks a commit that reached the target without a landing; From
	// is the tip the worker last saw before it, so From..Commit is the range
	// a red run blames.
	Direct bool
	From   string
}

// PostLandResult is one run of the post-landing command. Err means it could
// not run at all; ExitCode is the verdict otherwise.
type PostLandResult struct {
	ExitCode int
	Tail     string
	// Packages are the Go packages the run reported as ok or FAIL.
	Packages []land.PackageResult
	// ShellFailures are the scripts the run's tier sweep reported red, in the
	// order its summary line named them. A red run that named no Go package
	// still names these, and red-main attributes the landing against them
	// (gt-40so9).
	ShellFailures []string
	// LogPath is the run's full output on disk, "" when none was kept.
	LogPath string
	Err     error
}

// PostLandTrigger starts the post-landing verdict for a landing, and reads the
// verdicts still waiting on CI once per worker pass.
type PostLandTrigger interface {
	Trigger(ctx context.Context, pl PostLand)
	PollPending(ctx context.Context)
}

// postLandTailLines is how much output a red comment quotes.
const postLandTailLines = 10

// PostLandRunner decides a rig's post-land verdict after each landing. Either
// it runs merge_queue.post_land_command (the slow test tier) asynchronously,
// one at a time: a landing that completes while a run is in flight is
// coalesced, and when the run finishes it runs once more at the newest landed
// commit, whatever number of landings arrived meanwhile. Or, when
// merge_queue.post_land_status_context is set, it reads the landed commit's CI
// status in that context: the landing records a pending verdict and returns,
// and PollPending reads it on a later worker pass. Nothing waits on CI
// (gt-gg9hv.7.1).
//
// A red verdict never blocks anything. OnRed and OnGreen receive every verdict
// on the goroutine that decides it (the runner's, or a worker pass's in status
// mode), so a landing that arrives while they work coalesces like one that
// arrives mid-run; RedMain is the production pair
// (gt-v4ssj.4). An UNKNOWN status verdict calls neither and raises one
// attention item per landed commit.
//
// In status mode a verdict fires before its pending entry is removed and
// saved, so a daemon that stops between the two calls it again for the same
// landing. Each callback is idempotent per rig and landed commit, which is
// what makes that repeat harmless: OnUnknown's record is keyed by rig and
// commit and the attention store replaces one with the same key, so the item
// keeps its first_seen and is raised once; OnGreen and OnRed are
// RedMain.Green and RedMain.Red, which records the same main state at the same
// commit and key their artifacts by rig and package and by culprit bead, so an
// open red-main bead is commented rather than filed a second time and an open
// revert is found rather than built again. A repeat re-runs the red reruns, and
// adds no duplicate bead, revert or attention item.
type PostLandRunner struct {
	Rig string
	// Command returns the command to run; "" leaves the command mode off.
	// It is read at every trigger and every run, so a settings change applies
	// to the next landing.
	Command func() string
	// StatusContext returns the commit-status context the verdict comes
	// from; "" leaves status mode off. When both it and Command are set,
	// status mode wins. It is read at every trigger and every run too.
	StatusContext func() string
	// Status reads the landed commit's combined status; nil disables status
	// mode. It is read at most once per PostLandStatusPollInterval per pending
	// commit.
	Status func(ctx context.Context, pl PostLand) (*forgejo.CombinedStatus, error)
	// State holds the pending status-mode verdicts, so a restarted daemon
	// resumes them; nil disables status mode.
	State MainStateStore
	// OnUnknown, when set, receives the record for a landed commit whose
	// status context never reached a verdict before Timeout. It is called
	// once per landed commit.
	OnUnknown func(ctx context.Context, u PostLandUnknown)
	// OnStoreFailure, when set, receives the rig's pending-verdict store
	// failure once PostLandStatusStoreFailureAfter passes running have failed
	// to read or write it, and nil on the pass that reaches the store again,
	// which clears the attention item. A failure nobody is told about is the
	// safety net going blind.
	OnStoreFailure func(ctx context.Context, f *PostLandStoreFailure)
	// readTimeoutOverride bounds one status read in place of
	// PostLandStatusReadTimeout, which production uses. A test shortens it so
	// a stalled read is cut off without waiting out the real bound.
	readTimeoutOverride time.Duration
	// Clock is the time source status mode reads its deadlines by; nil means
	// the real clock.
	Clock PostLandClock
	// Timeout bounds one pending verdict; 0 means PostLandStatusDefaultTimeout.
	// Production passes merge_queue.post_land_timeout.
	Timeout time.Duration
	// Run runs cmd for pl (throwaway worktree at pl.Commit, container slot).
	Run   func(ctx context.Context, cmd string, pl PostLand) PostLandResult
	Beads interface {
		AddComment(id, text string) error
	}
	Logf    func(format string, args ...any)
	OnRed   func(ctx context.Context, cmd string, pl PostLand, res PostLandResult)
	OnGreen func(ctx context.Context, cmd string, pl PostLand, res PostLandResult)
	// Busy, when set, is called with true when a run starts with none in
	// flight and with false when the last queued run has finished, so the
	// daemon can hold an upgrade restart for it (gt-gb4ij).
	Busy func(busy bool)

	mu      sync.Mutex
	running bool
	pending *PostLand
	wg      sync.WaitGroup

	// statusMu serializes the read-modify-write of the pending verdicts: a
	// landing's Trigger records one while a worker pass polls them, and an
	// unsynchronized pair of loads and saves would drop one.
	statusMu sync.Mutex
	// done holds the landed commits whose verdict or timeout already fired,
	// so a store write that fails does not fire it again on the next pass.
	// An entry leaves when its removal is saved.
	done map[string]bool
	// storeFailures counts the passes running that could not read or write
	// the pending verdicts, and storeFailureRaised says the attention item is
	// up; the next pass that reaches the store clears it.
	storeFailures      int
	storeFailureRaised bool
}

var _ PostLandTrigger = (*PostLandRunner)(nil)

func (p *PostLandRunner) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf("landing_worker: "+p.Rig+": post-land: "+format, args...)
	}
}

func (p *PostLandRunner) command() string {
	if p.Command == nil {
		return ""
	}
	return strings.TrimSpace(p.Command())
}

// enabled reports whether either verdict mode is configured.
func (p *PostLandRunner) enabled() bool {
	return p.statusEnabled() || p.command() != ""
}

// Trigger schedules the verdict for pl and returns at once. In status mode it
// only records the pending verdict; the command mode starts a run.
func (p *PostLandRunner) Trigger(ctx context.Context, pl PostLand) {
	if !p.enabled() {
		return
	}
	if p.statusEnabled() {
		if cmd := p.command(); cmd != "" {
			p.logf("merge_queue.post_land_status_context %q is set, so it decides the verdict and post_land_command %q is not run", p.statusContext(), cmd)
		}
		p.recordPending(ctx, pl, p.statusContext())
		return
	}
	p.mu.Lock()
	if p.running {
		if p.pending != nil {
			p.logf("%s (%s) supersedes queued %s (%s); one run at the newest commit", short(pl.Commit), pl.by(), short(p.pending.Commit), p.pending.by())
		}
		p.pending = &pl
		p.mu.Unlock()
		return
	}
	p.running = true
	p.wg.Add(1)
	p.mu.Unlock()
	if p.Busy != nil {
		p.Busy(true)
	}
	go p.loop(ctx, pl)
}

// Wait blocks until no run is in flight or queued.
func (p *PostLandRunner) Wait() { p.wg.Wait() }

func (p *PostLandRunner) loop(ctx context.Context, pl PostLand) {
	defer p.wg.Done()
	if p.Busy != nil {
		defer p.Busy(false)
	}
	for {
		p.runOne(ctx, pl)
		p.mu.Lock()
		if p.pending == nil || ctx.Err() != nil {
			p.running, p.pending = false, nil
			p.mu.Unlock()
			return
		}
		pl, p.pending = *p.pending, nil
		p.mu.Unlock()
	}
}

func (p *PostLandRunner) runOne(ctx context.Context, pl PostLand) {
	cmd := p.command()
	if cmd == "" || ctx.Err() != nil {
		return
	}
	p.logf("running %q at %s (%s)", cmd, short(pl.Commit), pl.by())
	res := p.Run(ctx, cmd, pl)
	if ctx.Err() != nil {
		// A run the daemon cut short is no verdict, and a killed run must
		// not read as red main (gt-f2voh). No verdict is recorded, so the
		// restarted worker runs again from the last verdict to the tip.
		p.logf("%s (%s) superseded: the daemon stopped before %q reached a verdict; the restarted worker reruns it at the untested tip%s", short(pl.Commit), pl.by(), cmd, fullLog(res.LogPath))
		return
	}
	switch {
	case res.Err != nil:
		p.logf("WARNING could not run %q at %s (%s): %v", cmd, short(pl.Commit), pl.by(), res.Err)
	case res.ExitCode != 0:
		tail := lastLines(res.Tail, postLandTailLines)
		failing := strings.Join(failingPackages(res), ", ")
		if failing == "" {
			failing = redMainNoPackage
		}
		p.logf("RED: %q exited %d at %s (%s); failing: %s%s\n%s", cmd, res.ExitCode, short(pl.Commit), pl.by(), failing, fullLog(res.LogPath), tail)
		msg := fmt.Sprintf("post-landing check RED at %s: exit %d, failing: %s%s; last lines:\n%s", pl.Commit, res.ExitCode, failing, fullLog(res.LogPath), tail)
		if p.Beads != nil && pl.BeadID != "" {
			if err := p.Beads.AddComment(pl.BeadID, msg); err != nil {
				p.logf("commenting on %s: %v", pl.BeadID, err)
			}
		}
		if p.OnRed != nil {
			p.OnRed(ctx, cmd, pl, res)
		}
	default:
		p.logf("green at %s (%s)", short(pl.Commit), pl.by())
		if p.OnGreen != nil {
			p.OnGreen(ctx, cmd, pl, res)
		}
	}
}

// fullLog is "; full log <path>" for a run that kept its output, else "".
func fullLog(path string) string {
	if path == "" {
		return ""
	}
	return "; full log " + path
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
