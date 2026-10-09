// Package specdispatch is the decision core of the spec dispatcher (gt-4k3fj.5):
// the dispatch-time spec lint, the seat budget, candidate ordering, and the Dolt-contention retry. It holds no I/O — the
// `gt spec` commands (internal/cmd/spec.go) read beads, sessions and settings
// and hand them here, so every decision is testable with plain values.
//
// A work bead is a bead carrying the D10 shape: an acceptance list plus the
// sections Template names — Goal, Constraints, Out of scope, Gate and Size by
// default. That list is the template file's when the file exists and has
// "## " headings, and DefaultSections otherwise; Template.Source names which
// one was read.
//
// Shape is a property of every work bead (gt-mmsr2): the lint checks the
// required sections, the acceptance list and the Size on a task, bug or feature alike.
// It does not read the bead's type or labels to decide whether to check —
// epics, agent beads and the town's runtime families are refused as "not a
// work bead", and nothing else is.
//
// A lint returns one of three routes: dispatch, planning (well-formed, but for
// the planner to decompose before a worker takes it), or refuse (a missing
// section is never guessed at). The dispatcher holds a refused bead and, under
// every gate that holds one, a planning bead too — a planning bead is labeled
// needs-planning and commented as the planner's, never slung (gt-tod3q).
package specdispatch

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// EpicType is the bead type of a container of work, never a unit of it.
const EpicType = "epic"

// Labels and types the lint and the dispatcher key on.
const (
	// SpecLabel is retired (gt-mmsr2): the shape lint checks every work bead,
	// so no bead needs it, and a filer who adds it by habit gets the same
	// verdict.
	SpecLabel = "spec"
	// SpecType is retired with SpecLabel (gt-mmsr2): a work bead of any type
	// is linted. Kept so callers that name it still compile.
	SpecType = "feature"
	// NeedsPlanningLabel routes a spec to the planner instead of a polecat.
	NeedsPlanningLabel = "needs-planning"
	// DispatchFailedLabel marks a spec whose sling failed for a reason other
	// than capacity. The dispatcher skips it until the label is removed. The
	// name is constants.LabelSpecDispatchFailed's: internal/townhealth counts
	// the beads wearing it and must not import this package to spell it
	// (gt-q6zoo).
	DispatchFailedLabel = constants.LabelSpecDispatchFailed
	// ShapeWaivedLabel waives a bead's shape lint: the dispatcher slings a
	// bead wearing it even when the lint refuses it, so an operator can take
	// one bead off the unshaped hold by hand instead of reshaping it (gt-f8ppx).
	ShapeWaivedLabel = "spec-shape-waived"

	// MinAcceptance and MaxAcceptance bound the acceptance list. Fewer than
	// one is a refusal; more than MaxAcceptance is a spec one worker cannot
	// take in one MR, so it goes to the planner. The template prefers 3-6.
	MinAcceptance       = 1
	PreferredAcceptance = 3
	MaxAcceptance       = 6
)

// NonWorkBeadTypes are the bead kinds the lint never reads a shape from: an
// epic is a container of work rather than a unit of it, and the town's runtime
// families (wisp, message, agent, convoy, ...) record state, not work. The
// runtime half is constants.NonDispatchableBeadTypes, the same list every
// other consumer that asks "can a polecat take this?" filters on.
func NonWorkBeadTypes() []string {
	return append([]string{EpicType}, constants.NonDispatchableBeadTypes...)
}

// NotWorkBead names why a bead is not a work bead the lint may check: its type
// is an epic or a runtime family, it wears a runtime family label (gt:agent,
// gt:wisp, ...), or it is ephemeral, which is what a wisp is. Empty means the
// bead is a work bead. The reason names what the bead is ("type epic",
// "label gt:agent", "wisp").
func NotWorkBead(s Spec) string {
	t := strings.ToLower(strings.TrimSpace(s.Type))
	if t == EpicType {
		return "type " + EpicType
	}
	for _, nt := range constants.NonDispatchableBeadTypes {
		if t == nt {
			return "type " + nt
		}
	}
	for _, l := range constants.NonDispatchableBeadLabels {
		if s.HasLabel(l) {
			return "label " + l
		}
	}
	if s.Ephemeral {
		return "wisp"
	}
	return ""
}

// DefaultSections are the template's required sections, used when the
// template file cannot be read. They match the file as of 2026-09-29; the file
// wins whenever it is present.
var DefaultSections = []string{"Goal", "Constraints", "Out of scope", "Gate", "Size"}

