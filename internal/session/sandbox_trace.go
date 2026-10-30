package session

import (
	"fmt"
	"strings"
	"time"
)

// SandboxTrace times the phases of one sling, so the spec dispatcher's tick log
// can say where a slow sling spent its time (gt-0uh64, gt-7nctk). A nil
// *SandboxTrace times nothing, so callers never guard it.
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

// Steps is Mark as a callback, for a caller that hands its step timer down a
// call chain so those steps land in this trace; a nil trace yields nil.
func (t *SandboxTrace) Steps() func(name string) {
	if t == nil {
		return nil
	}
	return t.Mark
}

func (t *SandboxTrace) record(name string, from time.Time) {
	end := t.now()
	t.phases = append(t.phases, tracePhase{name: name, took: end.Sub(from)})
	t.last = end
}

// Summary is the trace on one line: each phase with its duration, then the
// total since the trace began. The time no phase named is reported as "other",
// so the phases always add up to the total and a gap is visible rather than
// folded into a neighbour (gt-7nctk). It is "" when no phase ran, which is how
// a sling that never touched a sandbox is left out of the log.
func (t *SandboxTrace) Summary() string {
	if t == nil || len(t.phases) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t.phases)+2)
	var timed time.Duration
	for _, p := range t.phases {
		timed += p.took
		parts = append(parts, fmt.Sprintf("%s %s", p.name, p.took.Round(time.Millisecond)))
	}
	total := t.now().Sub(t.began)
	if rest := total - timed; rest > 0 {
		parts = append(parts, fmt.Sprintf("other %s", rest.Round(time.Millisecond)))
	}
	parts = append(parts, fmt.Sprintf("total %s", total.Round(time.Millisecond)))
	return strings.Join(parts, ", ")
}
