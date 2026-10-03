package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/formula"
)

// TestAutoInferRig verifies the rig auto-selection logic used when --rig is
// not provided and cwd-based detection finds nothing (e.g. Deacon at HQ level
// on a non-default install where "gastown" rig does not exist).
func TestAutoInferRig(t *testing.T) {
	t.Parallel()

	makeWorkspace := func(t *testing.T) (root string) {
		t.Helper()
		root = t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
			t.Fatalf("mkdir mayor: %v", err)
		}
		return root
	}

	writeRigsJSON := func(t *testing.T, root string, rigNames []string) {
		t.Helper()
		cfg := &config.RigsConfig{
			Version: 1,
			Rigs:    make(map[string]config.RigEntry),
		}
		for _, name := range rigNames {
			cfg.Rigs[name] = config.RigEntry{}
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal rigs.json: %v", err)
		}
		path := filepath.Join(root, "mayor", "rigs.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write rigs.json: %v", err)
		}
	}

	t.Run("single rig auto-selects", func(t *testing.T) {
		t.Parallel()
		root := makeWorkspace(t)
		rigDir := filepath.Join(root, "myrig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatalf("mkdir myrig: %v", err)
		}
		writeRigsJSON(t, root, []string{"myrig"})

		name, path, err := autoInferRig(root)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if name != "myrig" {
			t.Errorf("name = %q, want %q", name, "myrig")
		}
		if path != rigDir {
			t.Errorf("path = %q, want %q", path, rigDir)
		}
	})

	t.Run("multiple rigs require explicit --rig", func(t *testing.T) {
		t.Parallel()
		root := makeWorkspace(t)
		for _, name := range []string{"rig1", "rig2"} {
			if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", name, err)
			}
		}
		writeRigsJSON(t, root, []string{"rig1", "rig2"})

		_, _, err := autoInferRig(root)
		if err == nil {
			t.Fatal("expected error for multiple rigs, got nil")
		}
		if !strings.Contains(err.Error(), "cannot determine target rig") {
			t.Errorf("expected rig-detection error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "--rig=NAME") {
			t.Errorf("error should suggest --rig=NAME, got: %v", err)
		}
		if !strings.Contains(err.Error(), "rig1") || !strings.Contains(err.Error(), "rig2") {
			t.Errorf("error should list available rigs, got: %v", err)
		}
	})

	t.Run("no rigs registered", func(t *testing.T) {
		t.Parallel()
		root := makeWorkspace(t)
		writeRigsJSON(t, root, []string{})

		_, _, err := autoInferRig(root)
		if err == nil {
			t.Fatal("expected error for no rigs, got nil")
		}
		if !strings.Contains(err.Error(), "no rigs registered") {
			t.Errorf("error should mention no rigs registered, got: %v", err)
		}
		if !strings.Contains(err.Error(), "--rig=NAME") {
			t.Errorf("error should suggest --rig=NAME, got: %v", err)
		}
	})

	t.Run("malformed rigs.json surfaces error", func(t *testing.T) {
		t.Parallel()
		root := makeWorkspace(t)
		path := filepath.Join(root, "mayor", "rigs.json")
		if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
			t.Fatalf("write rigs.json: %v", err)
		}

		// discoverRigsForTownRoot silently falls back to an empty config on
		// parse error, so autoInferRig surfaces the "no rigs registered" path.
		_, _, err := autoInferRig(root)
		if err == nil {
			t.Fatal("expected error for malformed rigs.json, got nil")
		}
		if !strings.Contains(err.Error(), "no rigs registered") {
			t.Errorf("expected no-rigs error (fallback from malformed JSON), got: %v", err)
		}
	})
}

// TestFormulaSyncMessage verifies 'gt formula sync' delivers embedded
// formulas to a fresh town root and is idempotent on a second run — the
// behavior gt-n6c requires so rebuilds can deliver formula fixes without a
// hand-copy.
func TestFormulaSyncMessage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	msg, err := formulaSyncMessage(root)
	if err != nil {
		t.Fatalf("formulaSyncMessage: %v", err)
	}
	if !strings.Contains(msg, "Synced formulas") {
		t.Errorf("first sync on fresh town root: expected a sync summary, got: %q", msg)
	}

	msg2, err := formulaSyncMessage(root)
	if err != nil {
		t.Fatalf("formulaSyncMessage (second run): %v", err)
	}
	if !strings.Contains(msg2, "up to date") {
		t.Errorf("second sync should be a no-op, got: %q", msg2)
	}
}

