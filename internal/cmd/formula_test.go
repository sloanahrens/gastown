package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/formula"
)

// TestDispatchWorkflowStepsReportsAFailedSling: a step whose `gt sling` fails
// never reaches a polecat, so the run must return a non-nil error naming it and
// must not print that the workflow was dispatched. Reporting success over a
// step nobody picked up leaves the workflow looking dispatched when the work is
// not running (gt-abr6v).
func TestDispatchWorkflowStepsReportsAFailedSling(t *testing.T) {
	t.Parallel()

	fake := beadsfake.New(beadsfake.WithPrefix("gt"))
	f := &cookedFormula{Steps: []cookedStep{
		{ID: "first", Title: "First step"},
		{ID: "second", Title: "Second step", Needs: []string{"first"}},
	}}

	var slung int
	run := &workflowStepRun{
		formula:    f,
		workflowID: "hq-wf-test",
		rigPrefix:  "gt",
		targetRig:  "gastown",
		rigBd:      fake,
		track:      func(string) error { return nil },
		sling: func([]string) error {
			slung++
			return errors.New("no polecat accepted the work")
		},
	}

	var out bytes.Buffer
	run.createBeads(&out)
	err := run.dispatch(&out)

	if err == nil {
		t.Fatalf("a failed sling returned no error; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("error %q does not name the step that failed to sling", err)
	}
	if strings.Contains(out.String(), "Workflow dispatched") {
		t.Errorf("a failed sling still printed the workflow dispatched:\n%s", out.String())
	}
	if slung != 1 {
		t.Errorf("slinged %d step(s), want 1 — only the dependency-free step is ready", slung)
	}
}

// TestDispatchWorkflowStepsReportsAnUnwiredStep: a step whose bead exists but
// whose needs could not be wired is not ready, so the run must not sling it and
// must name it in the error (gt-abr6v).
func TestDispatchWorkflowStepsReportsAnUnwiredStep(t *testing.T) {
	t.Parallel()

	fake := beadsfake.New(beadsfake.WithPrefix("gt"))
	// "second" needs a step the formula never declares: its bead is never
	// created, so there is nothing to depend on.
	f := &cookedFormula{Steps: []cookedStep{
		{ID: "second", Title: "Second step", Needs: []string{"missing"}},
	}}

	var slung int
	run := &workflowStepRun{
		formula:    f,
		workflowID: "hq-wf-test",
		rigPrefix:  "gt",
		targetRig:  "gastown",
		rigBd:      fake,
		track:      func(string) error { return nil },
		sling: func([]string) error {
			slung++
			return nil
		},
	}

	var out bytes.Buffer
	run.createBeads(&out)
	err := run.dispatch(&out)

	if err == nil {
		t.Fatalf("an unwired step returned no error; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "second") {
		t.Errorf("error %q does not name the unwired step", err)
	}
	if slung != 0 {
		t.Errorf("slinged %d step(s), want 0 — an unwired step is not ready", slung)
	}
	if strings.Contains(out.String(), "Workflow dispatched") {
		t.Errorf("an unwired step still printed the workflow dispatched:\n%s", out.String())
	}
}

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

	t.Run("malformed rigs.json surfaces the load error", func(t *testing.T) {
		t.Parallel()
		root := makeWorkspace(t)
		path := filepath.Join(root, "mayor", "rigs.json")
		if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
			t.Fatalf("write rigs.json: %v", err)
		}

		// A rigs.json that does not parse is damage, not an empty registry:
		// autoInferRig reports the load failure rather than reading the town
		// as one with no rigs (gt-52mgl).
		_, _, err := autoInferRig(root)
		if err == nil {
			t.Fatal("expected error for malformed rigs.json, got nil")
		}
		if strings.Contains(err.Error(), "no rigs registered") {
			t.Errorf("malformed rigs.json read as an empty town, got: %v", err)
		}
		if !strings.Contains(err.Error(), "loading rigs config") {
			t.Errorf("expected the rigs-config load error, got: %v", err)
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
