package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/formula"
)

func threeStepFormula() *formula.Formula {
	return &formula.Formula{
		Name: "mol-test-work",
		Steps: []formula.Step{
			{ID: "one", Title: "Load {{issue}}", Description: "Body of step one for {{issue}}.\nSecond line."},
			{ID: "two", Title: "Implement", Description: "Body of step two."},
			{ID: "three", Title: "Submit", Description: "Body of step three."},
		},
	}
}

func TestRenderFormulaChecklist_TitlesForAllStepsBodyForOne(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"issue": "gt-abc"}
	out := renderFormulaChecklist("mol-test-work", threeStepFormula(), vars, 1)

	for _, want := range []string{
		"**Formula Checklist** (3 steps from mol-test-work)",
		"### Step 1: Load gt-abc",
		"Body of step one for gt-abc.",
		"Second line.",
		"Step 2: Implement",
		"Step 3: Submit",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("checklist missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"Body of step two.", "Body of step three."} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("checklist must not include other step bodies, found %q:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "gt prime --step <N> --formula mol-test-work") {
		t.Fatalf("checklist must tell the agent how to fetch the other step bodies:\n%s", out)
	}
}

func TestRenderFormulaChecklist_FullStepSelectsBody(t *testing.T) {
	t.Parallel()
	out := renderFormulaChecklist("mol-test-work", threeStepFormula(), nil, 3)
	if !strings.Contains(out, "Body of step three.") {
		t.Fatalf("expected step 3 body:\n%s", out)
	}
	if strings.Contains(out, "Body of step one") {
		t.Fatalf("step 1 body must not render when step 3 is the full step:\n%s", out)
	}
}

func TestRenderFormulaChecklist_OutOfRangeFallsBackToStepOne(t *testing.T) {
	t.Parallel()
	out := renderFormulaChecklist("mol-test-work", threeStepFormula(), nil, 9)
	if !strings.Contains(out, "Body of step one") {
		t.Fatalf("out-of-range full step must fall back to step 1:\n%s", out)
	}
}

func TestRenderFormulaChecklist_EmptyFormula(t *testing.T) {
	t.Parallel()
	if out := renderFormulaChecklist("x", &formula.Formula{}, nil, 1); out != "" {
		t.Fatalf("expected empty output for a formula without steps, got %q", out)
	}
}

func TestPrimePayload_UnderBudgetRendersInOrder(t *testing.T) {
	t.Parallel()
	var p primePayload
	p.add("hook", 1, false, "HOOK\n")
	p.add("memories", 5, false, "MEMORIES\n")
	p.add("footer", 9, true, "FOOTER\n")

	if got := p.render(1000); got != "HOOK\nMEMORIES\nFOOTER\n" {
		t.Fatalf("unexpected render:\n%q", got)
	}
}

func TestPrimePayload_DropsLowestPriorityFirstAndNamesIt(t *testing.T) {
	t.Parallel()
	var p primePayload
	p.add("hook", 1, false, strings.Repeat("H", 40)+"\n")
	p.add("directives", 3, false, strings.Repeat("D", 40)+"\n")
	p.add("memories", 5, false, strings.Repeat("M", 40)+"\n")
	p.add("footer", 9, true, "FOOTER\n")

	got := p.render(120)
	if strings.Contains(got, "MMMM") {
		t.Fatalf("memories (lowest priority) should be dropped first:\n%s", got)
	}
	if !strings.Contains(got, "DDDD") || !strings.Contains(got, "HHHH") {
		t.Fatalf("higher-priority sections must survive:\n%s", got)
	}
	if !strings.Contains(got, "omitted to fit the hook budget: memories") {
		t.Fatalf("render must name the omitted section:\n%s", got)
	}
	if !strings.HasSuffix(got, "FOOTER\n") {
		t.Fatalf("footer must stay last:\n%s", got)
	}
}

func TestPrimePayload_KeepSectionsNeverDropped(t *testing.T) {
	t.Parallel()
	var p primePayload
	p.add("hook", 1, true, strings.Repeat("H", 100)+"\n")
	p.add("memories", 5, false, strings.Repeat("M", 100)+"\n")
	p.add("footer", 9, true, strings.Repeat("F", 100)+"\n")

	got := p.render(50)
	if !strings.Contains(got, "HHHH") || !strings.Contains(got, "FFFF") {
		t.Fatalf("keep sections must render even over budget:\n%s", got)
	}
	if strings.Contains(got, "MMMM") {
		t.Fatalf("droppable section must go:\n%s", got)
	}
}

