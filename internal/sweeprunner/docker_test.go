package sweeprunner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// goldenSpec is the fixed Spec testdata/docker-render.golden.json was rendered
// from: gastown at one commit, the limits the design gives a sweep, dind on.
// Every field is a literal so the file never follows the host.
func goldenSpec() Spec {
	return Spec{
		Rig:      "gastown",
		SHA:      "4f2a9c1e8b7d6a5f4e3d2c1b0a9f8e7d6c5b4a39",
		Image:    "gt-polecat:gt-74ix9",
		CPUs:     8,
		MemoryGB: 24,
		Jobs:     4,
		OutDir:   "/Users/ops/gt/.runtime/sweep/out",
		RepoDir:  "/Users/ops/gt/gastown/.repo.git",
		Dind:     true,
		RunID:    "mvv8asko",
		Context:  "colima-gt-sbx",
	}
}

// renderGolden is a plan as the golden file holds it: one object per step, argv
// quoted exactly.
func renderGolden(t *testing.T, plan Plan) string {
	t.Helper()
	type goldenStep struct {
		Argv     []string `json:"argv"`
		Teardown bool     `json:"teardown,omitempty"`
	}
	steps := make([]goldenStep, len(plan.Steps))
	for i, step := range plan.Steps {
		steps[i] = goldenStep{Argv: step.Argv, Teardown: step.Teardown}
	}
	encoded, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		t.Fatalf("marshalling the plan: %v", err)
	}
	return string(encoded) + "\n"
}

