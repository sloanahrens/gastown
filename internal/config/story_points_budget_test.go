package config

import (
	"os"
	"path/filepath"
	"strconv"
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

// loadSpecDispatchWarn loads body through the production load path with the
// warnings it emits captured, so a row can assert both the decoded block and
// what the operator is told.
func loadSpecDispatchWarn(t *testing.T, body string) (*SpecDispatchConfig, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var warn strings.Builder
	cfg, err := loadDaemonPatrolConfig(path, &warn)
	if err != nil {
		t.Fatalf("loadDaemonPatrolConfig: %v", err)
	}
	if cfg.Patrols == nil || cfg.Patrols.SpecDispatch == nil {
		t.Fatal("spec_dispatch = nil, want the block")
	}
	return cfg.Patrols.SpecDispatch, warn.String()
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

// A negative lint_budget_max_lines loads the way its sibling duration fields
// do: the load warns naming the key and keeps the operator's value as written,
// and StoryPointsLintBudgetMaxLines reads it as off, so a typo cannot fail the
// whole load or refuse every bead the lint sees.
func TestRunawayBeadLintBudgetWarnsNegative(t *testing.T) {
	t.Parallel()
	for _, value := range []int{-1, -300} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			t.Parallel()
			sd, warn := loadSpecDispatchWarn(t, `{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"story_points":{`+
				`"lint_budget_max_lines":`+strconv.Itoa(value)+`,"lint_budget_required":true}}}}`)
			if sd.StoryPoints.LintBudgetMaxLines != value {
				t.Errorf("LintBudgetMaxLines = %d, want the operator's %d kept", sd.StoryPoints.LintBudgetMaxLines, value)
			}
			if !sd.StoryPoints.LintBudgetRequired {
				t.Error("LintBudgetRequired = false, want the block's other settings kept")
			}
			if got := sd.StoryPointsLintBudgetMaxLines(); got != 0 {
				t.Errorf("StoryPointsLintBudgetMaxLines() = %d, want 0 (off)", got)
			}
			if !strings.Contains(warn, "daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines") {
				t.Errorf("warning = %q, want it to name daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines", warn)
			}
			if !strings.Contains(warn, strconv.Itoa(value)) {
				t.Errorf("warning = %q, want it to name the value %d", warn, value)
			}
		})
	}
}

// A negative lint_budget_max_lines is refused on save, naming the key, so a
// writer cannot persist a ceiling the load only warns about.
func TestSaveDaemonPatrolConfigRefusesNegativeLintBudget(t *testing.T) {
	t.Parallel()
	for _, value := range []int{-1, -300} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.json")
			cfg := &DaemonPatrolConfig{
				Type:    "daemon-patrol-config",
				Version: 1,
				Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{
					StoryPoints: &StoryPointsConfig{LintBudgetMaxLines: value},
				}},
			}
			err := SaveDaemonPatrolConfig(path, cfg)
			if err == nil {
				t.Fatalf("SaveDaemonPatrolConfig accepted lint_budget_max_lines %d", value)
			}
			if !strings.Contains(err.Error(), "daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines") {
				t.Errorf("error = %v, want it to name daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("a refused save left a file behind: %v", err)
			}
		})
	}
}

// A usable lint_budget_max_lines loads without a word: only a value below zero
// is a typo the load reports.
func TestRunawayBeadLintBudgetGoodValueWarnsNothing(t *testing.T) {
	t.Parallel()
	for _, value := range []int{0, 300} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			t.Parallel()
			sd, warn := loadSpecDispatchWarn(t, `{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"story_points":{`+
				`"lint_budget_max_lines":`+strconv.Itoa(value)+`}}}}`)
			if warn != "" {
				t.Errorf("a usable lint_budget_max_lines %d warned: %q", value, warn)
			}
			if got := sd.StoryPointsLintBudgetMaxLines(); got != value {
				t.Errorf("StoryPointsLintBudgetMaxLines() = %d, want %d", got, value)
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

// A block built in code around a negative lint_budget_max_lines reads as off:
// the accessor is the one reader and clamps to zero, so the loader's warning
// and the save's refusal are the signals such a value gets (gt-p2zuge).
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
