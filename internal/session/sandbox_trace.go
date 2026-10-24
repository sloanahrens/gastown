package session

import (
	"fmt"
	"strings"
	"time"
)

// SandboxTrace times the phases of one sling's sandbox session start, so the
// spec dispatcher's tick log can say where a slow sling spent its time
// (gt-0uh64). A nil *SandboxTrace times nothing, so callers never guard it.
//
// A trace belongs to one sling and is not safe for concurrent use.
type SandboxTrace struct {
	now    func() time.Time
	began  time.Time
	last   time.Time
	phases []tracePhase
}

type tracePhase struct {
	name string
	took time.Duration
}

// NewSandboxTrace starts a trace whose total runs from now, read through now.
func NewSandboxTrace(now func() time.Time) *SandboxTrace {
	t := now()
	return &SandboxTrace{now: now, began: t, last: t}
}

// Phase runs fn and records how long it took under name. A phase that fails
// is recorded like one that succeeds: a refused start still shows the time it
// spent before the refusal.
func (t *SandboxTrace) Phase(name string, fn func() error) error {
	if t == nil {
		return fn()
	}
	start := t.now()
	err := fn()
	t.record(name, start)
	return err
}

// Mark records the time since the last phase ended under name. It covers the
// work between two timed phases that no Phase wraps, such as the agent's
// launch after the sandbox is filled.
func (t *SandboxTrace) Mark(name string) {
	if t == nil {
		return
	}
	t.record(name, t.last)
}

func (t *SandboxTrace) record(name string, from time.Time) {
	end := t.now()
	t.phases = append(t.phases, tracePhase{name: name, took: end.Sub(from)})
	t.last = end
}

// Summary is the trace on one line: each phase with its duration, then the
// total since the trace began. It is "" when no phase ran, which is how a
// sling that never touched a sandbox is left out of the log.
func (t *SandboxTrace) Summary() string {
	if t == nil || len(t.phases) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t.phases)+1)
	for _, p := range t.phases {
		parts = append(parts, fmt.Sprintf("%s %s", p.name, p.took.Round(time.Millisecond)))
	}
	parts = append(parts, fmt.Sprintf("total %s", t.now().Sub(t.began).Round(time.Millisecond)))
	return strings.Join(parts, ", ")
}
