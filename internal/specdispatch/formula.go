package specdispatch

import (
	"fmt"
	"slices"
	"strings"
)

// Labels and section a bead uses to declare the formula that runs it
// (gt-j5cr2). The dispatcher slings such a bead like any other, with that
// formula and those variables instead of the rig's default formula.
const (
	// FormulaLabelPrefix starts the label naming the bead's formula:
	// formula:<name>.
	FormulaLabelPrefix = "formula:"
	// ScheduledLabelPrefix starts the label the scheduler puts on a run bead:
	// scheduled:<name>. A bead wearing both labels is a scheduled run, and is
	// slung under the actor the scheduler itself used.
	ScheduledLabelPrefix = "scheduled:"
	// ScheduledActorPrefix is that actor's prefix: daemon/scheduled:<name>.
	ScheduledActorPrefix = "daemon/scheduled:"
	// formulaVarsSection is the "## " heading, lower-cased as Sections keys it,
	// whose lines are the formula's variables.
	formulaVarsSection = "formula vars"
)

// FormulaRun is what a bead declares about its own run: the formula to sling
// with, its variables as key=value pairs in the order they first appear, and
// the actor the sling is recorded under. The zero value is a bead that
// declares nothing, which is slung as the dispatcher always has.
type FormulaRun struct {
	Formula string
	Vars    []string
	// Actor is set only for a scheduled run; empty keeps the dispatcher's own.
	Actor string
}

// Declared reports whether the bead names a formula.
func (r FormulaRun) Declared() bool { return r.Formula != "" }

// labelValues returns the value of every label starting with prefix (matched as
// HasLabel matches: case- and space-insensitive), in label order and without
// repeats. A label that is only the prefix yields "".
func (s Spec) labelValues(prefix string) []string {
	var out []string
	for _, l := range s.Labels {
		l = strings.TrimSpace(l)
		if len(l) < len(prefix) || !strings.EqualFold(l[:len(prefix)], prefix) {
			continue
		}
		v := strings.TrimSpace(l[len(prefix):])
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// ParseFormulaVars reads a "## Formula vars" section body: one key=value per
// line, split at the first "=". Blank lines and lines starting with "#" are
// ignored. A line without "=" or with an empty key is an error naming the line,
// so a typo is never slung as a run missing a variable. A repeated key keeps
// its first position and its last value. The value is trimmed and may be empty.
func ParseFormulaVars(body string) ([]string, error) {
	var order []string
	values := map[string]string{}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("%s: %s", SkipBadFormulaVars, line)
		}
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		values[key] = strings.TrimSpace(value)
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+values[k])
	}
	return out, nil
}

// FormulaRunOf reads the run a bead declares. A bead with no formula label
// declares nothing and returns the zero value, whatever its description says.
// A bead whose declaration cannot be read — two different formula labels, an
// empty one, or a malformed vars line — returns an error whose text is the
// reason the dispatcher skips it with; whether the formula exists is the
// caller's to check, since this package never queries.
func FormulaRunOf(s Spec) (FormulaRun, error) {
	names := s.labelValues(FormulaLabelPrefix)
	if len(names) == 0 {
		return FormulaRun{}, nil
	}
	if len(names) > 1 {
		return FormulaRun{}, fmt.Errorf("%s: %s", SkipConflictingFormulaLabels, strings.Join(names, ", "))
	}
	if names[0] == "" {
		return FormulaRun{}, fmt.Errorf("%s: %s", SkipBadFormulaLabel, FormulaLabelPrefix)
	}
	run := FormulaRun{Formula: names[0]}
	vars, err := ParseFormulaVars(Sections(s.Description)[formulaVarsSection])
	if err != nil {
		return FormulaRun{}, err
	}
	run.Vars = vars
	for _, name := range s.labelValues(ScheduledLabelPrefix) {
		if name != "" {
			run.Actor = ScheduledActorPrefix + name
			break
		}
	}
	return run, nil
}
