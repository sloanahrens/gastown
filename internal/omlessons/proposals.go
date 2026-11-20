package omlessons

import (
	"fmt"
	"strconv"
	"strings"
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

// ProposalDraft returns one proposal's draft text: its rule coverage, its
// counts, three example beads, and the directive edit the overseer writes.
// directivesDir is the directory the edit goes to, named in the Constraints
// line.
//
// The daemon files this text in its weekly bead and `gt land om-lessons
// --propose` prints it, so the two read the same. Callers print the heading
// that names the class themselves; a too-few proposal has no draft and is
// left out before this is called.
func ProposalDraft(p Proposal, directivesDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Coverage: %s\n", p.Coverage)
	fmt.Fprintf(&b, "Counts: %d finding(s) on %d bead(s), %d round(s) burned; prior window %d finding(s) on %d bead(s), %d round(s)\n",
		p.Current.Findings, p.Current.Beads, p.Current.Rounds,
		p.Prior.Findings, p.Prior.Beads, p.Prior.Rounds)
	fmt.Fprintf(&b, "Examples: %s\n", strings.Join(p.Examples, ", "))
	fmt.Fprintln(&b, "\nGoal: a directive edit that stops om raising this class.")
	fmt.Fprintf(&b, "Constraints: the edit goes to the directive files in %s; the edit text is the overseer's to write. A rule that changes what polecats may do needs the operator's yes.\n", directivesDir)
	fmt.Fprintln(&b, "Out of scope: om's bar or threshold; turning anything on.")
	fmt.Fprintln(&b, "Gate: go test ./internal/cmd -run TestLandOmLessons -count=1")
	fmt.Fprintln(&b, "Size: one directive edit, one landing.")
	fmt.Fprintln(&b, "Acceptance:")
	fmt.Fprintln(&b, "- [ ] the rule for this class says what the case is and what to do instead")
	fmt.Fprintln(&b, "- [ ] the next weekly pass counts fewer findings of this class than this window")
	return b.String()
}

// ProposalNote returns one proposal as plain prose for the bead the daemon
// files: the window, the class, its coverage, the counts, three example beads
// and the directive directory, in sentences.
//
// It carries no shaped section, so the shape lint refuses the filed bead and
// no dispatcher slings the overseer's directive edit to a seat (gt-dw2vj2).
// ProposalDraft stays the shaped text `gt land om-lessons --propose` prints on
// purpose. A too-few proposal is left out before this is called, as with
// ProposalDraft.
func ProposalNote(p Proposal, window, directivesDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The om lessons pass read the window %s.\n", window)
	fmt.Fprintf(&b, "The proposal is for the class %s (%s).\n", p.Name, p.ID)
	fmt.Fprintf(&b, "Its rule coverage is %s.\n", p.Coverage)
	fmt.Fprintf(&b, "It counted %d finding(s) on %d bead(s) with %d round(s) burned, against %d finding(s) on %d bead(s) with %d round(s) in the prior window.\n",
		p.Current.Findings, p.Current.Beads, p.Current.Rounds,
		p.Prior.Findings, p.Prior.Beads, p.Prior.Rounds)
	fmt.Fprintf(&b, "The example beads are %s.\n", strings.Join(p.Examples, ", "))
	fmt.Fprintf(&b, "The overseer writes the directive edit in %s.\n", directivesDir)
	return b.String()
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
