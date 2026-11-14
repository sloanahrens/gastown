package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// TestSpecLintSizeGate: gt spec lint refuses a bead whose own sp-minutes label
// is at or over the town's lint_over_minutes, names the label, the setting and
// the split rule, and with the gate off it answers exactly as it did before
// (gt-mtzx6s).
func TestSpecLintSizeGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		labels   []string
		lintOver float64
		wantCode int
		// wantOut are phrases the verdict line must carry.
		wantOut []string
		// wantWarn is a phrase the stderr WARN line must carry; empty is none.
		wantWarn string
	}{
		{
			name:     "over the setting",
			labels:   []string{"sp:13", "sp-rung:all", "sp-minutes:12.3"},
			lintOver: 11,
			wantCode: specLintExitRefused,
			wantOut: []string{
				"size:", "sp-minutes:12.3", "lint_over_minutes 11",
				"split the bead into beads of about twenty minutes each, one deliverable each",
				"remove the label with the reason",
			},
		},
		{
			name:     "under the setting",
			labels:   []string{"sp-minutes:8.8"},
			lintOver: 11,
		},
		{
			name:     "the gate is off",
			labels:   []string{"sp-minutes:12.3"},
			lintOver: 0,
		},
		{
			name:     "a minutes label that does not parse",
			labels:   []string{"sp-minutes:soon"},
			lintOver: 11,
			wantWarn: `"sp-minutes:soon" is unreadable as sp-minutes`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := cleanSpec("gt-size", 2, "2026-01-01")
			spec.Labels = tc.labels
			var out, errOut bytes.Buffer
			err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), specLintOpts{LintOverMinutes: tc.lintOver})
			if code, _ := IsSilentExit(err); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (err %v, out %q)", code, tc.wantCode, err, out.String())
			}
			if len(tc.wantOut) == 0 {
				if !strings.Contains(out.String(), "spec lint ok") {
					t.Errorf("verdict %q, want the clean line", out.String())
				}
			} else {
				for _, want := range tc.wantOut {
					if !strings.Contains(out.String(), want) {
						t.Errorf("verdict %q does not name %q", out.String(), want)
					}
				}
			}
			if tc.wantWarn == "" {
				if strings.Contains(errOut.String(), "WARN:") {
					t.Errorf("stderr %q warns with the gate reading a readable bead", errOut.String())
				}
				return
			}
			if !strings.Contains(errOut.String(), "WARN:") || !strings.Contains(errOut.String(), tc.wantWarn) {
				t.Errorf("stderr %q does not warn %q", errOut.String(), tc.wantWarn)
			}
			if !strings.Contains(errOut.String(), "the size gate did not run") {
				t.Errorf("stderr %q does not say the gate did not run", errOut.String())
			}
		})
	}
}

// TestStoryPointsLintWireReadsTheDaemonConfig: gt spec lint and the dispatcher
// tick both read the size gate from patrols.spec_dispatch.story_points through
// the town's own config read, and an unset block leaves it off (gt-mtzx6s).
func TestStoryPointsLintWireReadsTheDaemonConfig(t *testing.T) {
	t.Parallel()
	const lintOn = `{"type":"daemon-patrol-config","version":1,` +
		`"patrols":{"spec_dispatch":{"enabled":true,"interval":"60s","story_points":{"lint_over_minutes":11}}}}`
	for _, tc := range []struct {
		name   string
		daemon string
		want   float64
	}{
		{"set", lintOn, 11},
		{"unset", specTickDaemon, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := poolTestTown(t, specTickSettings, tc.daemon)
			_, sd, err := specTickTown(root)
			if err != nil {
				t.Fatalf("specTickTown: %v", err)
			}
			if got := specLintOptsFrom(sd).LintOverMinutes; got != tc.want {
				t.Errorf("gt spec lint's LintOverMinutes = %v, want %v", got, tc.want)
			}
			var env specDispatchEnv
			specWireStoryPoints(&env, sd, root)
			if env.StoryPointLintOver != tc.want {
				t.Errorf("the tick's StoryPointLintOver = %v, want %v", env.StoryPointLintOver, tc.want)
			}
		})
	}
}

// TestStoryPointsLintTickHoldsAnOverSizeCandidate: the dispatcher's shape gate
// holds a candidate the size gate refuses and slings the ones under it, one
// whose rung is none and one the overseer waived with size-ok (gt-mtzx6s).
func TestStoryPointsLintTickHoldsAnOverSizeCandidate(t *testing.T) {
	t.Parallel()
	over := cleanSpec("gt-over", 2, "2026-09-29T10:00:00Z")
	over.Labels = []string{"sp:13", "sp-rung:all", "sp-minutes:12.3"}
	under := cleanSpec("gt-under", 2, "2026-09-29T10:01:00Z")
	under.Labels = []string{"sp-minutes:8.8"}
	noRung := cleanSpec("gt-norung", 2, "2026-09-29T10:02:00Z")
	noRung.Labels = []string{"sp-minutes:40", "sp-rung:none"}
	waived := cleanSpec("gt-waived", 2, "2026-09-29T10:03:00Z")
	waived.Labels = []string{"sp-minutes:40", "size-ok"}

	f := newFakeSpecTown(over, under, noRung, waived)
	env := f.env()
	env.ShapeGate = specdispatch.ShapeGateWarn
	env.StoryPointLintOver = 11
	env.PerTick = 4
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-under gt-norung gt-waived" {
		t.Fatalf("slung %q, want the three beads the size gate passes", got)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "gt-over: unshaped: size") {
		t.Fatalf("skipped = %+v, want gt-over held on its size", r.Skipped)
	}
	if n := f.notes["gt-over"]; len(n) != 1 || !strings.Contains(n[0], "sp-minutes:12.3") {
		t.Errorf("notes = %v, want one SHAPE note naming the label", n)
	}
}

// TestStoryPointsLintTickLogsAnUnreadableLabel: a candidate whose sp-minutes
// labels disagree is slung anyway — an unreadable size is never a refusal — and
// the tick says once why the size gate did not run, naming the bead and its
// labels. A bead whose label reads logs nothing (gt-mtzx6s).
func TestStoryPointsLintTickLogsAnUnreadableLabel(t *testing.T) {
	t.Parallel()
	unreadable := cleanSpec("gt-unreadable", 2, "2026-09-29T10:00:00Z")
	unreadable.Labels = []string{"sp-minutes:12", "sp-minutes:14"}
	readable := cleanSpec("gt-readable", 2, "2026-09-29T10:01:00Z")
	readable.Labels = []string{"sp-minutes:8.8"}

	f := newFakeSpecTown(unreadable, readable)
	env := f.env()
	env.ShapeGate = specdispatch.ShapeGateWarn
	env.StoryPointLintOver = 11
	env.PerTick = 2
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-unreadable gt-readable" {
		t.Fatalf("slung %q, want both beads: an unreadable label is not a refusal", got)
	}
	var gateNotes []string
	for _, n := range r.Notices {
		if strings.Contains(n, "size gate did not run") {
			gateNotes = append(gateNotes, n)
		}
	}
	if len(gateNotes) != 1 {
		t.Fatalf("notices %q, want one line for the bead the gate could not read", r.Notices)
	}
	for _, want := range []string{"gt-unreadable", "sp-minutes:12", "sp-minutes:14", "disagree"} {
		if !strings.Contains(gateNotes[0], want) {
			t.Errorf("notice %q does not name %q", gateNotes[0], want)
		}
	}
}