func TestPrimePayload_ZeroBudgetMeansUnlimited(t *testing.T) {
	t.Parallel()
	var p primePayload
	p.add("a", 1, false, strings.Repeat("A", 5000))
	p.add("b", 2, false, strings.Repeat("B", 5000))
	if got := p.render(0); len(got) != 10000 {
		t.Fatalf("budget 0 must not drop anything, got %d chars", len(got))
	}
}

func TestPrimePayload_EmptySectionsAreSkipped(t *testing.T) {
	t.Parallel()
	var p primePayload
	p.add("a", 1, false, "")
	p.add("b", 2, false, "B\n")
	if got := p.render(1000); got != "B\n" {
		t.Fatalf("empty sections must not render or count, got %q", got)
	}
}

func TestPrimeStaticTextDelivered_NeedsEnvAndFile(t *testing.T) {
	t.Setenv(config.EnvSystemPromptFile, "")
	if primeStaticTextDelivered() {
		t.Fatal("unset env must mean not delivered")
	}
	path := filepath.Join(t.TempDir(), "system-prompt.md")
	t.Setenv(config.EnvSystemPromptFile, path)
	if primeStaticTextDelivered() {
		t.Fatal("env pointing at a missing file must mean not delivered")
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !primeStaticTextDelivered() {
		t.Fatal("env + existing file must mean delivered")
	}
}

func TestWriteSystemPromptFile_WritesOnlyOnChange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", ".claude", "system-prompt.md")
	changed, err := writeSystemPromptFile(path, "one")
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "one" {
		t.Fatalf("content = %q", got)
	}
	changed, err = writeSystemPromptFile(path, "one")
	if err != nil || changed {
		t.Fatalf("identical content must be a no-op: changed=%v err=%v", changed, err)
	}
	changed, err = writeSystemPromptFile(path, "two")
	if err != nil || !changed {
		t.Fatalf("changed content must rewrite: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "two" {
		t.Fatalf("content = %q", got)
	}
	if _, err := writeSystemPromptFile("", "x"); err != nil {
		t.Fatalf("empty path must be ignored, got %v", err)
	}
}

func TestStaticRoleText_TemplatePlusContextFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, "CONTEXT.md"), []byte("OPERATOR CONTEXT LINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town, WorkDir: town}
	text, fromTemplate, err := staticRoleText(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !fromTemplate {
		t.Fatal("polecat text must come from the role template")
	}
	for _, want := range []string{"Directory Discipline", "OPERATOR CONTEXT LINE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("static role text missing %q", want)
		}
	}
	if len(text) < 10000 {
		t.Fatalf("polecat role template should be the bulk of the static text, got %d chars", len(text))
	}
}

func TestUseCompactResumePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		source, handoff string
		delivered, want bool
	}{
		{"startup", "", false, false},
		{"startup", "", true, false},
		{"resume", "", false, true},
		{"resume", "", true, true},
		{"compact", "", false, true},
		{"compact", "", true, false}, // system prompt persists; re-send the dynamic payload
		{"startup", "compaction", false, true},
		{"startup", "compaction", true, false},
	}
	for _, c := range cases {
		if got := useCompactResumePath(c.source, c.handoff, c.delivered); got != c.want {
			t.Errorf("useCompactResumePath(%q,%q,%v) = %v, want %v", c.source, c.handoff, c.delivered, got, c.want)
		}
	}
}