// TestBuildConvoyLegSlingArgs: a leg's sling carries the bead, the rig, the
// context and the optional agent/review flags — and no --no-convoy, since a
// sling creates no convoy to suppress (gt-gzhin.4).
func TestBuildConvoyLegSlingArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		agent      string
		reviewOnly bool
		wantFlags  []string
	}{
		{"no agent no review", "", false, nil},
		{"with agent", "claude", false, []string{"--agent", "claude"}},
		{"review only", "", true, []string{"--review-only"}},
		{"agent and review", "gemini", true, []string{"--agent", "gemini", "--review-only"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildConvoyLegSlingArgs("bead-1", "myrig", "desc", "title", tt.agent, tt.reviewOnly)
			for _, want := range tt.wantFlags {
				if !slices.Contains(got, want) {
					t.Errorf("buildConvoyLegSlingArgs() missing %q in %v", want, got)
				}
			}
			if got[0] != "sling" {
				t.Errorf("first arg must be 'sling', got %q", got[0])
			}
			if slices.Contains(got, "--no-convoy") {
				t.Errorf("buildConvoyLegSlingArgs() still passes --no-convoy: %v", got)
			}
		})
	}
}

// TestBuildWorkflowStepSlingArgs: a step's sling carries the bead, the rig,
// the context and the optional agent — and no --no-convoy (gt-gzhin.4).
func TestBuildWorkflowStepSlingArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		agent string
	}{
		{"no agent", ""},
		{"with agent", "claude-haiku"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildWorkflowStepSlingArgs("bead-2", "myrig", "desc", "title", tt.agent)
			if slices.Contains(got, "--no-convoy") {
				t.Errorf("buildWorkflowStepSlingArgs() still passes --no-convoy: %v", got)
			}
			if got[0] != "sling" {
				t.Errorf("first arg must be 'sling', got %q", got[0])
			}
			if tt.agent != "" && !slices.Contains(got, tt.agent) {
				t.Errorf("buildWorkflowStepSlingArgs() missing agent %q in %v", tt.agent, got)
			}
		})
	}
}

func TestResolveFormulaLegAgent_Precedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		legAgent string
		cliAgent string
		want     string
	}{
		{"all empty", "", "", ""},
		{"cli only", "", "codex", "codex"},
		{"leg only", "claude-haiku", "", "claude-haiku"},
		{"leg overrides cli", "claude-haiku", "codex", "claude-haiku"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveFormulaLegAgent(tt.legAgent, tt.cliAgent)
			if got != tt.want {
				t.Errorf("resolveFormulaLegAgent(%q, %q) = %q, want %q",
					tt.legAgent, tt.cliAgent, got, tt.want)
			}
		})
	}
}

// convoyTree is bd's cooked tree for a convoy formula shaped like design:
// run settings as vars, two legs and a synthesis step marked in metadata.
const convoyTree = `{
  "formula": "design", "type": "convoy", "description": "Design",
  "vars": [
    {"name": "base_prompt", "default": "Leg {{.leg.id}}", "value": "Leg {{.leg.id}}"},
    {"name": "output_directory", "default": ".designs/{{.review_id}}", "value": ".designs/{{.review_id}}"},
    {"name": "output_leg_pattern", "default": "{{.leg.id}}.md", "value": "{{.leg.id}}.md"},
    {"name": "output_synthesis", "default": "design-doc.md", "value": "design-doc.md"},
    {"name": "problem", "required": true, "default": null, "value": "set problem", "provided": true},
    {"name": "context", "default": null, "value": null},
    {"name": "review_only", "default": "true", "value": "true"}
  ],
  "unresolved_vars": [], "warnings": [],
  "steps": [
    {"id": "api", "title": "API", "description": "api body", "needs": [], "children": [],
     "metadata": {"focus": "Interfaces", "agent": "codex"}},
    {"id": "data", "title": "Data", "description": "data body", "needs": [], "children": [],
     "metadata": {"focus": "Storage"}},
    {"id": "synthesis", "title": "Synthesis", "description": "Combine {{.output.directory}}", "needs": ["api", "data"], "children": [],
     "metadata": {"convoy": "synthesis"}}
  ]
}`