// Template is the shape a spec is linted against.
type Template struct {
	// Sections are the required "## " headings, in template order.
	Sections []string
	// Source names where Sections came from: the file path, or "built-in".
	Source string
}

// DefaultTemplatePath is the operator's template,
// ~/.claude/docs/agents/spec-template.md. daemon.json
// patrols.spec_dispatch.template overrides it.
func DefaultTemplatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "docs", "agents", "spec-template.md")
}

// LoadTemplate reads the required sections from the template file. A missing
// or unreadable file, or one with no "## " headings, falls back to
// DefaultSections; Source says which one was used so a report can name it.
func LoadTemplate(path string) Template {
	if path != "" {
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: operator-configured template path
			if sections := ParseTemplateSections(string(data)); len(sections) > 0 {
				return Template{Sections: sections, Source: path}
			}
		}
	}
	return Template{Sections: append([]string(nil), DefaultSections...), Source: "built-in"}
}

// ParseTemplateSections returns the distinct "## " headings in the template
// text, in order. The template shows the description inside a fenced example,
// so headings are read wherever they appear, fence or not.
func ParseTemplateSections(text string) []string {
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// The description is quoted inside a shell string in the template, so
		// the first heading follows the opening quote: --description="## Goal.
		if i := strings.Index(line, `"## `); i >= 0 {
			line = line[i+1:]
		}
		line = strings.TrimLeft(line, `"'`)
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "## "))
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// acceptanceHint is the skeleton's acceptance line: the item count the lint
// enforces, so the printed bound cannot drift from MinAcceptance/MaxAcceptance.
var acceptanceHint = fmt.Sprintf(`%d-%d "- [ ] ..." items`, MinAcceptance, MaxAcceptance)

// sectionHints are the one-phrase hints the skeleton prints under a required
// section. A section a template adds on its own gets the generic hint.
var sectionHints = map[string]string{
	"goal":         "what the bead must achieve",
	"constraints":  "files, limits and rules the work must respect",
	"out of scope": "what this bead does not do",
	"gate":         "the command that must pass",
	"size":         "one worker, one landing",
}

// genericSectionHint is the hint for a template section the skeleton has no
// phrase for.
const genericSectionHint = "one or two sentences"

// Skeleton renders the description shape a refused bead is missing: every
// required section with a one-phrase hint, the acceptance line and the re-lint
// command that takes the bead back to the lint.
func (t Template) Skeleton(beadID string) string {
	var b strings.Builder
	for _, name := range t.Sections {
		fmt.Fprintf(&b, "## %s\n%s\n", name, sectionHint(name))
	}
	if !t.hasAcceptanceSection() {
		fmt.Fprintf(&b, "## Acceptance\n%s\n", acceptanceHint)
	}
	fmt.Fprintf(&b, "Fix the description (bd update %s --description=...) and run gt spec lint %s again.\n", beadID, beadID)
	return b.String()
}

// hasAcceptanceSection reports whether the template already requires an
// acceptance section, under any name acceptanceText reads.
func (t Template) hasAcceptanceSection() bool {
	for _, name := range t.Sections {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "acceptance") {
			return true
		}
	}
	return false
}

func sectionHint(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(key, "acceptance") {
		return acceptanceHint
	}
	if hint, ok := sectionHints[key]; ok {
		return hint
	}
	return genericSectionHint
}

// Spec is the slice of a bead the lint reads.
type Spec struct {
	ID          string
	Title       string
	Type        string
	Status      string
	Assignee    string
	Priority    int
	CreatedAt   string
	Labels      []string
	Description string
	Design      string
	Notes       string
	Acceptance  string
	// SubmittedForLanding marks a bead whose notes carry a live landing
	// request: a READY TO LAND block that no LANDING RECORD and no MERGE
	// REJECTION follows. gt done writes that block before the gt:ready-to-land
	// label, and a label is the half of an answer known to go missing
	// (gt-q6zoo), so a bead mid-submission reaches the dispatcher marked here
	// while it wears no label (gt-kr5xv). The caller fills it from the notes;
	// nothing in this package parses the block.
	SubmittedForLanding bool
	// Ephemeral marks a wisp: an ephemeral bead (a molecule, a merge
	// request) that bd keeps out of the issues table. No wisp is work.
	Ephemeral bool
	// Children are the bead's direct children, filled by the caller from bd's
	// parent-child edges. The container rule reads them through OpenChildHold;
	// nothing in this package queries (gt-gektq).
	Children []Child
}