func TestRenderFormulaStep_OneStepBody(t *testing.T) {
	t.Parallel()
	out, err := renderFormulaStep("mol-test-work", threeStepFormula(), map[string]string{"issue": "gt-abc"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Step 2: Implement") || !strings.Contains(out, "Body of step two.") {
		t.Fatalf("expected step 2 title and body:\n%s", out)
	}
	if strings.Contains(out, "Body of step one") {
		t.Fatalf("other bodies must not render:\n%s", out)
	}
	if _, err := renderFormulaStep("mol-test-work", threeStepFormula(), nil, 4); err == nil {
		t.Fatal("out-of-range step must error")
	}
}

func TestShowFormulaStepsFull_UsesBoundedChecklist(t *testing.T) {
	out := captureStdout(t, func() {
		showFormulaStepsFull("mol-polecat-work", t.TempDir(), "")
	})
	for _, want := range []string{"Step 1: Load context and verify assignment", "Step 2: Set up working branch", "Step 8: Submit work and self-clean", "gt prime --step <N> --formula " + "mol-polecat-work"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if len(out) > 6000 {
		t.Fatalf("bounded checklist for %s is %d chars; the full formula is ~19 KB", "mol-polecat-work", len(out))
	}
}

func TestCaptureOutput_ReturnsWhatFnPrinted(t *testing.T) {
	got := captureOutput(func() {
		fmt.Println("hello")
		fmt.Print(strings.Repeat("x", 100000)) // larger than any pipe buffer
	})
	if !strings.HasPrefix(got, "hello\n") || len(got) != len("hello\n")+100000 {
		t.Fatalf("captured %d chars, prefix %q", len(got), got[:min(len(got), 10)])
	}
}

func TestAssemblePrimePayload_HookedPolecatFitsAndLeadsWithWork(t *testing.T) {
	t.Setenv(config.EnvSystemPromptFile, "")
	town := t.TempDir()
	ctx := RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town, WorkDir: town}
	bead := &beads.Issue{
		ID:    "gt-zz9",
		Title: "Make the thing do the other thing",
		Description: "attached_molecule: gt-wisp-abc\nattached_formula: mol-polecat-work\nattached_vars: [\"issue=gt-zz9\"]\n\n" +
			strings.Repeat("A long description line that should be capped by the 5-line rule.\n", 20),
	}
	directive := strings.Repeat("Operator directive line.\n", 60) // ~1.5 KB

	payload := assemblePrimePayload(primeParts{
		session:    func() string { return "GAS TOWN role:myrig/polecats/nux pid:1 session:s\n" },
		hookedWork: func() string { return captureOutput(func() { _, _ = checkSlungWork(ctx, bead) }) },
		directives: func() string { return directive },
		memories:   func() string { return strings.Repeat("- memory-key: preview\n", 200) }, // ~4.6 KB, droppable
		startup:    func() string { return "**START NOW**\n" },
	}, "", false, true)
	out := payload.render(primeHookBudget)

	if len(out) > primeHookTestBudget {
		t.Fatalf("hooked polecat payload is %d chars, budget %d:\n%s", len(out), primeHookTestBudget, out[:2000])
	}
	preview := out[:min(len(out), 2000)]
	for _, want := range []string{"gt-zz9", "Step 1: Load context and verify assignment"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("first 2,000 chars must show the hooked work; missing %q:\n%s", want, preview)
		}
	}
	if strings.Contains(out, "**START NOW**") {
		t.Fatal("hooked work replaces the startup protocol; footer must not render")
	}
	if strings.Contains(out, "Set up working branch\n\nBody") {
		t.Fatal("only step 1 may carry a body")
	}
}

func TestAssemblePrimePayload_StaticTextAfterHookedWorkWhenNotDelivered(t *testing.T) {
	t.Parallel()
	payload := assemblePrimePayload(primeParts{
		session:    func() string { return "SESSION\n" },
		hookedWork: func() string { return "HOOKED\n" },
		memories:   func() string { return "MEMORIES\n" },
	}, "STATIC ROLE TEXT\n", true, true)
	out := payload.render(0)
	if strings.Index(out, "HOOKED") > strings.Index(out, "STATIC ROLE TEXT") || strings.Index(out, "SESSION") > strings.Index(out, "HOOKED") {
		t.Fatalf("order must be session, hooked work, static text, rest:\n%s", out)
	}
}

