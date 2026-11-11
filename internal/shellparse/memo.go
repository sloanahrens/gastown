package shellparse

import (
	"slices"
	"sync"
)

// The guards that read a command (the bd close guard, the heredoc stripper, the
// substitution reader, the dangerous-command checks) each call Parse on the same
// text, or on texts that differ by a trim, so one command is parsed several times
// in a row. The parse is the expensive part of a guard: parsing a command costs
// far more than anything done with the result. parseMemo keeps the most recent
// results, so the repeats are served from memory.
//
// The memo is bounded in entries and in the size of a cached command, and drops
// everything when it fills: the repeats it serves come back to back, so a
// generation scheme would add nothing. A caller gets its own copy of the Script,
// deep, so it may edit what it receives without another caller seeing it.

const (
	memoMaxEntries = 512
	memoMaxCommand = 16 << 10 // bytes; a longer command is parsed every time
)

type memoResult struct {
	script Script
	err    error
}

type parseMemo struct {
	mu      sync.Mutex
	results map[string]memoResult
}

// parses is the process-wide memo of Parse.
var parses = &parseMemo{results: map[string]memoResult{}}

// parse returns the result for command, running do on a miss.
func (m *parseMemo) parse(command string, do func(string) (Script, error)) (Script, error) {
	if len(command) > memoMaxCommand {
		return do(command)
	}
	m.mu.Lock()
	r, ok := m.results[command]
	m.mu.Unlock()
	if ok {
		return cloneScript(r.script), r.err
	}
	script, err := do(command)
	m.mu.Lock()
	if len(m.results) >= memoMaxEntries {
		clear(m.results)
	}
	m.results[command] = memoResult{script: cloneScript(script), err: err}
	m.mu.Unlock()
	return script, err
}

// cloneScript copies s down to every slice, keeping a nil slice nil and an empty
// one empty. A field added to the types below that holds a slice, map or pointer
// must be copied here: TestScriptTypesHoldNothingTheCloneSkips fails until it is.
func cloneScript(s Script) Script {
	s.Segments = cloneSlice(s.Segments, cloneSegment)
	s.Subs = cloneSlice(s.Subs, cloneScript)
	return s
}

func cloneSegment(g Segment) Segment {
	g.Words = cloneSlice(g.Words, cloneWord)
	g.Assigns = cloneSlice(g.Assigns, cloneWord)
	g.Redirs = cloneSlice(g.Redirs, cloneRedirection)
	g.Heredocs = slices.Clone(g.Heredocs)
	g.Subs = cloneSlice(g.Subs, cloneScript)
	g.Shells = slices.Clone(g.Shells)
	return g
}

func cloneWord(w Word) Word {
	w.ProcSubs = cloneSlice(w.ProcSubs, func(p ProcSubst) ProcSubst {
		p.Script = cloneScript(p.Script)
		return p
	})
	return w
}

func cloneRedirection(r Redirection) Redirection {
	r.Word = cloneWord(r.Word)
	return r
}

// cloneSlice maps in over each element, returning nil for a nil slice.
func cloneSlice[T any](in []T, one func(T) T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	for i := range in {
		out[i] = one(in[i])
	}
	return out
}