// HasLabel reports whether the spec carries label, ignoring case and space.
func (s Spec) HasLabel(label string) bool {
	for _, l := range s.Labels {
		if strings.EqualFold(strings.TrimSpace(l), label) {
			return true
		}
	}
	return false
}

// Route is what the lint says to do with a spec.
type Route string

const (
	// RouteDispatch: the spec is clean and one worker can take it.
	RouteDispatch Route = "dispatch"
	// RoutePlanning: the spec is well-formed but needs decomposition first.
	RoutePlanning Route = "planning"
	// RouteRefuse: the spec is missing something; never guess, never dispatch.
	RouteRefuse Route = "refuse"
)

// Refusal is one shape failure: the field that failed ("## Gate",
// "acceptance", "size", "not a work bead") and the phrase the one-line report
// uses. gt spec lint --json prints the whole list so a shell caller can act on
// every failure at once.
type Refusal struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Verdict is the lint's answer.
type Verdict struct {
	Route Route
	// Field names the first refusal's field ("## Gate", "acceptance", "size",
	// "not a work bead"), or "size" when the route is planning. Empty when
	// clean.
	Field string
	// Reason is a short human phrase for Field.
	Reason string
	// Refusals is every shape failure found, in check order. Empty unless
	// Route is RouteRefuse; Field and Reason are its first entry.
	Refusals []Refusal
}

// refusal builds a refused verdict from one or more failures. Field and Reason
// mirror the first failure so the one-line report and the dispatcher's note
// keep naming one field.
func refusal(failures ...Refusal) Verdict {
	v := Verdict{Route: RouteRefuse, Refusals: failures}
	if len(failures) > 0 {
		v.Field, v.Reason = failures[0].Field, failures[0].Reason
	}
	return v
}

// Clean reports whether the spec may be dispatched to a polecat.
func (v Verdict) Clean() bool { return v.Route == RouteDispatch }

// ShapeNote renders the verdict as the note a shape gate leaves on a bead the
// lint did not clear: "SHAPE: <field>: <reason>; ..." for a refusal, "SHAPE:
// needs planning" for a spec that must be decomposed first. It is the
// seat-refill plugin's own comment text (gt-cq5gb), kept so the bead's history
// reads the same across the move to the Go dispatcher. A refused bead wears it
// under every gate that holds one; a planning verdict wears it only when the
// bead is waived and dispatched, because the dispatcher otherwise holds a
// planning bead and comments it with its own planning note (gt-tod3q). Empty
// when the verdict is clean.
func (v Verdict) ShapeNote() string {
	switch v.Route {
	case RouteDispatch:
		return ""
	case RoutePlanning:
		return "SHAPE: needs planning"
	}
	parts := make([]string, 0, len(v.Refusals))
	for _, r := range v.Refusals {
		parts = append(parts, r.Field+": "+r.Reason)
	}
	if len(parts) == 0 {
		// A refused verdict the lint rendered without refusals is the lint's
		// problem, not a reason to leave the note half-written.
		return "SHAPE: refused"
	}
	return "SHAPE: " + strings.Join(parts, "; ")
}

// UnshapedReason is the dispatcher's hold reason for a bead whose shape the
// lint refused: "unshaped: <fields>", naming every failed field in check order
// ("unshaped: ## Goal, acceptance"). Empty when the verdict is not a refusal:
// a planning verdict is the planner's hold, not an unshaped one — the
// dispatcher holds it under warn as well as refuse and names it by its
// planning state instead (gt-f8ppx, gt-tod3q).
func (v Verdict) UnshapedReason() string {
	if v.Route != RouteRefuse {
		return ""
	}
	fields := make([]string, 0, len(v.Refusals))
	for _, r := range v.Refusals {
		fields = append(fields, r.Field)
	}
	if len(fields) == 0 {
		if v.Field == "" {
			return "unshaped"
		}
		fields = append(fields, v.Field)
	}
	return "unshaped: " + strings.Join(fields, ", ")
}

// Line renders the one-line report for a bead: "<id>: spec lint ok",
// "<id>: spec lint refused: <field>: <reason>", or
// "<id>: spec needs planning: <reason>".
func (v Verdict) Line(id string) string {
	switch v.Route {
	case RouteDispatch:
		return fmt.Sprintf("%s: spec lint ok", id)
	case RoutePlanning:
		return fmt.Sprintf("%s: spec needs planning: %s", id, v.Reason)
	}
	return fmt.Sprintf("%s: spec lint refused: %s: %s", id, v.Field, v.Reason)
}