func TestReadHookSessionID_RecordsHookEventName(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	t.Setenv("GT_SESSION_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "")
	t.Setenv("GT_HOOK_SOURCE", "")
	go func() {
		_, _ = w.Write([]byte(`{"session_id":"abc","hook_event_name":"PreCompact","trigger":"auto"}` + "\n"))
		w.Close()
	}()
	primeHookEventName = ""
	id, _ := readHookSessionID()
	if id != "abc" {
		t.Fatalf("session id = %q", id)
	}
	if primeHookEventName != "PreCompact" {
		t.Fatalf("primeHookEventName = %q, want PreCompact", primeHookEventName)
	}
}

func TestOutputRoleDirectives_CapsLongDirective(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "directives"), 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("directive line number x\n", 200) // 4.8 KB
	if err := os.WriteFile(filepath.Join(town, "directives", "polecat.md"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	primeHookMode = true
	t.Cleanup(func() { primeHookMode = false })
	var sb strings.Builder
	outputRoleDirectives(RoleContext{Role: RolePolecat, TownRoot: town}, &sb, false)
	out := sb.String()
	if len(out) > primeDirectiveMaxChars+400 {
		t.Fatalf("directive output %d chars, cap %d", len(out), primeDirectiveMaxChars)
	}
	if !strings.Contains(out, "directive truncated") {
		t.Fatalf("truncated directive must say so:\n%s", out)
	}
}

func TestPrimeStepFormulaName(t *testing.T) {
	t.Parallel()
	hooked := &beads.Issue{ID: "gt-1", Description: "attached_formula: mol-polecat-work\n"}
	cases := []struct {
		name     string
		ctx      RoleContext
		bead     *beads.Issue
		explicit string
		want     string
	}{
		{"explicit wins", RoleContext{Role: RolePolecat}, hooked, "mol-custom", "mol-custom"},
		{"hooked attachment", RoleContext{Role: RolePolecat}, hooked, "", "mol-polecat-work"},
		{"witness patrol", RoleContext{Role: RoleWitness}, nil, "", constants.MolWitnessPatrol},
		{"refinery patrol", RoleContext{Role: RoleRefinery}, nil, "", constants.MolRefineryPatrol},
		{"deacon patrol", RoleContext{Role: RoleDeacon}, nil, "", constants.MolDeaconPatrol},
		{"nothing", RoleContext{Role: RolePolecat}, nil, "", ""},
	}
	for _, c := range cases {
		if got := primeStepFormulaName(c.ctx, c.bead, c.explicit); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestHookSessionBeaconLines_SilentForPreCompact(t *testing.T) {
	primeStructuredSessionStartOutput = false
	primeHookEventName = "PreCompact"
	t.Cleanup(func() { primeHookEventName = "" })
	if lines := hookSessionBeaconLines("abc", "startup"); len(lines) != 0 {
		t.Fatalf("PreCompact must print no beacon lines, got %v", lines)
	}
}

// TestPrimeRoleFixturesFitHookBudget is the acceptance guard for gt-layt: every
// role's dynamic payload, rendered against its largest realistic content,
// stays under primeHookTestBudget and leads with the work.
//
// A role whose work formula is attached renders its checklist through the
// hooked-work section (outputMoleculeWorkflow), so its molecule section stays
// empty; no role text appears here, because in production it rides the role's
// system-prompt file. For the dog (gt-mbuf) that file is what keeps the prime
// inside the hook budget — TestBuildStartupCommand_FirstDogSpawnCarriesSystemPromptFlag
// guards it.
func TestPrimeRoleFixturesFitHookBudget(t *testing.T) {
	t.Setenv(config.EnvSystemPromptFile, "")
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "directives"), 0o755); err != nil {
		t.Fatal(err)
	}
	directive := strings.Repeat("Operator directive line that steers this role.\n", 50) // >2,000 chars, capped
	memories := strings.Repeat("- some-memory-key: first sentence of the memory preview\n", 120)

	patrol := map[Role]string{RoleWitness: constants.MolWitnessPatrol, RoleRefinery: constants.MolRefineryPatrol, RoleDeacon: constants.MolDeaconPatrol}
	workFormula := map[Role]string{RolePolecat: "mol-polecat-work", RoleCrew: "mol-polecat-work", RoleDog: "mol-dog-reaper"}
	for _, role := range []Role{RolePolecat, RoleCrew, RoleDog, RoleWitness, RoleRefinery, RoleDeacon, RoleMayor} {
		role := role
		t.Run(string(role), func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(town, "directives", string(role)+".md"), []byte(directive), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := RoleContext{Role: role, Rig: "myrig", Polecat: "nux", TownRoot: town, WorkDir: town}
			if role == RoleDog {
				// A dog is town-level (no rig) and runs from its own kennel.
				ctx = RoleContext{Role: RoleDog, Polecat: "alpha", TownRoot: town, WorkDir: filepath.Join(town, "deacon", "dogs", "alpha")}
				if err := os.MkdirAll(ctx.WorkDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var bead *beads.Issue
			if f, ok := workFormula[role]; ok {
				bead = &beads.Issue{ID: "gt-fix1", Title: "A realistic bead title of ordinary length for the fixture",
					Description: "attached_molecule: gt-wisp-1\nattached_formula: " + f + "\nattached_vars: [\"issue=gt-fix1\"]\n" + strings.Repeat("desc line\n", 30)}
			}
			parts := primeParts{
				session:    func() string { return "GAS TOWN role:x pid:1 session:s\n" },
				hookedWork: func() string { return captureOutput(func() { _, _ = checkSlungWork(ctx, bead) }) },
				molecule: func() string {
					if f, ok := patrol[role]; ok {
						return captureOutput(func() { showFormulaStepsFull(f, town, "myrig") })
					}
					return ""
				},
				directives: func() string { return captureOutput(func() { outputRoleDirectives(ctx, os.Stdout, false) }) },
				memories:   func() string { return memories },
				startup:    func() string { return captureOutput(func() { outputStartupDirective(ctx) }) },
			}
			out := assemblePrimePayload(parts, "", false, bead != nil).render(primeHookBudget)
			if len(out) > primeHookTestBudget {
				t.Fatalf("%s payload %d chars > %d", role, len(out), primeHookTestBudget)
			}
			preview := out[:min(len(out), 2000)]
			if bead != nil && (!strings.Contains(preview, "gt-fix1") || !strings.Contains(preview, "Step 1:")) {
				t.Fatalf("%s preview must show the hooked bead and step 1:\n%s", role, preview)
			}
			if f, ok := patrol[role]; ok && !strings.Contains(out, "steps from "+f) {
				t.Fatalf("%s payload must include its patrol checklist:\n%s", role, out)
			}
		})
	}
}

func TestSystemPromptFile_EqualsStaticRoleText(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, "CONTEXT.md"), []byte("ctx"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := RoleContext{Role: RoleWitness, Rig: "myrig", TownRoot: town, WorkDir: town}
	want, _, err := staticRoleText(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := systemPromptPathFor(ctx)
	if path != filepath.Join(town, "myrig", "witness", ".claude", "system-prompt.md") {
		t.Fatalf("unexpected path %s", path)
	}
	if _, err := writeSystemPromptFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("file content must equal the static role text (err=%v, %d vs %d chars)", err, len(got), len(want))
	}
}

func TestRenderFormulaChecklist_CapsOversizedStepBody(t *testing.T) {
	t.Parallel()
	f := &formula.Formula{Steps: []formula.Step{
		{Title: "Huge", Description: strings.Repeat("a line of step body text\n", 400)}, // ~10 KB
		{Title: "Small", Description: "tiny"},
	}}
	out := renderFormulaChecklist("mol-big", f, nil, 1)
	if len(out) > primeStepBodyMaxChars+600 {
		t.Fatalf("checklist %d chars; step body must be capped near %d", len(out), primeStepBodyMaxChars)
	}
	if !strings.Contains(out, "step body truncated") || !strings.Contains(out, "gt prime --step 1 --formula mol-big") {
		t.Fatalf("truncated body must say so and name the fetch command:\n%s", out[len(out)-400:])
	}
}

func TestStaticRoleText_UnknownRoleKeepsFallbackContextAndContextFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, "CONTEXT.md"), []byte("OPERATOR CONTEXT LINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := RoleContext{Role: RoleUnknown, TownRoot: town, WorkDir: town}
	text, fromTemplate, err := staticRoleText(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fromTemplate {
		t.Fatal("unknown role has no template; fromTemplate must be false")
	}
	for _, want := range []string{"Could not determine specific role", "OPERATOR CONTEXT LINE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback static text missing %q:\n%s", want, text)
		}
	}
}

func TestAssemblePrimePayload_MailIsNeverDropped(t *testing.T) {
	t.Parallel()
	// `gt mail check --inject` ACKs deliveries as a side effect, so the mail
	// section must survive the budget or the agent loses mail for good.
	payload := assemblePrimePayload(primeParts{
		hookedWork: func() string { return strings.Repeat("H", 8000) + "\n" },
		mail:       func() string { return "## Mail\nMAIL BODY\n" },
		memories:   func() string { return strings.Repeat("M", 3000) + "\n" },
	}, "", false, true)
	out := payload.render(primeHookBudget)
	if !strings.Contains(out, "MAIL BODY") {
		t.Fatalf("mail must never be dropped by the budget:\n%s", out[len(out)-300:])
	}
	if strings.Contains(out, "MMMM") {
		t.Fatal("memories should have been dropped before mail")
	}
}

func TestCheckSlungWork_ContinuationModeDoesNotReannounce(t *testing.T) {
	primeContinuationMode = true
	t.Cleanup(func() { primeContinuationMode = false })
	town := t.TempDir()
	ctx := RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town, WorkDir: town}
	bead := &beads.Issue{ID: "gt-cont1", Title: "Continue me", Description: "attached_formula: mol-polecat-work\n"}
	out := captureOutput(func() { _, _ = checkSlungWork(ctx, bead) })
	if strings.Contains(out, "AUTONOMOUS WORK MODE") || strings.Contains(out, "Announce:") {
		t.Fatalf("post-compaction prime must not re-announce (GH#1965):\n%s", out[:min(len(out), 800)])
	}
	for _, want := range []string{"CONTINUE HOOKED WORK", "gt-cont1", "Step 1:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("continuation output missing %q:\n%s", want, out[:min(len(out), 800)])
		}
	}
}

