package notify

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/nudge/deliver"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Nudger delivers one nudge. CLI.Nudge hands its nudges to one when set.
type Nudger interface {
	Nudge(ctx context.Context, target, message string) error
}

// TownNudger delivers a nudge in-process, exactly as `gt nudge <target>
// <message>` run in Dir with Env does (internal/nudge/deliver): the town and
// the sender resolve from Dir and Env's identity variables, delivery waits
// for the target to go idle and falls back to its queue, and a context that
// ends mid-delivery stops it and is reported as the context's error, where
// the CLI killed the gt process.
//
// Channel targets (channel:<name>) are refused; they need gt nudge.
type TownNudger struct {
	// Dir is the directory the nudge is sent from; empty means the caller's
	// working directory.
	Dir string
	// Env returns the environment whose identity variables (GT_ROLE, GT_RIG,
	// GT_CREW, GT_POLECAT) attribute the nudge; nil reads the caller's.
	Env func() []string
	// Tmux is the tmux server the target lives on; nil is tmux.NewTmux().
	Tmux deliver.Tmux

	// town builds the delivery for a town root; nil is newTown.
	town func(townRoot string) *deliver.Town
}

func (n *TownNudger) newTown(townRoot string) *deliver.Town {
	if n.town != nil {
		return n.town(townRoot)
	}
	var t deliver.Tmux = n.Tmux
	if t == nil {
		t = tmux.NewTmux()
	}
	return &deliver.Town{Delivery: deliver.New(t, townRoot)}
}

// getenv reads one variable from Env, or from the process without it.
func (n *TownNudger) getenv() func(string) string {
	if n.Env == nil {
		return os.Getenv
	}
	env := n.Env()
	return func(key string) string {
		// The last assignment wins, as for a process environment.
		for i := len(env) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
				return v
			}
		}
		return ""
	}
}

// Nudge delivers message to target.
func (n *TownNudger) Nudge(ctx context.Context, target, message string) error {
	if err := ValidateNudge(target, message); err != nil {
		return err
	}
	if err := contextErr(ctx); err != nil {
		return fmt.Errorf("gt nudge: %w", err)
	}
	dir := n.Dir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return fmt.Errorf("gt nudge: %w", err)
		}
	}
	townRoot, _ := workspace.Find(dir)
	sender := deliver.Sender(dir, townRoot, n.getenv())
	town := n.newTown(townRoot)

	// Deliver on its own goroutine so a tmux call that hangs past the
	// deadline cannot hold the caller: the delivery checks ctx before every
	// keystroke and queue write, so once ctx ends it does nothing more.
	done := make(chan error, 1)
	go func() { done <- town.Nudge(ctx, target, message, sender) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("gt nudge: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("gt nudge: %w", ctx.Err())
	}
}

var _ Nudger = (*TownNudger)(nil)
