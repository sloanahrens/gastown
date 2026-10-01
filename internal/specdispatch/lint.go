// Package specdispatch is the decision core of the spec dispatcher (gt-4k3fj.5):
// the dispatch-time spec lint, the seat budget, candidate ordering, and the Dolt-contention retry. It holds no I/O — the
// `gt spec` commands (internal/cmd/spec.go) read beads, sessions and settings
// and hand them here, so every decision is testable with plain values.
//
// A spec is a bead the operator writes against the D10 template
// (~/.claude/docs/agents/spec-template.md). The operator-side writer
// (/workorder) runs the same checks before filing and calls `gt spec lint`
// afterwards, so the two stay in parity by reading one template file.
package specdispatch

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Labels and types the lint and the dispatcher key on.
const (
	// SpecLabel marks a bead as a dispatchable spec.
	SpecLabel = "spec"
	// NeedsPlanningLabel routes a spec to the planner instead of a polecat.
	NeedsPlanningLabel = "needs-planning"
	// DispatchFailedLabel marks a spec whose sling failed for a reason other
	// than capacity. The dispatcher skips it until the label is removed.
	DispatchFailedLabel = "spec-dispatch-failed"
	// SpecType is the only bead type a spec may carry.
	SpecType = "feature"

	// MinAcceptance and MaxAcceptance bound the acceptance list. Fewer than
	// one is a refusal; more than MaxAcceptance is a spec one worker cannot
	// take in one MR, so it goes to the planner. The template prefers 3-6.
	MinAcceptance       = 1
	PreferredAcceptance = 3
	MaxAcceptance       = 6

	// TemplateEnv overrides the template path (tests, other homes).
	TemplateEnv = "GT_SPEC_TEMPLATE"
)

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

// DefaultTemplatePath is the operator's template: $GT_SPEC_TEMPLATE, else
// ~/.claude/docs/agents/spec-template.md.
func DefaultTemplatePath() string {
	if p := os.Getenv(TemplateEnv); p != "" {
		return p
	}
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

// Verdict is the lint's answer.
type Verdict struct {
	Route Route
	// Field names the first missing or failing field ("type", "label spec",
	// "## Gate", "acceptance", "size"). Empty when clean.
	Field string
	// Reason is a short human phrase for Field.
	Reason string
}

// Clean reports whether the spec may be dispatched to a polecat.
func (v Verdict) Clean() bool { return v.Route == RouteDispatch }

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

// Lint validates a spec against the template. Checks run in a fixed order and
// the first miss is the verdict, so the one-line report always names the same
// field for the same bead:
//
//  1. type feature
//  2. label spec
//  3. every template section present and non-empty
//  4. at least one acceptance item
//  5. size: needs-planning label, a Size that says planning, or more than
//     MaxAcceptance items route to the planner; a Size that does not say one
//     worker is refused
func Lint(s Spec, t Template) Verdict {
	if !strings.EqualFold(strings.TrimSpace(s.Type), SpecType) {
		got := strings.TrimSpace(s.Type)
		if got == "" {
			got = "none"
		}
		return Verdict{Route: RouteRefuse, Field: "type", Reason: fmt.Sprintf("type is %s, want %s", got, SpecType)}
	}
	if !s.HasLabel(SpecLabel) {
		return Verdict{Route: RouteRefuse, Field: "label spec", Reason: "label spec missing"}
	}
	sections := Sections(s.Description)
	for _, name := range t.Sections {
		body, ok := sections[strings.ToLower(name)]
		if !ok {
			return Verdict{Route: RouteRefuse, Field: "## " + name, Reason: "section missing"}
		}
		if body == "" {
			return Verdict{Route: RouteRefuse, Field: "## " + name, Reason: "section empty"}
		}
	}
	items := CountAcceptance(acceptanceText(s, sections))
	if items < MinAcceptance {
		return Verdict{Route: RouteRefuse, Field: "acceptance", Reason: "no acceptance criteria"}
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
		return Verdict{Route: RouteRefuse, Field: "size", Reason: fmt.Sprintf("size %q is not one worker, one MR", firstLine(size))}
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