func TestPrimeStepVars_FollowTheResolvedFormula(t *testing.T) {
	t.Parallel()
	// A witness whose hooked bead has no attached formula reads its patrol
	// formula, so the vars must be the patrol vars, not the (empty) attachment vars.
	ctx := RoleContext{Role: RoleWitness, Rig: "myrig", TownRoot: t.TempDir()}
	bead := &beads.Issue{ID: "gt-x", Description: "no attachment here\n"}
	name := primeStepFormulaName(ctx, bead, "")
	if name != constants.MolWitnessPatrol {
		t.Fatalf("name = %q", name)
	}
	vars := primeStepVars(ctx, bead, name)
	found := false
	for _, v := range vars {
		if v == "rig=myrig" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected witness patrol vars (rig=myrig), got %v", vars)
	}
	// With an attached formula the attachment vars win.
	bead2 := &beads.Issue{ID: "gt-y", Description: "attached_formula: mol-polecat-work\nattached_vars: [\"issue=gt-y\"]\n"}
	vars2 := primeStepVars(RoleContext{Role: RolePolecat}, bead2, "mol-polecat-work")
	if len(vars2) == 0 || vars2[0] != "issue=gt-y" {
		t.Fatalf("expected attachment vars, got %v", vars2)
	}
}

func TestSystemPromptPathFor_PolecatIsPerAgent(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	got := systemPromptPathFor(RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town})
	want := filepath.Join(town, "myrig", "polecats", ".claude", "system-prompt-nux.md")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestOutputRoleDirectives_UncappedOutsideHookMode(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "directives"), 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("directive line number x\n", 200)
	if err := os.WriteFile(filepath.Join(town, "directives", "polecat.md"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	primeHookMode = false
	var sb strings.Builder
	outputRoleDirectives(RoleContext{Role: RolePolecat, TownRoot: town}, &sb, false)
	if strings.Contains(sb.String(), "directive truncated") || len(sb.String()) < len(long) {
		t.Fatalf("plain gt prime must show the whole directive (%d chars rendered)", len(sb.String()))
	}
}

func TestFindAgentWorkWithAttempts_SingleAttemptDoesNotBackOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	ctx := RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town, WorkDir: town}
	start := time.Now()
	_, _ = findAgentWorkWithAttempts(ctx, 1)
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("single attempt took %v; the retry backoff must not apply", d)
	}
}