var (
	headingRe    = regexp.MustCompile(`^#{2}\s+(.+?)\s*#*\s*$`)
	acceptItemRe = regexp.MustCompile(`^\s*(?:[-*+]\s+(?:\[[ xX]\]\s*)?|\d+[.)]\s+)(\S.*)$`)
	oneWorkerRe  = regexp.MustCompile(`(?i)\bone\s+worker\b`)
	planningRe   = regexp.MustCompile(`(?i)needs?[\s-]+planning|decompos|multiple\s+workers`)
)

// Sections splits markdown into "## " sections keyed by lower-case heading.
// Deeper headings (###) stay inside their section's body.
func Sections(markdown string) map[string]string {
	out := map[string]string{}
	var current string
	var body strings.Builder
	flush := func() {
		if current != "" {
			if _, dup := out[current]; !dup {
				out[current] = strings.TrimSpace(body.String())
			}
		}
		body.Reset()
	}
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			if m := headingRe.FindStringSubmatch(trimmed); m != nil {
				flush()
				current = strings.ToLower(strings.TrimSpace(m[1]))
				continue
			}
		}
		if current != "" {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	flush()
	return out
}

// CountAcceptance counts list items in an acceptance text: "- [ ] x", "- x",
// "* x", "1. x". Free prose with no list items counts as one item when it is
// non-empty, so a single-sentence criterion is not read as none.
func CountAcceptance(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if acceptItemRe.MatchString(line) {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

// acceptanceText is the bead's acceptance field, else an "## Acceptance..."
// section of the description.
func acceptanceText(s Spec, sections map[string]string) string {
	if strings.TrimSpace(s.Acceptance) != "" {
		return s.Acceptance
	}
	for key, body := range sections {
		if strings.HasPrefix(key, "acceptance") {
			return body
		}
	}
	return ""
}

// Lint validates the shape of a work bead against the template. Every failure
// is collected, in a fixed check order, so the one-line report always names
// the same first field for the same bead and --json can list the rest:
//
//  1. not a work bead: an epic, an agent bead, a wisp and the other runtime
//     families are refused before any shape is read
//  2. every template section present and non-empty
//  3. at least one acceptance item
//  4. size: needs-planning label, a Size that says planning, or more than
//     MaxAcceptance items route to the planner; a Size that does not say one
//     worker is refused
//
// The bead's type and labels are not a gate: a task or bug with the five
// sections is as lintable as a feature (gt-mmsr2).
func Lint(s Spec, t Template) Verdict {
	if why := NotWorkBead(s); why != "" {
		return refusal(Refusal{Field: "not a work bead", Reason: why})
	}
	sections := Sections(s.Description)
	var failures []Refusal
	for _, name := range t.Sections {
		body, ok := sections[strings.ToLower(name)]
		switch {
		case !ok:
			failures = append(failures, Refusal{Field: "## " + name, Reason: "section missing"})
		case body == "":
			failures = append(failures, Refusal{Field: "## " + name, Reason: "section empty"})
		}
	}
	items := CountAcceptance(acceptanceText(s, sections))
	if items < MinAcceptance {
		failures = append(failures, Refusal{Field: "acceptance", Reason: "no acceptance criteria"})
	}
	if len(failures) > 0 {
		return refusal(failures...)
	}
	size := sections["size"]
	switch {
	case s.HasLabel(NeedsPlanningLabel):
		return Verdict{Route: RoutePlanning, Field: "size", Reason: "label " + NeedsPlanningLabel}
	case size != "" && planningRe.MatchString(size):
		return Verdict{Route: RoutePlanning, Field: "size", Reason: "size says: " + firstLine(size)}
	case items > MaxAcceptance:
		return Verdict{Route: RoutePlanning, Field: "size", Reason: fmt.Sprintf("%d acceptance items (max %d for one worker)", items, MaxAcceptance)}
	}
	// A template without a Size section has nothing to read here.
	if hasSection(t, "size") && !oneWorkerRe.MatchString(size) {
		return refusal(Refusal{Field: "size", Reason: fmt.Sprintf("size %q is not one worker, one MR", firstLine(size))})
	}
	return Verdict{Route: RouteDispatch}
}

func hasSection(t Template, name string) bool {
	for _, s := range t.Sections {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}
