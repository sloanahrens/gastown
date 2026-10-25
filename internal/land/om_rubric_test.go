package land

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shippedRubricPath is the repo-root .om.json: the rubric this repo's landing
// worker hands to om. The path is resolved from this test file, so it does not
// depend on the test's working directory.
func shippedRubricPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", ".om.json")
}

// omCriterion is one rubric entry. Weight is a pointer so a missing or
// non-numeric weight is a finding rather than a silent zero.
type omCriterion struct {
	Name     string   `json:"name"`
	Weight   *float64 `json:"weight"`
	Guidance string   `json:"guidance"`
}

type omRubric struct {
	Rubric []omCriterion `json:"rubric"`
}

func shippedRubric(t *testing.T) omRubric {
	t.Helper()
	path := shippedRubricPath(t)
	data, err := os.ReadFile(path) //nolint:gosec // G304: a path this test builds from its own file
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var r omRubric
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return r
}

// TestShippedOMRubricIsGradeable pins the shape om reads: the repo ships its
// own rubric, and a criterion missing a name, a weight, or guidance is a
// criterion no reviewer can apply or cite. The gate has no other check on
// .om.json — om parses the file itself, and a rubric that stopped parsing
// would be reported as a review that could not run, not as a red tree.
func TestShippedOMRubricIsGradeable(t *testing.T) {
	t.Parallel()
	r := shippedRubric(t)
	if len(r.Rubric) == 0 {
		t.Fatalf("%s carries no rubric criteria: om would grade every landing against nothing", shippedRubricPath(t))
	}
	for i, c := range r.Rubric {
		where := fmt.Sprintf("rubric[%d]", i)
		if c.Name != "" {
			where = fmt.Sprintf("criterion %q", c.Name)
		}
		if strings.TrimSpace(c.Name) == "" {
			t.Errorf("%s has no name", where)
		}
		if c.Weight == nil {
			t.Errorf("%s has no numeric weight", where)
		}
		if strings.TrimSpace(c.Guidance) == "" {
			t.Errorf("%s has no guidance", where)
		}
	}
}

// instructionProliferationCriterion is the criterion gt-1zff added: a new copy
// of an instruction that already exists is itself the finding, before the
// copies drift. docs-and-comments reaches the same class only after the copies
// disagree, so this pin keeps the two apart: a reword that keeps the name but
// lets "the copies agree today" answer the finding would collapse it back into
// docs-and-comments.
const instructionProliferationCriterion = "instruction-proliferation"

