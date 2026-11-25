package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/land"
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

// TestSpecFollowupBudgetReadsTheRigMergeQueue: the lint's follow-up budget is
// the bead rig's merge_queue settings, read as gt done reads them, and land's
// defaults for a rig that sets none (gt-hixqtv).
func TestSpecFollowupBudgetReadsTheRigMergeQueue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rigDir := filepath.Join(root, "gastown")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating the rig dir: %v", err)
	}
	cfg := `{"name":"gastown","merge_queue":{"followup_max_added_lines":150,"followup_max_files":5}}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing the rig config: %v", err)
	}
	if added, files := specFollowupBudget(root, "gastown"); added != 150 || files != 5 {
		t.Errorf("the configured budget = %d added lines, %d files, want 150 and 5", added, files)
	}
	for _, rig := range []string{"nosuchrig", ""} {
		added, files := specFollowupBudget(root, rig)
		if added != land.DefaultFollowupMaxAddedLines || files != land.DefaultFollowupMaxFiles {
			t.Errorf("the budget of rig %q = %d added lines, %d files, want the defaults %d and %d",
				rig, added, files, land.DefaultFollowupMaxAddedLines, land.DefaultFollowupMaxFiles)
		}
	}
}

// TestLintOptionsCarryTheFollowupBudget: the lint options carry the resolved
// follow-up budget, so a rig that configures one has it read by the command's
// lint and the tick's lint alike (gt-hixqtv).
func TestLintOptionsCarryTheFollowupBudget(t *testing.T) {
	t.Parallel()
	budget := func(rig string) (int, int) { return 150, 5 }
	if got := (specLintOpts{FollowupBudget: budget}).lintOptionsFor("gastown"); got.FollowupMaxAddedLines != 150 || got.FollowupMaxFiles != 5 {
		t.Errorf("gt spec lint's options = %d added lines, %d files, want 150 and 5", got.FollowupMaxAddedLines, got.FollowupMaxFiles)
	}
	if got := (specDispatchEnv{FollowupBudget: budget}).lintOptionsFor("gastown"); got.FollowupMaxAddedLines != 150 || got.FollowupMaxFiles != 5 {
		t.Errorf("the tick's options = %d added lines, %d files, want 150 and 5", got.FollowupMaxAddedLines, got.FollowupMaxFiles)
	}
}
