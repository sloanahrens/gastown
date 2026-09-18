package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/formula"
)

// renderFormulaChecklist renders a formula as a checklist the agent can hold in
// a bounded context: the title of every step, the full body of exactly one step
// (fullStep, 1-based; out of range falls back to step 1), and one line telling
// the agent how to fetch any other step body on demand.
//
// Claude Code delivers at most 10,000 characters of hook output to the model, so
// prime cannot afford the full body of every step (mol-polecat-work alone is
// ~19 KB, mol-refinery-patrol ~51 KB).
func renderFormulaChecklist(formulaName string, f *formula.Formula, varMap map[string]string, fullStep int) string {
	if f == nil || len(f.Steps) == 0 {
		return ""
	}
	if fullStep < 1 || fullStep > len(f.Steps) {
		fullStep = 1
	}

	var sb strings.Builder
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "**Formula Checklist** (%d steps from %s):\n\n", len(f.Steps), formulaName)
	for i, step := range f.Steps {
		title := applyFormulaVars(step.Title, varMap)
		fmt.Fprintf(&sb, "### Step %d: %s\n\n", i+1, title)
		if i+1 != fullStep {
			continue
		}
		if desc := applyFormulaVars(step.Description, varMap); desc != "" {
			sb.WriteString(desc)
			sb.WriteString("\n\n")
		}
	}
	fmt.Fprintf(&sb, "Only step %d is shown in full. Before starting any other step, read it with `%s prime --step <N> --formula %s`.\n\n",
		fullStep, cli.Name(), formulaName)
	return sb.String()
}

// primeHookBudget is the most prime will print as a SessionStart hook. Claude
// Code persists hook stdout over 10,000 characters to a file and gives the
// model only a 2 KB preview, so the payload must stay under that with margin.
const primeHookBudget = 9000

// primeSection is one independently droppable block of prime output.
type primeSection struct {
	name     string
	priority int // lower number = more important; dropped last
	keep     bool
	text     string
}

// primePayload collects prime output sections and renders them under a budget,
// dropping the least important droppable sections first.
type primePayload struct {
	sections []primeSection
}

// add records a section. Empty text is ignored. keep marks a section that is
// never dropped (the hooked work, the start-now footer).
func (p *primePayload) add(name string, priority int, keep bool, text string) {
	if text == "" {
		return
	}
	p.sections = append(p.sections, primeSection{name: name, priority: priority, keep: keep, text: text})
}

// render concatenates the sections in insertion order. When budget > 0 and the
// total exceeds it, droppable sections are removed lowest priority first until
// the payload fits (or nothing droppable remains), and a note naming what was
// omitted is placed before the final section so the closing instruction stays last.
func (p *primePayload) render(budget int) string {
	sections := make([]primeSection, len(p.sections))
	copy(sections, p.sections)

	var omitted []string
	for budget > 0 && payloadLen(sections) > budget {
		victim := -1
		for i, s := range sections {
			if s.keep {
				continue
			}
			if victim == -1 || s.priority > sections[victim].priority {
				victim = i
			}
		}
		if victim == -1 {
			break
		}
		omitted = append(omitted, sections[victim].name)
		sections = append(sections[:victim], sections[victim+1:]...)
	}

	var sb strings.Builder
	for i, s := range sections {
		if len(omitted) > 0 && i == len(sections)-1 {
			fmt.Fprintf(&sb, "\n_[prime] omitted to fit the hook budget: %s. Run `%s prime` (no --hook) to read the full payload._\n\n",
				strings.Join(omitted, ", "), cli.Name())
		}
		sb.WriteString(s.text)
	}
	if len(omitted) > 0 && len(sections) == 0 {
		fmt.Fprintf(&sb, "\n_[prime] omitted to fit the hook budget: %s._\n", strings.Join(omitted, ", "))
	}
	return sb.String()
}

func payloadLen(sections []primeSection) int {
	n := 0
	for _, s := range sections {
		n += len(s.text)
	}
	return n
}
