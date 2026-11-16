package omlessons

import (
	"fmt"
	"strconv"
)

// ProposeMinFindings is how many findings a class needs in the window before a
// proposal drafts a directive edit for it; fewer is too few to say.
const ProposeMinFindings = 5

// Proposal is one class's entry in the proposals pass: a draft for a class with
// enough findings in the window, or a too-few entry for one without.
type Proposal struct {
	ID   string
	Name string
	// TooFew is true when the class has fewer than ProposeMinFindings findings
	// in the window. A too-few entry carries no Coverage, so it reads as no draft.
	TooFew bool
	// Coverage names the class's rule coverage: covered by a rule the directive
	// carries, or a gap. It is empty when TooFew.
	Coverage string
	Current  ClassCounts
	Prior    ClassCounts
	// Examples are the class's example bead ids, as ExampleBeads holds them.
	Examples []string
}

// Proposals returns one Proposal per class of the report, in the report's
// class order. rules is the set of directive rule numbers, as their decimal
// text; a nil or empty set reads every class as a gap, so callers build it from
// the directive files and refuse an empty one.
func Proposals(report Report, rules map[string]bool) []Proposal {
	out := make([]Proposal, 0, len(report.Classes))
	for _, c := range report.Classes {
		p := Proposal{
			ID:       c.ID,
			Name:     c.Name,
			Current:  c.Current,
			Prior:    c.Prior,
			Examples: c.ExampleBeads,
		}
		if c.Current.Findings < ProposeMinFindings {
			p.TooFew = true
		} else {
			p.Coverage = coverage(c, rules)
		}
		out = append(out, p)
	}
	return out
}

// coverage names a class's coverage. A class whose rule is in the directive is
// violated-anyway when it still appears in the window. A class with no rule, or
// whose rule number the directive does not carry, is a gap.
func coverage(c ClassRow, rules map[string]bool) string {
	switch {
	case c.Rule == 0:
		return "gap: no numbered rule covers it"
	case !rules[strconv.Itoa(c.Rule)]:
		return fmt.Sprintf("gap: the table names rule #%d and the directive has no rule #%d", c.Rule, c.Rule)
	}
	return fmt.Sprintf("violated-anyway: rule #%d covers it and it still appears", c.Rule)
}
