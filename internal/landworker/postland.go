package landworker

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/land"
)

// PostLand is one landing whose post-landing command should run.
type PostLand struct {
	BeadID string
	Commit string
	Target string
}

// PostLandResult is one run of the post-landing command. Err means it could
// not run at all; ExitCode is the verdict otherwise.
type PostLandResult struct {
	ExitCode int
	Tail     string
	// Packages are the Go packages the run reported as ok or FAIL.
	Packages []land.PackageResult
	Err      error
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
			p.logf("%s (%s) supersedes queued %s (%s); one run at the newest commit", pl.BeadID, short(pl.Commit), p.pending.BeadID, short(p.pending.Commit))
		}
		p.pending = &pl
		p.mu.Unlock()
		return
	}
	p.running = true
	p.wg.Add(1)
	p.mu.Unlock()
	go p.loop(ctx, pl)
}

// Wait blocks until no run is in flight or queued.
func (p *PostLandRunner) Wait() { p.wg.Wait() }

func (p *PostLandRunner) loop(ctx context.Context, pl PostLand) {
	defer p.wg.Done()
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
	p.logf("running %q at %s for %s", cmd, short(pl.Commit), pl.BeadID)
	res := p.Run(ctx, cmd, pl)
	switch {
	case res.Err != nil:
		if ctx.Err() == nil {
			p.logf("WARNING could not run %q at %s for %s: %v", cmd, short(pl.Commit), pl.BeadID, res.Err)
		}
	case res.ExitCode != 0:
		tail := lastLines(res.Tail, postLandTailLines)
		p.logf("RED: %q exited %d at %s (landed by %s)\n%s", cmd, res.ExitCode, short(pl.Commit), pl.BeadID, tail)
		msg := fmt.Sprintf("post-landing slow tier RED at %s: %s", pl.Commit, "exit "+fmt.Sprint(res.ExitCode)+", last lines:\n"+tail)
		if p.Beads != nil {
			if err := p.Beads.AddComment(pl.BeadID, msg); err != nil {
				p.logf("commenting on %s: %v", pl.BeadID, err)
			}
		}
		if p.OnRed != nil {
			p.OnRed(ctx, cmd, pl, res)
		}
	default:
		p.logf("green at %s (%s)", short(pl.Commit), pl.BeadID)
		if p.OnGreen != nil {
			p.OnGreen(ctx, cmd, pl, res)
		}
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
