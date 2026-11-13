package session

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
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
	note string
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

// traceNoteMax is the most of a note the summary keeps. A note is text from
// outside the trace, such as a gateway's condition message, and the summary is
// one log line: a long message is cut rather than allowed to flood the tick log.
const traceNoteMax = 200

// Note attaches text to the phase recorded last, which the summary shows in
// parentheses after that phase's duration. The text is localized to one line
// first, because the summary is one log line and the text can come from a
// gateway. It is a no-op for a nil trace, a note with no visible text, or a
// trace with no phase yet, so a wait with nothing to say adds nothing to the log.
func (t *SandboxTrace) Note(note string) {
	if t == nil || len(t.phases) == 0 {
		return
	}
	note = noteLine(note)
	if note == "" {
		return
	}
	t.phases[len(t.phases)-1].note = note
}

// ansiCSI matches a terminal control sequence: the escape byte, its parameter
// and intermediate bytes, then its final byte. Gateway text can carry color,
// and the summary's reader is a terminal that would act on it.
var ansiCSI = regexp.MustCompile(`\x1b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]`)

// noteLine is note as the summary can carry it: one line, its CSI escape
// sequences (ansiCSI) removed whole and its whitespace runs and other control
// characters, which include the lead byte of any other escape sequence,
// collapsed to a single space, cut to traceNoteMax runes and marked with an
// ellipsis when longer.
func noteLine(note string) string {
	note = ansiCSI.ReplaceAllString(note, "")
	note = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, note)
	note = strings.Join(strings.Fields(note), " ")
	if r := []rune(note); len(r) > traceNoteMax {
		return string(r[:traceNoteMax]) + " …"
	}
	return note
}

func (t *SandboxTrace) record(name string, from time.Time) {
	end := t.now()
	t.phases = append(t.phases, tracePhase{name: name, took: end.Sub(from)})
	t.last = end
}

// Breakdown is a trace as numbers: the phases that were named, the time no
// phase named and the total since the trace began. Named plus Other is the
// total by construction, so the property worth asserting is how small Other is.
type Breakdown struct {
	Phases []NamedPhase
	Other  time.Duration
	Total  time.Duration
}

// NamedPhase is one phase of a Breakdown.
type NamedPhase struct {
	Name string
	Took time.Duration
}

// Named is the time the named phases account for.
func (b Breakdown) Named() time.Duration {
	var sum time.Duration
	for _, p := range b.Phases {
		sum += p.Took
	}
	return sum
}

// Took is the time of the first phase called name, 0 when there is none.
func (b Breakdown) Took(name string) time.Duration {
	for _, p := range b.Phases {
		if p.Name == name {
			return p.Took
		}
	}
	return 0
}

// Breakdown reads the trace as numbers. A nil trace has none.
func (t *SandboxTrace) Breakdown() Breakdown {
	if t == nil {
		return Breakdown{}
	}
	b := Breakdown{Total: t.now().Sub(t.began)}
	for _, p := range t.phases {
		b.Phases = append(b.Phases, NamedPhase{Name: p.name, Took: p.took})
	}
	if rest := b.Total - b.Named(); rest > 0 {
		b.Other = rest
	}
	return b
}

// Summary is the trace on one line: each phase with its duration, then the
// total since the trace began. The time no phase named is reported as "other",
// so the phases always add up to the total and a gap is visible rather than
// folded into a neighbor (gt-7nctk). It is "" when no phase ran, which is how
// a sling that never touched a sandbox is left out of the log.
func (t *SandboxTrace) Summary() string {
	if t == nil || len(t.phases) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t.phases)+2)
	var timed time.Duration
	for _, p := range t.phases {
		timed += p.took
		part := fmt.Sprintf("%s %s", p.name, p.took.Round(time.Millisecond))
		if p.note != "" {
			part += " (" + p.note + ")"
		}
		parts = append(parts, part)
	}
	total := t.now().Sub(t.began)
	if rest := total - timed; rest > 0 {
		parts = append(parts, fmt.Sprintf("other %s", rest.Round(time.Millisecond)))
	}
	parts = append(parts, fmt.Sprintf("total %s", total.Round(time.Millisecond)))
	return strings.Join(parts, ", ")
}