// TestConvoyPlanFrom_ReadsLegsSynthesisAndSettings: gt formula run reads a
// convoy from bd's cooked tree (gt-fd2cu.1.1): the step metadata marks the
// synthesis and every other step is a leg; prompt, output files and
// review_only come from vars.
func TestConvoyPlanFrom_ReadsLegsSynthesisAndSettings(t *testing.T) {
	t.Parallel()
	p := convoyPlanFrom(cookedFixture(t, convoyTree))

	want := []convoyLeg{
		{ID: "api", Title: "API", Focus: "Interfaces", Description: "api body", Agent: "codex", ReviewOnly: true},
		{ID: "data", Title: "Data", Focus: "Storage", Description: "data body", ReviewOnly: true},
	}
	if !reflect.DeepEqual(p.Legs, want) {
		t.Errorf("legs = %+v, want %+v", p.Legs, want)
	}
	if p.Synthesis == nil || p.Synthesis.ID != "synthesis" || !slices.Equal(p.Synthesis.Needs, []string{"api", "data"}) {
		t.Errorf("synthesis = %+v", p.Synthesis)
	}
	if p.BasePrompt != "Leg {{.leg.id}}" || p.OutputDir != ".designs/{{.review_id}}" ||
		p.LegPattern != "{{.leg.id}}.md" || p.SynthesisFile != "design-doc.md" {
		t.Errorf("settings = %q %q %q %q", p.BasePrompt, p.OutputDir, p.LegPattern, p.SynthesisFile)
	}
}

// TestFormulaRunVars_ResolvedVarsThenUndeclaredSets: the template context has
// every var bd gave a value and the --set pairs the formula does not declare;
// a var without a value stays out, so a template reads it as missing.
func TestFormulaRunVars_ResolvedVarsThenUndeclaredSets(t *testing.T) {
	t.Parallel()
	got := formulaRunVars(cookedFixture(t, convoyTree), []string{"problem=set problem", "extra=x"})
	if got["problem"] != "set problem" || got["extra"] != "x" || got["output_synthesis"] != "design-doc.md" {
		t.Errorf("vars = %v", got)
	}
	if _, ok := got["context"]; ok {
		t.Errorf("context has no value but is in the context: %v", got)
	}
}

func TestParseSetVarsPreservesMultilineValues(t *testing.T) {
	t.Parallel()

	got := parseSetVars([]string{"problem=First\n\nSecond", "context=a=b"})
	if got["problem"] != "First\n\nSecond" {
		t.Fatalf("problem = %q, want multiline value", got["problem"])
	}
	if got["context"] != "a=b" {
		t.Fatalf("context = %q, want value with equals", got["context"])
	}
}

func TestWorkflowStepTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		step cookedStep
		want string
	}{
		{name: "default rig", step: cookedStep{}, want: "gastown"},
		{name: "explicit rig", step: cookedStep{Metadata: map[string]any{"target": "rig"}}, want: "gastown"},
		{name: "mayor", step: cookedStep{Metadata: map[string]any{"target": "mayor"}}, want: "mayor"},
		{name: "crew path", step: cookedStep{Metadata: map[string]any{"target": "gastown/crew/alex"}}, want: "gastown/crew/alex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workflowStepTarget(tt.step, "gastown"); got != tt.want {
				t.Fatalf("workflowStepTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkflowStepDescriptionAddsTargetMetadata(t *testing.T) {
	t.Parallel()

	got := workflowStepDescription(cookedStep{Description: "Line one\n\nLine two", Metadata: map[string]any{"target": "mayor"}})
	want := "workflow_target: mayor\n\nLine one\n\nLine two"
	if got != want {
		t.Fatalf("workflowStepDescription() = %q, want %q", got, want)
	}
}

func TestWorkflowStepTargetFromDescription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		description string
		want        string
	}{
		{name: "no metadata", description: "Body only", want: ""},
		{name: "mayor", description: "workflow_target: mayor\n\nBody", want: "mayor"},
		{name: "rig alias", description: "workflow_target: rig\n\nBody", want: "gastown"},
		{name: "path target", description: "workflow_target: gastown/crew/alex\n\nBody", want: "gastown/crew/alex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workflowStepTargetFromDescription(tt.description, "gastown"); got != tt.want {
				t.Fatalf("workflowStepTargetFromDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAttachmentFormulaVarsPrefersAttachedVars(t *testing.T) {
	t.Parallel()

	attachment := &beads.AttachmentFields{
		AttachedVars: []string{"problem=First\n\nSecond"},
		FormulaVars:  "problem=First\n\ntruncated",
	}
	got := attachmentFormulaVars(attachment)
	want := []string{"problem=First\n\nSecond"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attachmentFormulaVars() = %#v, want %#v", got, want)
	}
}

func TestAttachmentFormulaVarsRoundTripsPersistedVars(t *testing.T) {
	t.Parallel()

	desc := beads.SetAttachmentFields(&beads.Issue{Description: "Body"}, &beads.AttachmentFields{
		AttachedFormula: "mol-polecat-work",
		AttachedVars:    []string{"feature=Attached Feature"},
		FormulaVars:     "feature=Persisted Feature\nissue=gt-123\nbase_branch=main",
	})
	attachment := beads.ParseAttachmentFields(&beads.Issue{Description: desc})
	got := attachmentFormulaVars(attachment)
	want := []string{"feature=Attached Feature", "issue=gt-123", "base_branch=main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attachmentFormulaVars() = %#v, want %#v\nDescription:\n%s", got, want, desc)
	}
}

func TestRenderTemplateUsesGoDotSyntax(t *testing.T) {
	t.Parallel()

	ctx := map[string]interface{}{"issue": "gt-123"}
	got, err := renderTemplate("bd show {{.issue}}", ctx)
	if err != nil {
		t.Fatalf("renderTemplate() dotted syntax error: %v", err)
	}
	if got != "bd show gt-123" {
		t.Fatalf("renderTemplate() = %q, want %q", got, "bd show gt-123")
	}

	if _, err := renderTemplate("bd show {{issue}}", ctx); err == nil {
		t.Fatal("renderTemplate() with bare syntax succeeded; want Go template error")
	}
}

func TestFormulaRunExamplesUseSetVars(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"design", "mol-idea-to-plan"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			content, err := formula.GetEmbeddedFormulaContent(name)
			if err != nil {
				t.Fatalf("GetEmbeddedFormulaContent(%s): %v", name, err)
			}
			text := string(content)
			for _, bad := range []string{"--problem=", "--context=", "--plan="} {
				if strings.Contains(text, bad) {
					t.Fatalf("%s still contains invalid gt formula run flag %q", name, bad)
				}
			}
		})
	}

	idea, err := formula.GetEmbeddedFormulaContent("mol-idea-to-plan")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(mol-idea-to-plan): %v", err)
	}
	ideaText := string(idea)
	if strings.Contains(ideaText, "<design-id>") {
		t.Fatal("mol-idea-to-plan still references stale <design-id> output paths")
	}
	if strings.Contains(ideaText, ".designs/<review-id>") {
		t.Fatal("mol-idea-to-plan conflates design output ID with PRD review ID")
	}
	for _, want := range []string{
		"--set problem=\"$PROBLEM\"",
		"--set context=\"See .prd-reviews/{{review_id}}/prd-draft.md. $CONTEXT\"",
		"--set context=\"PRD with clarifications: .prd-reviews/{{review_id}}/prd-draft.md. $CONTEXT\"",
		".designs/<design-review-id>/design-doc.md",
	} {
		if !strings.Contains(ideaText, want) {
			t.Fatalf("mol-idea-to-plan missing %q", want)
		}
	}

	design, err := formula.GetEmbeddedFormulaContent("design")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(design): %v", err)
	}
	if !strings.Contains(string(design), "gt formula run design --set problem=") {
		t.Fatal("design usage examples do not mention --set problem=")
	}
}

