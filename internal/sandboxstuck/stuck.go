// Package sandboxstuck names sandboxes left outside Ready or Running.
package sandboxstuck

import (
	"sort"
	"strings"
	"time"
)

const After = 30 * time.Minute

type Sandbox struct {
	Name  string
	Phase string
}

type Finding struct {
	Name  string
	Phase string
	Age   time.Duration
}

// Tracker remembers when each sandbox entered its current phase; its zero value is ready.
type Tracker struct{ since map[string]reading }

type reading struct {
	phase string
	since time.Time
}

// Observe records the listing as read at now.
func (t *Tracker) Observe(listing []Sandbox, now time.Time) {
	next := make(map[string]reading, len(listing))
	for _, sandbox := range listing {
		name := strings.TrimSpace(sandbox.Name)
		if name == "" || ignoredPhase(sandbox.Phase) {
			continue
		}
		prev, ok := t.since[name]
		if !ok || prev.phase != sandbox.Phase {
			prev = reading{phase: sandbox.Phase, since: now}
		} else if now.Before(prev.since) {
			prev.since = now
		}
		next[name] = prev
	}
	t.since = next
}

// Stuck returns the sandboxes in their phase for After or longer, sorted by name.
func (t *Tracker) Stuck(now time.Time) []Finding {
	var findings []Finding
	for name, read := range t.since {
		if age := now.Sub(read.since); age >= After {
			findings = append(findings, Finding{Name: name, Phase: read.phase, Age: age})
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Name < findings[j].Name })
	return findings
}

func ignoredPhase(phase string) bool {
	phase = strings.ToLower(strings.TrimSpace(phase))
	return phase == "" || phase == "ready" || phase == "running" || phase == "error" || phase == "failed"
}
