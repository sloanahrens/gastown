package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// namedBackend is a session backend known only by name: the trace wiring reads
// nothing else from it.
type namedBackend struct {
	session.Backend
	name string
}

func (b namedBackend) Name() string { return b.name }

func TestStartSessionWithTraceTimesASandboxLaunch(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000_000, 0)
	trace := session.NewSandboxTrace(func() time.Time { return clock })
	_ = trace.Phase("create", func() error {
		clock = clock.Add(3 * time.Second)
		return nil
	})

	var got polecat.SessionStartOptions
	err := startSessionWithTrace(polecat.SessionStartOptions{Agent: "box"},
		namedBackend{name: session.BackendOpenShell}, trace,
		func(opts polecat.SessionStartOptions) error {
			got = opts
			clock = clock.Add(5 * time.Second)
			return nil
		})

	if err != nil {
		t.Fatalf("start error = %v, want nil", err)
	}
	if got.Trace != trace || got.Agent != "box" {
		t.Errorf("start options = %+v, want the trace carried beside the caller's options", got)
	}
	if want := "create 3s, start 5s, total 8s"; trace.Summary() != want {
		t.Errorf("trace = %q, want %q: the launch is the gap since the last timed phase", trace.Summary(), want)
	}
}

// A start that failed still spent the launch time, and the tick's line is the
// only place that time can appear (gt-csltr).
func TestStartSessionWithTraceTimesAFailedLaunch(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000_000, 0)
	trace := session.NewSandboxTrace(func() time.Time { return clock })
	startErr := errors.New("starting session: sandbox never came up")

	err := startSessionWithTrace(polecat.SessionStartOptions{},
		namedBackend{name: session.BackendOpenShell}, trace,
		func(polecat.SessionStartOptions) error {
			clock = clock.Add(5 * time.Second)
			return startErr
		})

	if !errors.Is(err, startErr) {
		t.Fatalf("start error = %v, want the start's own error", err)
	}
	if want := "start 5s, total 5s"; trace.Summary() != want {
		t.Errorf("trace = %q, want %q: a failed launch is still timed", trace.Summary(), want)
	}
}

func TestStartSessionWithTraceLeavesAHostStartUntimed(t *testing.T) {
	t.Parallel()
	backends := map[string]session.Backend{
		"tmux":       namedBackend{name: session.BackendTmux},
		"no backend": nil,
	}
	for name, backend := range backends {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trace := session.NewSandboxTrace(func() time.Time { return time.Unix(1_000_000, 0) })
			started := false
			err := startSessionWithTrace(polecat.SessionStartOptions{}, backend, trace,
				func(opts polecat.SessionStartOptions) error {
					started = true
					if opts.Trace != nil {
						t.Errorf("opts.Trace = %v, want nil for a host start", opts.Trace)
					}
					return nil
				})

			if err != nil || !started {
				t.Fatalf("start error = %v, started = %v, want the start run", err, started)
			}
			if got := trace.Summary(); got != "" {
				t.Errorf("trace = %q, want empty: a host start adds no line to the tick's log", got)
			}
		})
	}
}
