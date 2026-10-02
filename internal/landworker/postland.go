package landworker

import (
	"context"
	"fmt"
	"strings"
	"sync"

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

// PostLandTrigger starts the post-landing command for a landing.
type PostLandTrigger interface {
	Trigger(ctx context.Context, pl PostLand)
}

// postLandTailLines is how much output a red comment quotes.
const postLandTailLines = 10

// PostLandRunner runs a rig's post-landing command (merge_queue.
// post_land_command, the slow test tier) after each landing, asynchronously
// and one at a time. A landing that completes while a run is in flight is
// coalesced: when the run finishes, the command runs once more at the newest
// landed commit, whatever number of landings arrived meanwhile.
//
// A red run comments on the landed bead and never blocks anything. OnRed and
// OnGreen receive every verdict on the runner's goroutine, so a landing that
// arrives while they work coalesces like one that arrives mid-run; RedMain is
// the production pair (gt-v4ssj.4).
type PostLandRunner struct {
	Rig string
	// Command returns the command to run; "" disables the runner. It is
	// read at every trigger and every run, so a settings change applies to
	// the next landing.
	Command func() string
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

// Trigger schedules the command for pl and returns at once.
func (p *PostLandRunner) Trigger(ctx context.Context, pl PostLand) {
	if p.command() == "" {
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
