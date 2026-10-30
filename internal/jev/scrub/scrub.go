// Package scrub removes secret-shaped strings and the operator's identity from
// text before it leaves the machine for the hosted Jev service.
//
// A Scrubber is an ordered rule list: each rule replaces one kind of secret
// with a fixed [REDACTED:name] marker and reports how many replacements it
// made, so a caller can log which rules fired without logging what they
// removed. Every rule is linear in the input and none panics, because the text
// being scrubbed is untrusted (a transcript tail, a test log, a diff hunk).
package scrub

import "sort"

// A Rule replaces one kind of secret-shaped text and counts its replacements.
type Rule struct {
	Name  string
	Apply func(text string) (string, int)
}

// A Scrubber applies an ordered rule list to text.
type Scrubber struct {
	rules []Rule
}

// New returns a Scrubber that applies rules in order.
func New(rules ...Rule) *Scrubber {
	return &Scrubber{rules: rules}
}

// Default returns the scrubber the town uses: every rule in DefaultRules.
func Default() *Scrubber {
	return New(DefaultRules()...)
}

// Scrub returns text with every rule applied and a per-rule replacement count.
func (s *Scrubber) Scrub(text string) (string, Report) {
	report := Report{counts: make(map[string]int, len(s.rules))}
	for _, rule := range s.rules {
		if rule.Apply == nil {
			continue
		}
		out, n := rule.Apply(text)
		text = out
		if n > 0 {
			report.counts[rule.Name] += n
		}
	}
	return text, report
}

// A Report counts the replacements a scrubbing made, per rule name.
//
// A Report is read-only to callers: Count and Rules never mutate it, and the
// zero Report counts nothing.
type Report struct {
	counts map[string]int
}

// Count returns the number of replacements rule made.
func (r Report) Count(rule string) int {
	return r.counts[rule]
}

// Total returns the number of replacements over every rule.
func (r Report) Total() int {
	total := 0
	for _, n := range r.counts {
		total += n
	}
	return total
}

// Rules returns the names of the rules that replaced something, sorted.
func (r Report) Rules() []string {
	names := make([]string, 0, len(r.counts))
	for name := range r.counts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// add sums other's counts into r.
func (r *Report) add(other Report) {
	if len(other.counts) == 0 {
		return
	}
	if r.counts == nil {
		r.counts = make(map[string]int, len(other.counts))
	}
	for name, n := range other.counts {
		r.counts[name] += n
	}
}