// TestDockerRenderGolden renders one fixed Spec and compares it to the checked-in
// file, so any change to a step is a change the reviewer sees.
func TestDockerRenderGolden(t *testing.T) {
	t.Parallel()
	plan, err := Docker{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	got := renderGolden(t, plan)
	golden, err := os.ReadFile(filepath.Join("testdata", "docker-render.golden.json"))
	if err != nil {
		t.Fatalf("reading the golden file: %v\nrendered:\n%s", err, got)
	}
	if got != string(golden) {
		t.Errorf("rendered plan differs from testdata/docker-render.golden.json\ngot:\n%s\nwant:\n%s", got, golden)
	}

	t.Run("without dind", func(t *testing.T) {
		t.Parallel()
		spec := goldenSpec()
		spec.Dind = false
		spec.RunID = ""
		plain, err := Docker{}.Render(spec)
		if err != nil {
			t.Fatalf("rendering a spec without dind: %v", err)
		}
		if len(plain.Steps) != 2 {
			t.Fatalf("a run without dind rendered %d steps, want the runner and its removal: %v", len(plain.Steps), plain.Steps)
		}
		if _, ok := findStep(plain, "network"); ok {
			t.Error("a run without dind rendered a network step")
		}
		for _, step := range plain.Steps {
			if slices.Contains(step.Argv, templates.DindImage) {
				t.Errorf("a run without dind rendered the sidecar: %v", step.Argv)
			}
			if slices.Contains(step.Argv, "DOCKER_HOST=tcp://"+dindAlias+":"+dindPort) {
				t.Errorf("a run without dind points the runner at an engine it did not start: %v", step.Argv)
			}
		}
	})
}

// TestDockerRenderPassesTheTiersItWasGiven keeps the run spec whole: the runner
// tests the tiers the Spec names, in the Spec's order, as many at a time as it
// names.
func TestDockerRenderPassesTheTiersItWasGiven(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	spec.Tiers = []string{"race", "shell"}
	spec.Jobs = 6
	plan, err := Docker{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering a spec with named tiers: %v", err)
	}
	runner, ok := stepNamed(plan, runnerName(spec))
	if !ok {
		t.Fatalf("the plan renders no step named %q: %v", runnerName(spec), plan.Steps)
	}
	if got, _ := envValue(runner.Argv, EnvTiers); got != "race,shell" {
		t.Errorf("%s = %q, want the tiers in the Spec's order", EnvTiers, got)
	}
	if got, _ := envValue(runner.Argv, EnvJobs); got != "6" {
		t.Errorf("%s = %q, want the job count the Spec names", EnvJobs, got)
	}
}

// TestDockerRenderNeverMountsTheSocketAndOnlyDindIsPrivileged holds the isolation
// the design promises: the run reads the repo read-only, writes /out, publishes
// nothing, and has no Docker of its own except the sidecar it starts.
func TestDockerRenderNeverMountsTheSocketAndOnlyDindIsPrivileged(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	plan, err := Docker{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	privileged := 0
	for _, step := range plan.Steps {
		for i, arg := range step.Argv {
			if strings.Contains(arg, "docker.sock") {
				t.Errorf("step %v mounts the host's Docker socket at %q", step.Argv, arg)
			}
			switch arg {
			case "-p", "--publish", "-P", "--publish-all":
				t.Errorf("step %v publishes a port with %s", step.Argv, arg)
			case "--network":
				if i+1 < len(step.Argv) && step.Argv[i+1] == "host" {
					t.Errorf("step %v joins the host's network", step.Argv)
				}
			}
			if strings.HasPrefix(arg, "--publish=") || arg == "--network=host" {
				t.Errorf("step %v weakens the run's isolation with %q", step.Argv, arg)
			}
			if arg == "--privileged" {
				privileged++
				if !slices.Contains(step.Argv, templates.DindImage) {
					t.Errorf("step %v is privileged but does not start the sidecar", step.Argv)
				}
			}
		}
	}
	if privileged != 1 {
		t.Errorf("rendered %d privileged steps, want the one sidecar", privileged)
	}

	mounts := map[string]string{}
	for _, step := range plan.Steps {
		for i, arg := range step.Argv {
			if arg == "--volume" && i+1 < len(step.Argv) {
				host, container, _ := strings.Cut(step.Argv[i+1], ":")
				mounts[container] = host
			}
		}
		if !step.Teardown && step.Argv[3] == "network" && !slices.Contains(step.Argv, "--internal") {
			t.Errorf("the network step %v is not internal, so the run reaches a host network", step.Argv)
		}
	}
	if !strings.HasSuffix(mounts[RepoMount+":ro"], spec.RepoDir) {
		t.Errorf("mount at %s = %q, want the repo %q read-only", RepoMount, mounts[RepoMount+":ro"], spec.RepoDir)
	}
	if _, readOnly := mounts[OutputDir+":ro"]; readOnly {
		t.Errorf("the output mount %q is read-only; the run writes result.json there", mounts[OutputDir+":ro"])
	}
	if got := mounts[OutputDir]; got != spec.OutDir {
		t.Errorf("mount at %s = %q, want the output directory %q", OutputDir, got, spec.OutDir)
	}
}

// TestSpecValidation refuses each bad field with an error that names it, through
// both Validate and the render a caller reaches first.
func TestSpecValidation(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		field string
		set   func(*Spec)
	}{
		{"rig escapes its directory", "rig", func(s *Spec) { s.Rig = "../x" }},
		{"rig is empty", "rig", func(s *Spec) { s.Rig = "" }},
		{"rig is dot-dot", "rig", func(s *Spec) { s.Rig = ".." }},
		{"rig holds a backslash", "rig", func(s *Spec) { s.Rig = `gas\town` }},
		{"rig reads as a flag", "rig", func(s *Spec) { s.Rig = "-x" }},
		{"sha is short", "sha", func(s *Spec) { s.SHA = "4f2a9c1e" }},
		{"sha is not hex", "sha", func(s *Spec) { s.SHA = strings.Repeat("z", 40) }},
		{"cpus is 0", "cpus", func(s *Spec) { s.CPUs = 0 }},
		{"cpus is 17", "cpus", func(s *Spec) { s.CPUs = 17 }},
		{"memory is 1", "memoryGB", func(s *Spec) { s.MemoryGB = 1 }},
		{"memory is 49", "memoryGB", func(s *Spec) { s.MemoryGB = 49 }},
		{"jobs is 0", "jobs", func(s *Spec) { s.Jobs = 0 }},
		{"jobs is 17", "jobs", func(s *Spec) { s.Jobs = 17 }},
		{"image is empty", "image", func(s *Spec) { s.Image = "" }},
		{"image reads as a flag", "image", func(s *Spec) { s.Image = "--rm" }},
		{"image holds a space", "image", func(s *Spec) { s.Image = "gt-polecat gt-74ix9" }},
		{"image holds a control character", "image", func(s *Spec) { s.Image = "gt-polecat:\n74ix9" }},
		{"tier is not one the sweep offers", "tiers", func(s *Spec) { s.Tiers = []string{"weekly"} }},
		{"tier appears twice", "tiers", func(s *Spec) { s.Tiers = []string{"shell", "shell"} }},
		{"out dir is relative", "outDir", func(s *Spec) { s.OutDir = "out" }},
		{"out dir holds a control character", "outDir", func(s *Spec) { s.OutDir = "/Users/ops/gt\n/out" }},
		{"repo dir holds a colon", "repoDir", func(s *Spec) { s.RepoDir = "/Users/ops/gt:stale/.repo.git" }},
		{"context is empty", "context", func(s *Spec) { s.Context = "" }},
		{"context holds a colon", "context", func(s *Spec) { s.Context = "colima:gt-sbx" }},
		{"dind has no run id", "runID", func(s *Spec) { s.RunID = "" }},
		{"run id escapes its segment", "runID", func(s *Spec) { s.RunID = "a/b" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			spec := goldenSpec()
			row.set(&spec)
			err := spec.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", spec)
			}
			var fieldErr *FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("Validate error %v is not a *FieldError", err)
			}
			if fieldErr.Field != row.field {
				t.Errorf("refused field %q, want %q (%v)", fieldErr.Field, row.field, err)
			}
			if !strings.Contains(err.Error(), "spec."+row.field) {
				t.Errorf("error %q does not name spec.%s", err, row.field)
			}
			if _, err := (Docker{}).Render(spec); err == nil {
				t.Errorf("Render accepted the spec Validate refuses: %+v", spec)
			}
		})
	}
}

