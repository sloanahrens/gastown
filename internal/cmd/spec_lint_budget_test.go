package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specLintBudgetDaemon is a patrol config whose spec_dispatch block turns both
// change budget settings on: a declared budget over 300 added lines is refused,
// and a bead that declares none is refused.
const specLintBudgetDaemon = `{"type":"daemon-patrol-config","version":1,` +
	`"patrols":{"spec_dispatch":{"enabled":true,"interval":"60s",` +
	`"story_points":{"lint_budget_max_lines":300,"lint_budget_required":true}}}}`

// specLintBudgetSpec is a clean work bead whose Size section declares line as
// its change budget, or no budget line at all when line is empty.
func specLintBudgetSpec(id, line string) specdispatch.Spec {
	body := "One worker, one landing."
	if line != "" {
		body += "\n\n" + line
	}
	body += "\n\nSplits into its own MR."
	s := cleanSpec(id, 2, "2026-01-01")
	s.Description = strings.Replace(s.Description, "one worker, one MR", body, 1)
	return s
}

// TestRunawayBeadBudgetLintReadsTheDaemonConfig: gt spec lint reads the change
// budget gate from patrols.spec_dispatch.story_points, so a bead over the
// declared maximum and a bead that declares none are both refused through the
// town's own config read, and an unset block leaves both settings off and the
// same beads clean. The command's options and the dispatcher tick's options are
// built from the same config and carry the same settings, so a candidate the
// seat's lint would refuse is refused at the tick too (gt-muzkwi).
func TestRunawayBeadBudgetLintReadsTheDaemonConfig(t *testing.T) {
	t.Parallel()
	over := specLintBudgetSpec("gt-over", "Change: at most 450 added lines in 3 files.")
	none := specLintBudgetSpec("gt-none", "")
	for _, tc := range []struct {
		name   string
		daemon string
		want   specdispatch.LintOptions
		// refusals are, per bead in order over then none, the phrases the
		// verdict must carry; nil is a clean bead.
		refusals [][]string
	}{
		{
			name:   "set",
			daemon: specLintBudgetDaemon,
			want:   specdispatch.LintOptions{BudgetMaxLines: 300, BudgetRequired: true},
			refusals: [][]string{
				{
					"declares at most 450 added lines",
					"change budget maximum of 300",
					"split the bead by package",
				},
				{"states no change budget"},
			},
		},
		{
			name:     "unset",
			daemon:   specTickDaemon,
			want:     specdispatch.LintOptions{},
			refusals: [][]string{nil, nil},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolTestTown(t, specTickSettings, tc.daemon)
			_, sd, err := specTickTown(root)
			if err != nil {
				t.Fatalf("specTickTown: %v", err)
			}
			opts, err := specLintOptsFrom(sd)
			if err != nil {
				t.Fatalf("specLintOptsFrom: %v", err)
			}
			fromConfig := opts.lintOptions()
			if fromConfig != tc.want {
				t.Errorf("gt spec lint's options = %+v, want %+v", fromConfig, tc.want)
			}
			var env specDispatchEnv
			if err := specWireStoryPoints(&env, sd, root); err != nil {
				t.Fatalf("specWireStoryPoints: %v", err)
			}
			if tick := env.lintOptions(); tick != tc.want {
				t.Errorf("the tick's options = %+v, want %+v", tick, tc.want)
			}
			for i, spec := range []specdispatch.Spec{over, none} {
				wantOut := tc.refusals[i]
				var out, errOut bytes.Buffer
				err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts)
				if code, _ := IsSilentExit(err); wantOut == nil && code != 0 {
					t.Fatalf("%s with the settings off: exit %d (err %v, out %q)", spec.ID, code, err, out.String())
				} else if wantOut != nil && code != specLintExitRefused {
					t.Fatalf("%s over the settings: exit %d, want refused (err %v, out %q)", spec.ID, code, err, out.String())
				}
				for _, want := range wantOut {
					if !strings.Contains(out.String(), want) {
						t.Errorf("%s's verdict %q does not name %q", spec.ID, out.String(), want)
					}
				}
			}
		})
	}
}

// A negative lint_budget_max_lines stops gt spec lint at its settings read,
// naming the key, rather than running with the budget gate silently off
// (gt-p2zuge).
func TestRunawayBeadBudgetNegativeStopsSpecLint(t *testing.T) {
	t.Parallel()
	sd := &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{LintBudgetMaxLines: -5}}
	_, err := specLintOptsFrom(sd)
	if err == nil {
		t.Fatal("specLintOptsFrom accepted a negative lint_budget_max_lines")
	}
	if !strings.Contains(err.Error(), "daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines") {
		t.Errorf("error = %v, want it to name daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines", err)
	}
}

// A negative lint_budget_max_lines stops the dispatch tick at its settings
// read, naming the key, rather than running with the budget gate silently off
// (gt-p2zuge).
func TestRunawayBeadBudgetNegativeStopsDispatchTick(t *testing.T) {
	t.Parallel()
	sd := &config.SpecDispatchConfig{StoryPoints: &config.StoryPointsConfig{LintBudgetMaxLines: -5}}
	var env specDispatchEnv
	err := specWireStoryPoints(&env, sd, "")
	if err == nil {
		t.Fatal("specWireStoryPoints accepted a negative lint_budget_max_lines")
	}
	if !strings.Contains(err.Error(), "daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines") {
		t.Errorf("error = %v, want it to name daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines", err)
	}
	if env.StoryPointLintBudgetMaxLines != 0 {
		t.Errorf("the tick's budget = %d, want 0 left unset when the block is refused", env.StoryPointLintBudgetMaxLines)
	}
}

// specNegativeBudgetDaemon is a patrol config whose change budget is below
// zero: the load warns and keeps it, so the readers refuse it.
const specNegativeBudgetDaemon = `{"type":"daemon-patrol-config","version":1,` +
	`"patrols":{"spec_dispatch":{"enabled":true,"interval":"60s",` +
	`"story_points":{"lint_budget_max_lines":-5}}}}`

// gt spec lint reads the town's config before the bead, so a town whose change
// budget is below zero answers with the refusal naming the key and never
// reaches the bead (gt-p2zuge).
func TestRunawayBeadBudgetNegativeStopsTheSpecLintCommand(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, specTickSettings, specNegativeBudgetDaemon)
	err := runSpecLintOnWith(&cobra.Command{}, []string{"gt-any"}, root, false, slingStores{}, false, false)
	if err == nil {
		t.Fatal("gt spec lint ran on a town whose lint_budget_max_lines is -5")
	}
	if !strings.Contains(err.Error(), "daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines") {
		t.Errorf("error = %v, want it to name daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines", err)
	}
}

// The dispatch tick stops on the same refusal before it reads a candidate, so
// a town whose change budget is below zero dispatches nothing until the file
// is fixed and the daemon logs why (gt-p2zuge).
func TestRunawayBeadBudgetNegativeStopsTheDispatchTickCommand(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, specTickSettings, specNegativeBudgetDaemon)
	// The operator's hold stops the tick before it reads the roster; the
	// settings read comes before the hold, so the refusal is still the answer.
	if err := os.WriteFile(dispatch.HoldFilePath(root), nil, 0o644); err != nil {
		t.Fatalf("planting the operator hold: %v", err)
	}
	_, err := runSpecTickForTest(t, root, "", false)
	if err == nil {
		t.Fatal("the dispatch tick ran on a town whose lint_budget_max_lines is -5")
	}
	if !strings.Contains(err.Error(), "daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines") {
		t.Errorf("error = %v, want it to name daemon.patrols.spec_dispatch.story_points.lint_budget_max_lines", err)
	}
}