// TestShippedOMRubricCarriesInstructionProliferationCriterion pins the
// criterion and the things that make it gradeable: its weight, its place after
// docs-and-comments, and guidance that refuses the agreeing-copies defence and
// names the rule a reviewer cites.
func TestShippedOMRubricCarriesInstructionProliferationCriterion(t *testing.T) {
	t.Parallel()
	r := shippedRubric(t)
	idx := -1
	for i, c := range r.Rubric {
		if c.Name == instructionProliferationCriterion {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("the shipped rubric %s carries no %q criterion: the class gt-1zff gated (an added copy of an existing instruction scores clean until the copies drift, and the later change that touches one is charged for it) is ungraded until it is restored", shippedRubricPath(t), instructionProliferationCriterion)
	}
	for i, c := range r.Rubric {
		if c.Name == "docs-and-comments" && i > idx {
			t.Errorf("%s appears before docs-and-comments in %s; gt-1zff appends it after, so the two read in the order they apply", instructionProliferationCriterion, shippedRubricPath(t))
		}
	}
	c := r.Rubric[idx]
	if c.Weight == nil || *c.Weight != 2 {
		t.Errorf("%s weight = %v, want 2: gt-1zff weights it to move the score without blocking alone, the shape docs-and-comments already has", instructionProliferationCriterion, c.Weight)
	}
	if !strings.Contains(c.Guidance, "agree") {
		t.Errorf("%s guidance no longer refuses the defence that the copies currently agree: %q", instructionProliferationCriterion, c.Guidance)
	}
	if !strings.Contains(c.Guidance, "source of truth") {
		t.Errorf("%s guidance no longer names the pointer that clears a copy as its source of truth: %q", instructionProliferationCriterion, c.Guidance)
	}
	if !strings.Contains(c.Guidance, "R2") {
		t.Errorf("%s guidance no longer points at the rule it enforces (R2, one place per meaning), so a reviewer has nothing to cite: %q", instructionProliferationCriterion, c.Guidance)
	}
}

// scriptsAreDebtCriterion is the criterion gt-7rhdv appended for the town's
// 2026-10-24 "Scripts are debt" policy: a new script that holds testable
// logic is a MAJOR finding.
const scriptsAreDebtCriterion = "scripts-are-debt"

// scriptsAreDebtEarlierEntries is the shipped rubric as it stood when
// gt-7rhdv appended scripts-are-debt. That landing's acceptance named every
// earlier entry unchanged, and om reads the file without comparing it against
// anything, so without this pin a reworded, reweighted or reordered earlier
// criterion would land silently. A deliberate change to an earlier entry
// updates this list in the same landing.
var scriptsAreDebtEarlierEntries = []omCriterion{
	{Name: "correctness", Weight: ptrTo(3.0), Guidance: "Logic errors, unhandled edge cases, and broken invariants outrank all else."},
	{Name: "regression-safety", Weight: ptrTo(3.0), Guidance: "This is a large, heavily-tested codebase run in production by its own agents. Weight hermetic tests, behavior preservation, and blast radius: a fix that unmasks or breaks distant tests, leaks live-environment state into tests, or changes shared plumbing without coverage is a finding even when the local change looks correct."},
	{Name: "upstream-mergeability", Weight: ptrTo(2.0), Guidance: "This is a fork of gastownhall/gastown that may contribute patches upstream. Flag gratuitous divergence: unrelated reformatting, renames of upstream identifiers, fork-local conventions leaking into shared code, or changes that could be expressed as smaller upstream-friendly diffs."},
	{Name: "tests", Weight: ptrTo(2.0), Guidance: "Changed behavior needs tests; regression fixes need a test that fails before the fix. Flag untested behavior changes. A test whose fixture builds the layout the implementation already assumes proves nothing: for behavior that depends on the runtime environment (worktree path, cwd, module root), at least one case must drive the shape production actually takes, or the finding stands (moves the score; does not block alone)."},
	{Name: "test-engineering", Weight: ptrTo(2.0), Guidance: "Test debt is a finding. The rules are in docs/testing.md, the source of truth: cite the rule or section a test breaks, and do not restate it. MAJOR: a breach that leaves a wait or loop unbounded, raises a timeout, budget or ratchet to land the change, lets the result cache serve a stale pass, synchronises with sleep, retries or skips a failing test, reads a clock baseline after the launch it times, or asserts implementation details. MINOR: a breach of the tier-cost, fixture-reuse, slow-case-family, failure-message or shared-state rules. Phrase test findings as static hypotheses from the code, never as 'fails when run'."},
	{Name: "security", Weight: ptrTo(3.0), Guidance: "Untrusted input (bead text, mail, transcripts, tool-call strings, git refs, filenames, env) reaching a shell command, filesystem path, SQL, URL fetch, or subprocess environment without validation or canonicalisation; secrets, tokens or API keys written to logs, beads, commits, notes or transcripts; any weakening or bypass of the town guards (dangerous-command, polecat-path, container-suite, pr-workflow) or of gt done/refinery verification; fail-open error handling in a guard or gate. A confirmed finding here outranks style and mergeability and should block on its own."},
	{Name: "fail-open-branch", Weight: ptrTo(2.0), Guidance: "Any check, gate, guard, or measurement whose failure path emits the same value as its success path is a finding, even when the local change is correct."},
	{Name: "alarming-branch", Weight: ptrTo(3.0), Guidance: "Any check, guard, probe or alarm needs a test that drives its failing branch and asserts the alarm is observable: coverage of the passing path is not evidence, and a check whose failure is indistinguishable from its success is not a check. Scope is code whose purpose is to detect or report a condition — doctor checks, gates, probes, alarms, escalation paths, liveness tests, and the counters feeding them; a change reporting no verdict is out of scope, so this is not a coverage mandate. Can this check's failing branch produce the same value as its succeeding branch? If so it is not shippable; if you cannot tell, the test that answers it is missing. Rejected example: a doctor check returning OK when its probe cannot run, tested only on the healthy verdict — a broken probe reads exactly like a clean town. A confirmed finding blocks on its own; name the missing assertion."},
	{Name: "docs-and-comments", Weight: ptrTo(2.0), Guidance: "A change that alters behaviour updates every doc and comment that describes it. New or edited docs and comments follow every rule in the repository context (docs/writing-for-agents.md); cite the rule id in a finding. A stale or contradicted line in an agent-facing file (AGENTS.md, internal/templates/polecat-CLAUDE.md, a directive, a formula description, a plugin.md, docs/HOOKS.md) is major; elsewhere minor. This criterion moves the score; it does not block alone."},
	{Name: "instruction-proliferation", Weight: ptrTo(2.0), Guidance: "An added statement of a fact or instruction already stated in an agent-facing file (the list docs-and-comments names) is a finding even when the copies agree today: the offence is the second copy, and the drift it permits is otherwise charged to whichever later change touches one of them. The finding does not stand when the new copy links to the existing one as its source of truth. R2 in docs/writing-for-agents.md is the rule this enforces; cite it, the existing copy, and whether the new one points at it. In an agent-facing file this is major; the criterion moves the score and does not block alone."},
}

// TestShippedOMRubricCarriesScriptsAreDebtCriterion pins the criterion once,
// at weight 2, immediately after the earlier entries
// scriptsAreDebtEarlierEntries holds unchanged, and the guidance a reviewer
// needs to apply it: the severity split, the remedy the finding names, and
// the exemption for deletions.
func TestShippedOMRubricCarriesScriptsAreDebtCriterion(t *testing.T) {
	t.Parallel()
	r := shippedRubric(t)
	var found []int
	for i, c := range r.Rubric {
		if c.Name == scriptsAreDebtCriterion {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the shipped rubric %s carries %d %q criteria, want exactly 1: a new script holding testable logic is ungraded without it, and a duplicate double-counts the finding", shippedRubricPath(t), len(found), scriptsAreDebtCriterion)
	}
	idx := found[0]
	if idx != len(scriptsAreDebtEarlierEntries) {
		t.Errorf("%s sits at rubric[%d], want rubric[%d] immediately after the earlier entries gt-7rhdv left unchanged", scriptsAreDebtCriterion, idx, len(scriptsAreDebtEarlierEntries))
	}
	for i, want := range scriptsAreDebtEarlierEntries {
		if i >= len(r.Rubric) {
			t.Fatalf("the shipped rubric %s carries %d entries, want at least the %d that preceded %s", shippedRubricPath(t), len(r.Rubric), len(scriptsAreDebtEarlierEntries), scriptsAreDebtCriterion)
		}
		got := r.Rubric[i]
		if got.Name != want.Name {
			t.Errorf("rubric[%d] name = %q, want %q: gt-7rhdv appended %s without renaming or reordering an earlier criterion", i, got.Name, want.Name, scriptsAreDebtCriterion)
			continue
		}
		if got.Weight == nil || *got.Weight != *want.Weight {
			gotWeight := "nil"
			if got.Weight != nil {
				gotWeight = fmt.Sprintf("%g", *got.Weight)
			}
			t.Errorf("rubric[%d] (%s) weight = %s, want %g: gt-7rhdv's acceptance left every earlier entry unchanged", i, got.Name, gotWeight, *want.Weight)
			continue
		}
		if got.Guidance != want.Guidance {
			t.Errorf("rubric[%d] (%s) guidance = %q, want %q: gt-7rhdv's acceptance left every earlier entry unchanged", i, got.Name, got.Guidance, want.Guidance)
		}
	}
	c := r.Rubric[idx]
	if c.Weight == nil || *c.Weight != 2 {
		t.Errorf("%s weight = %v, want 2", scriptsAreDebtCriterion, c.Weight)
	}
	for _, want := range []string{"MAJOR", "MINOR", "port to Go with unit tests behind an Exec seam", "Never flag deleting or shrinking a script"} {
		if !strings.Contains(c.Guidance, want) {
			t.Errorf("%s guidance no longer carries %q: %q", scriptsAreDebtCriterion, want, c.Guidance)
		}
	}
}

// TestShippedOMThresholdIsGastownBar pins the approval bar the landing worker
// reads from the repo-root .om.json, through the reader that reads it: an
// OMThreshold that finds no file, no key or a non-numeric one reports 0, and no
// om verdict scores below 0, so a bar degraded into one of those shapes holds
// every approve to nothing and no other test in the tree would say so. It also
// catches the plainer one: the file still parses and the value moved.
//
// Sloan set gastown's bar to 0.75 on 2026-10-15 (gt-bnagt); a later deliberate
// change moves this pin with it.
func TestShippedOMThresholdIsGastownBar(t *testing.T) {
	t.Parallel()
	root := filepath.Dir(shippedRubricPath(t))
	got, err := OMThreshold(root)
	if err != nil {
		t.Fatalf("OMThreshold(%s): %v", root, err)
	}
	if got != 0.75 {
		t.Errorf("OMThreshold(%s) = %v, want 0.75: the bar is the value the landing worker holds an approve to, and OMThreshold reports a missing file or key as 0", root, got)
	}
}
