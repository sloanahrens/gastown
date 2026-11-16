package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadSpecDispatch loads body as a daemon patrol config from t.TempDir() and
// returns the spec_dispatch block the loader produced, so every row below drives
// the production load path rather than decoding the block on its own.
func loadSpecDispatch(t *testing.T, body string) (*SpecDispatchConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadDaemonPatrolConfig(path)
	if err != nil {
		return nil, err
	}
	if cfg.Patrols == nil {
		t.Fatal("patrols = nil, want the spec_dispatch block")
	}
	return cfg.Patrols.SpecDispatch, nil
}

// An absent lint_budget_max_lines and lint_budget_required leave the change
// budget off, so a town that sets nothing pays no budget lint.
func TestRunawayBeadLintBudgetOffByDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{name: "absent keys", body: `{"patrols":{"spec_dispatch":{"story_points":{"shadow":true}}}}`},
		{name: "no story_points block", body: `{"patrols":{"spec_dispatch":{"enabled":true}}}`},
		{name: "explicit zero and false", body: `{"patrols":{"spec_dispatch":{"story_points":{` +
			`"lint_budget_max_lines":0,"lint_budget_required":false}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sd, err := loadSpecDispatch(t, tc.body)
			if err != nil {
				t.Fatalf("LoadDaemonPatrolConfig: %v", err)
			}
			if got := sd.StoryPointsLintBudgetMaxLines(); got != 0 {
				t.Errorf("StoryPointsLintBudgetMaxLines() = %d, want 0 (off)", got)
			}
			if sd.StoryPointsLintBudgetRequired() {
				t.Error("StoryPointsLintBudgetRequired() = true, want false (off)")
			}
		})
	}
}

// A set lint_budget_max_lines and lint_budget_required load as written, so the
// lint can judge a bead's declared Change line against them.
func TestRunawayBeadLintBudgetReadsValues(t *testing.T) {
	t.Parallel()
	sd, err := loadSpecDispatch(t, `{"patrols":{"spec_dispatch":{"story_points":{`+
		`"lint_budget_max_lines":300,"lint_budget_required":true}}}}`)
	if err != nil {
		t.Fatalf("LoadDaemonPatrolConfig: %v", err)
	}
	if sd.StoryPoints == nil {
		t.Fatal("story_points = nil, want the block")
	}
	if got := sd.StoryPoints.LintBudgetMaxLines; got != 300 {
		t.Errorf("LintBudgetMaxLines = %d, want 300", got)
	}
	if !sd.StoryPoints.LintBudgetRequired {
		t.Error("LintBudgetRequired = false, want true")
	}
	if got := sd.StoryPointsLintBudgetMaxLines(); got != 300 {
		t.Errorf("StoryPointsLintBudgetMaxLines() = %d, want 300", got)
	}
	if !sd.StoryPointsLintBudgetRequired() {
		t.Error("StoryPointsLintBudgetRequired() = false, want true")
	}
}

// A negative lint_budget_max_lines is refused at decode, naming the key: a
// ceiling below zero is a typo, and read as written it would refuse every bead.
func TestRunawayBeadLintBudgetRefusesNegative(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"-1", "-300"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			_, err := loadSpecDispatch(t, `{"patrols":{"spec_dispatch":{"story_points":{`+
				`"lint_budget_max_lines":`+value+`}}}}`)
			if err == nil {
				t.Fatalf("LoadDaemonPatrolConfig accepted lint_budget_max_lines %s", value)
			}
			if !strings.Contains(err.Error(), "spec_dispatch.story_points.lint_budget_max_lines") {
				t.Errorf("error = %v, want it to name spec_dispatch.story_points.lint_budget_max_lines", err)
			}
		})
	}
}

// Both accessors answer off on a nil spec_dispatch block and on a block that
// leaves story_points out, which is what an absent setting leaves callers
// holding.
func TestRunawayBeadLintBudgetNilIsOff(t *testing.T) {
	t.Parallel()
	var nilBlock *SpecDispatchConfig
	if got := nilBlock.StoryPointsLintBudgetMaxLines(); got != 0 {
		t.Errorf("StoryPointsLintBudgetMaxLines() on a nil block = %d, want 0", got)
	}
	if nilBlock.StoryPointsLintBudgetRequired() {
		t.Error("StoryPointsLintBudgetRequired() on a nil block = true, want false")
	}

	var noStoryPoints SpecDispatchConfig
	if got := noStoryPoints.StoryPointsLintBudgetMaxLines(); got != 0 {
		t.Errorf("StoryPointsLintBudgetMaxLines() with no story_points = %d, want 0", got)
	}
	if noStoryPoints.StoryPointsLintBudgetRequired() {
		t.Error("StoryPointsLintBudgetRequired() with no story_points = true, want false")
	}
}

// A block built in code around a negative lint_budget_max_lines still reads as
// off: the accessor is the one reader, and it clamps to zero.
func TestRunawayBeadLintBudgetNegativeInMemoryIsOff(t *testing.T) {
	t.Parallel()
	sd := &SpecDispatchConfig{StoryPoints: &StoryPointsConfig{LintBudgetMaxLines: -5}}
	if got := sd.StoryPointsLintBudgetMaxLines(); got != 0 {
		t.Errorf("StoryPointsLintBudgetMaxLines() = %d, want 0 for a negative value", got)
	}
}

// The block decodes its own JSON, so it repeats the kernel's unknown-key
// refusal: a key that is not one of its settings is a typo, never an ignored
// setting.
func TestRunawayBeadStoryPointsUnknownKeyRefused(t *testing.T) {
	t.Parallel()
	_, err := loadSpecDispatch(t, `{"patrols":{"spec_dispatch":{"story_points":{`+
		`"lint_budget_max_line":300}}}}`)
	if err == nil {
		t.Fatal("LoadDaemonPatrolConfig accepted lint_budget_max_line")
	}
	if !strings.Contains(err.Error(), "lint_budget_max_line") {
		t.Errorf("error = %v, want it to name lint_budget_max_line", err)
	}
}

// The block's other settings still load through its custom decoder, so adding
// the change budget keys takes nothing away from them.
func TestRunawayBeadStoryPointsBlockStillDecodes(t *testing.T) {
	t.Parallel()
	sd, err := loadSpecDispatch(t, `{"patrols":{"spec_dispatch":{"story_points":{`+
		`"embedder_url":"http://host.docker.internal:11434",`+
		`"embedder_model":"qwen3-embedding:4b-q8_0",`+
		`"shadow":true,"split_over_minutes":15,"lint_over_minutes":11,`+
		`"lint_budget_max_lines":300,"lint_budget_required":true}}}}`)
	if err != nil {
		t.Fatalf("LoadDaemonPatrolConfig: %v", err)
	}
	if sd.StoryPoints == nil {
		t.Fatal("story_points = nil, want the block")
	}
	want := StoryPointsConfig{
		EmbedderURL:        "http://host.docker.internal:11434",
		EmbedderModel:      "qwen3-embedding:4b-q8_0",
		Shadow:             true,
		SplitOverMinutes:   15,
		LintOverMinutes:    11,
		LintBudgetMaxLines: 300,
		LintBudgetRequired: true,
	}
	if *sd.StoryPoints != want {
		t.Errorf("story_points = %+v, want %+v", *sd.StoryPoints, want)
	}
}