// TestFormulaSyncMessage_ReplacesAndNamesDrift: the binary is canonical
// (gt-fd2cu.3), so a town copy whose hash gt never wrote is replaced, and the
// summary names it so the hand edit is not lost silently (gt-dt7r).
func TestFormulaSyncMessage_ReplacesAndNamesDrift(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	if _, err := formulaSyncMessage(root); err != nil {
		t.Fatalf("formulaSyncMessage (initial): %v", err)
	}

	// Hand-edit one formula the way a human debugging a stuck patrol would.
	const edited = "mol-polecat-work.formula.toml"
	path := filepath.Join(root, ".beads", "formulas", edited)
	if err := os.WriteFile(path, []byte("# hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	msg, err := formulaSyncMessage(root)
	if err != nil {
		t.Fatalf("formulaSyncMessage: %v", err)
	}
	for _, want := range []string{edited, "were replaced", "not one gt wrote", "gastown source"} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary lacks %q:\n%s", want, msg)
		}
	}

	embedded, err := formula.GetEmbeddedFormulaContent(edited)
	if err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(path); string(content) != string(embedded) {
		t.Error("sync left the drifted copy in place")
	}

	// root is a bare temp dir with no gt source checkout under it, so drift must
	// come back unknown, and it must say so of root. That is what keeps this
	// test hermetic: resolving the ambient checkout instead is what used to
	// reach the network (gt-vxjz).
	if !strings.Contains(msg, "is UNKNOWN") || !strings.Contains(msg, root) {
		t.Errorf("drift should be unknown for the town root %s, with no ambient checkout consulted:\n%s", root, msg)
	}
}

// TestBuildFormulaSyncReport_DryRunReplacesNothing: a dry run names the drift
// a real run would replace and writes nothing.
func TestBuildFormulaSyncReport_DryRunReplacesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := formulaSyncMessage(root); err != nil {
		t.Fatalf("formulaSyncMessage (initial): %v", err)
	}
	const edited = "mol-polecat-work.formula.toml"
	path := filepath.Join(root, ".beads", "formulas", edited)
	if err := os.WriteFile(path, []byte("# hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := buildFormulaSyncReport(root, formula.SyncOptions{DryRun: true})
	if err != nil {
		t.Fatalf("buildFormulaSyncReport: %v", err)
	}
	if len(report.ReplacedDrift) != 1 || report.ReplacedDrift[0] != edited {
		t.Errorf("ReplacedDrift = %v, want [%s]", report.ReplacedDrift, edited)
	}
	if msg := formatFormulaSyncReport(report); !strings.Contains(msg, "would be replaced") {
		t.Errorf("a dry run must not claim it replaced anything:\n%s", msg)
	}
	if content, _ := os.ReadFile(path); string(content) != "# hand-edited\n" {
		t.Error("a dry run overwrote the drifted copy")
	}
}

// TestFormatFormulaSyncReport_NamesOrphanedCopies: a town copy whose formula
// left the binary is still listed by gt formula list, so the summary names it
// and says how to remove it (gt-zggoh); so is a file gt never wrote (gt-fd2cu.3).
func TestFormatFormulaSyncReport_NamesOrphanedCopies(t *testing.T) {
	t.Parallel()
	report := &formulaSyncReport{
		UpToDate:     3,
		Orphaned:     []string{"mol-refinery-patrol.formula.toml"},
		Unowned:      []string{"mol-polecat-work.formula.toml.bak-20260921-resync"},
		DriftChecked: true,
		CompareRef:   "origin/main",
	}
	msg := formatFormulaSyncReport(report)
	for _, want := range []string{"mol-refinery-patrol.formula.toml", "no longer shipped",
		"mol-polecat-work.formula.toml.bak-20260921-resync", "not in gastown source"} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary lacks %q:\n%s", want, msg)
		}
	}
}
