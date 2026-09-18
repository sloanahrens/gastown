package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/formula"
)

// primeStepBodyMaxChars caps the one step body the checklist renders in full.
// Some patrol steps run to 7 KB (mol-witness-patrol step 1); the payload has
// to stay bounded regardless of formula content, and the agent can read the
// rest with `gt prime --step N`.
const primeStepBodyMaxChars = 3000

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
			if len(desc) > primeStepBodyMaxChars {
				cut := strings.LastIndexByte(desc[:primeStepBodyMaxChars], '\n')
				if cut <= 0 {
					cut = primeStepBodyMaxChars
				}
				sb.WriteString(desc[:cut])
				fmt.Fprintf(&sb, "\n\n_[prime] step body truncated at %d of %d chars; read it in full with `%s prime --step %d --formula %s`._",
					cut, len(desc), cli.Name(), fullStep, formulaName)
			} else {
				sb.WriteString(desc)
			}
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

// primeStaticTextDelivered reports whether the agent runtime was started with
// the rendered role system prompt (see config.withRoleSystemPromptFlag). When
// true, gt prime omits the static role text from its output: the model
// already has it, and repeating 15-25 KB in the hook would push the payload
// over Claude Code's 10,000-character hook limit.
func primeStaticTextDelivered() bool {
	path := os.Getenv(config.EnvSystemPromptFile)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// writeSystemPromptFile refreshes the role's system-prompt file for the next
// spawn. It writes only when the content differs (so concurrent primes of the
// same role do not churn the file) and reports whether it wrote.
func writeSystemPromptFile(path, content string) (bool, error) {
	if path == "" {
		return false, nil
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := atomicfile.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// staticRoleText is everything about a role that does not change between
// sessions: the role template (or, when templates are unavailable or the role
// is unknown, the hardcoded fallback context) plus the operator's CONTEXT.md.
// fromTemplate reports whether the role template rendered; only that text is
// worth persisting to the system-prompt file.
func staticRoleText(ctx RoleContext) (text string, fromTemplate bool, err error) {
	text, err = renderRoleTemplate(ctx)
	if err != nil {
		return "", false, err
	}
	fromTemplate = text != ""
	if !fromTemplate {
		explain(true, "Role context: templates unavailable or role unknown, using hardcoded fallback")
		text = captureOutput(func() { outputPrimeContextFallback(ctx) })
	}
	contextPath := filepath.Join(ctx.TownRoot, "CONTEXT.md")
	data, readErr := os.ReadFile(contextPath)
	if readErr != nil || len(data) == 0 {
		explain(true, "CONTEXT.md: not found at "+contextPath)
		return text, fromTemplate, nil
	}
	explain(true, "CONTEXT.md: found at "+contextPath+", injecting contents")
	text += "\n" + string(data)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text, fromTemplate, nil
}

// systemPromptPathFor returns the system-prompt file for the priming agent, or
// "" for roles that do not use one.
func systemPromptPathFor(ctx RoleContext) string {
	rigPath := ""
	if ctx.Rig != "" && ctx.TownRoot != "" {
		rigPath = filepath.Join(ctx.TownRoot, ctx.Rig)
	}
	return config.SystemPromptFilePath(string(ctx.Role), ctx.TownRoot, rigPath)
}

// useCompactResumePath decides between the brief compact/resume output and the
// full dynamic payload. Resume always takes the brief path (the conversation is
// intact). After compaction the hooked work and checklist have been summarised
// away; when the static role text lives in the system prompt (which survives
// compaction) the dynamic payload is small enough to re-send in full, so we do.
func useCompactResumePath(source, handoffReason string, staticDelivered bool) bool {
	if source == "resume" {
		return true
	}
	if source == "compact" || handoffReason == "compaction" {
		return !staticDelivered
	}
	return false
}

// renderFormulaStep renders the title and full body of one step (1-based) for
// `gt prime --step N`.
func renderFormulaStep(formulaName string, f *formula.Formula, varMap map[string]string, n int) (string, error) {
	if f == nil || n < 1 || n > len(f.Steps) {
		return "", fmt.Errorf("formula %s has no step %d", formulaName, n)
	}
	step := f.Steps[n-1]
	var sb strings.Builder
	fmt.Fprintf(&sb, "### Step %d: %s\n\n", n, applyFormulaVars(step.Title, varMap))
	if desc := applyFormulaVars(step.Description, varMap); desc != "" {
		sb.WriteString(desc)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// primeHookTestBudget is the ceiling the fixture tests hold every role to,
// leaving headroom under primeHookBudget for live variance (longer bead
// titles, more memories, an extra directive).
const primeHookTestBudget = 8000

// primeDirectiveMaxChars caps the operator directive text rendered into the
// hook payload. Directives are meant to steer a role, not to be a manual.
const primeDirectiveMaxChars = 2000

// captureOutput runs fn with os.Stdout redirected and returns what it wrote.
// prime's section emitters print directly; capturing lets the payload
// assembler order and budget them without rewriting every emitter.
func captureOutput(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		fn()
		return ""
	}
	old := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	func() {
		defer func() {
			os.Stdout = old
			_ = w.Close()
		}()
		fn()
	}()
	<-done
	_ = r.Close()
	return buf.String()
}

// primeParts are the dynamic sections of a prime, each produced on demand.
// A nil part is skipped.
type primeParts struct {
	session     func() string
	hookedWork  func() string
	molecule    func() string
	directives  func() string
	handoff     func() string
	checkpoint  func() string
	memories    func() string
	mail        func() string
	escalations func() string
	startup     func() string
}

// assemblePrimePayload orders the prime output so the hooked work comes first
// and the least important sections are the ones a budget drops. staticText is
// the role template (+CONTEXT.md); it is included only when the runtime did
// not receive it as a system prompt, placed after the hooked work so the 2 KB
// preview still shows the assignment.
func assemblePrimePayload(parts primeParts, staticText string, includeStatic bool, hasSlungWork bool) *primePayload {
	p := &primePayload{}
	call := func(f func() string) string {
		if f == nil {
			return ""
		}
		return f()
	}
	p.add("session", 0, true, call(parts.session))
	p.add("hooked-work", 1, true, call(parts.hookedWork))
	if includeStatic {
		p.add("role", 1, true, staticText)
	}
	p.add("molecule", 2, false, call(parts.molecule))
	p.add("directives", 3, false, call(parts.directives))
	p.add("handoff", 4, false, call(parts.handoff))
	p.add("checkpoint", 4, false, call(parts.checkpoint))
	p.add("mail", 5, false, call(parts.mail))
	p.add("memories", 6, false, call(parts.memories))
	p.add("escalations", 5, false, call(parts.escalations))
	if !hasSlungWork {
		p.add("startup", 9, true, call(parts.startup))
	}
	return p
}

// primeStepFormulaName resolves which formula `gt prime --step N` reads:
// an explicit --formula, else the hooked bead's attached formula, else the
// role's patrol formula.
func primeStepFormulaName(ctx RoleContext, hookedBead *beads.Issue, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if hookedBead != nil {
		if att := beads.ParseAttachmentFields(hookedBead); att != nil && att.AttachedFormula != "" {
			return att.AttachedFormula
		}
	}
	switch ctx.Role {
	case RoleWitness:
		return constants.MolWitnessPatrol
	case RoleRefinery:
		return constants.MolRefineryPatrol
	case RoleDeacon:
		return constants.MolDeaconPatrol
	}
	return ""
}