// TestDockerRenderTeardownRemovesEveryObjectItMade keeps the plan's cleanup
// complete and flagged, so a caller that stops at the first failure still runs
// it and no container, volume or network outlives the run.
func TestDockerRenderTeardownRemovesEveryObjectItMade(t *testing.T) {
	t.Parallel()
	plan, err := Docker{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	made, removed := map[string]bool{}, map[string]bool{}
	seenTeardown := false
	for _, step := range plan.Steps {
		if !step.Teardown && seenTeardown {
			t.Fatalf("step %v runs after the teardown began", step.Argv)
		}
		seenTeardown = seenTeardown || step.Teardown
		object := step.Argv[len(step.Argv)-1]
		switch {
		case step.Teardown:
			removed[object] = true
		case step.Argv[3] == "network" || step.Argv[3] == "volume":
			made[step.Argv[len(step.Argv)-1]] = true
		default:
			if name, ok := valueAfter(step.Argv, "--name"); ok {
				made[name] = true
			}
		}
	}
	if len(removed) == 0 {
		t.Fatal("the plan renders no teardown, so a run leaves its network, volume and containers behind")
	}
	for object := range made {
		if !removed[object] {
			t.Errorf("%s is made and never removed", object)
		}
	}
	for object := range removed {
		if !made[object] {
			t.Errorf("%s is removed but no step makes it", object)
		}
	}
}

// TestDockerRenderGivesEachRunItsOwnDindVolume is the single-run invariant: a
// run's engine state is named for that run, so a rerun of the same commit never
// inherits it (gt-74ix9).
func TestDockerRenderGivesEachRunItsOwnDindVolume(t *testing.T) {
	t.Parallel()
	first, second := goldenSpec(), goldenSpec()
	second.RunID = "mvv8askp"
	firstPlan, err := Docker{}.Render(first)
	if err != nil {
		t.Fatalf("rendering the first run: %v", err)
	}
	secondPlan, err := Docker{}.Render(second)
	if err != nil {
		t.Fatalf("rendering the second run: %v", err)
	}
	created := func(plan Plan) string {
		step, ok := findStep(plan, "volume")
		if !ok {
			t.Fatal("the plan renders no volume step for a dind run")
		}
		return step.Argv[len(step.Argv)-1]
	}
	firstVolume, secondVolume := created(firstPlan), created(secondPlan)
	if firstVolume == secondVolume {
		t.Fatalf("two runs of %s render the same dind volume %q", first.SHA[:8], firstVolume)
	}
	if !strings.Contains(firstVolume, first.RunID) {
		t.Errorf("volume %q does not carry the run id %q", firstVolume, first.RunID)
	}
}

// findStep returns the first step whose verb is verb.
func findStep(plan Plan, verb string) (Step, bool) {
	for _, step := range plan.Steps {
		if len(step.Argv) > 3 && step.Argv[3] == verb {
			return step, true
		}
	}
	return Step{}, false
}

// stepNamed returns the step whose --name is name.
func stepNamed(plan Plan, name string) (Step, bool) {
	for _, step := range plan.Steps {
		if got, ok := valueAfter(step.Argv, "--name"); ok && got == name {
			return step, true
		}
	}
	return Step{}, false
}

// envValue returns the value a step sets for env.
func envValue(argv []string, env string) (string, bool) {
	for _, arg := range argv {
		if value, found := strings.CutPrefix(arg, env+"="); found {
			return value, true
		}
	}
	return "", false
}

// valueAfter returns the argument following flag.
func valueAfter(argv []string, flag string) (string, bool) {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}
