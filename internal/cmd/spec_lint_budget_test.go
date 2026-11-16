package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

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
// same beads clean (gt-muzkwi).
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
				{"450", "300"},
				{"no change budget"},
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
			opts := specLintOptsFrom(sd)
			if got := opts.lintOptions(); got != tc.want {
				t.Errorf("gt spec lint's options = %+v, want %+v", got, tc.want)
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

// TestRunawayBeadBudgetEverySitePassesTheSameValues: the command's options and
// the dispatcher tick's options are built from the same config and carry the
// same change budget settings, so a candidate the seat's lint would refuse is
// refused at the tick too (gt-muzkwi).
func TestRunawayBeadBudgetEverySitePassesTheSameValues(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, specTickSettings, specLintBudgetDaemon)
	_, sd, err := specTickTown(root)
	if err != nil {
		t.Fatalf("specTickTown: %v", err)
	}
	want := specdispatch.LintOptions{BudgetMaxLines: 300, BudgetRequired: true}
	fromConfig := specLintOptsFrom(sd).lintOptions()
	if fromConfig != want {
		t.Errorf("gt spec lint's options = %+v, want %+v", fromConfig, want)
	}
	var env specDispatchEnv
	specWireStoryPoints(&env, sd, root)
	tick := env.lintOptions()
	if tick != want {
		t.Errorf("the tick's options = %+v, want %+v", tick, want)
	}
	if fromConfig != tick {
		t.Errorf("the two sites disagree: gt spec lint %+v, the tick %+v", fromConfig, tick)
	}
}
