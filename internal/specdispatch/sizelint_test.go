package specdispatch

import (
	"slices"
	"strings"
	"testing"
)

// With the setting unset the story-point size gate reads no label: a bead with
// any sp-minutes label — one the setting would refuse, one that does not parse,
// or two that disagree — lints exactly as a bead without labels does, verdict
// line and warnings alike (gt-mtzx6s).
func TestStoryPointsLintOffChangesNothing(t *testing.T) {
	t.Parallel()
	plain := Lint(goodSpec(), defaultTemplate())
	if !plain.Clean() {
		t.Fatalf("plain bead refused: %s", plain.Line("gt-good"))
	}
	cases := []struct {
		name   string
		labels []string
		opts   LintOptions
	}{
		{"over the setting, unset", []string{"sp-minutes:12.3", "sp-rung:all", "sp:13"}, LintOptions{}},
		{"over the setting, zero", []string{"sp-minutes:12.3", "sp-rung:all", "sp:13"}, LintOptions{LintOverMinutes: 0}},
		{"over the setting, negative", []string{"sp-minutes:12.3", "sp-rung:all", "sp:13"}, LintOptions{LintOverMinutes: -11}},
		{"a minutes label that does not parse", []string{"sp-minutes:soon"}, LintOptions{}},
		{"two minutes labels that disagree", []string{"sp-minutes:12", "sp-minutes:14"}, LintOptions{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec("one worker, one MR", tc.labels...)
			v := LintWith(s, defaultTemplate(), tc.opts)
			if !v.Clean() {
				t.Fatalf("bead %v refused with the gate off: %s", tc.labels, v.Line(s.ID))
			}
			if got, want := v.Line(s.ID), plain.Line("gt-good"); got != want {
				t.Errorf("Line = %q, want %q", got, want)
			}
			if len(v.Warnings) != 0 {
				t.Errorf("warnings %q with the gate off, want none", v.Warnings)
			}
		})
	}
}

// With lint_over_minutes set, a bead whose own sp-minutes label is at or over
// it is refused with the label, the setting and the split rule named. An
// estimate under the setting passes, and so does a bead with no label, one
// whose rung is none, and one the overseer marked size-ok. A label that cannot
// be read passes the gate but says so: an unjudged bead is not a small one
// (gt-mtzx6s).
func TestStoryPointsLintGate(t *testing.T) {
	t.Parallel()
	const over = 11
	cases := []struct {
		name   string
		labels []string
		// refuse is the refusal the bead earns; empty means it passes.
		refuse bool
		// wantReason lists the phrases a refusal must carry.
		wantReason []string
		// wantWarn, when set, is a phrase the one warning must carry.
		wantWarn string
	}{
		{
			name:   "over the setting",
			labels: []string{"sp:13", "sp-rung:all", "sp-minutes:12.3"},
			refuse: true,
			wantReason: []string{
				"sp-minutes:12.3", "lint_over_minutes 11", "at or over",
				"split the bead into beads of about twenty minutes each, one deliverable each",
				"remove the label with the reason",
			},
		},
		{
			name:       "at the setting",
			labels:     []string{"sp-minutes:11"},
			refuse:     true,
			wantReason: []string{"sp-minutes:11", "lint_over_minutes 11", "at or over"},
		},
		{
			name:       "an upper-case prefix",
			labels:     []string{"SP-MINUTES:12.3"},
			refuse:     true,
			wantReason: []string{"SP-MINUTES:12.3", "lint_over_minutes 11"},
		},
		{
			name:   "under the setting",
			labels: []string{"sp-minutes:8.8"},
		},
		{
			name: "no label",
		},
		{
			name:   "the rung is none",
			labels: []string{"sp-minutes:40", "sp-rung:none"},
		},
		{
			name:   "the overseer marked size-ok",
			labels: []string{"sp-minutes:40", "size-ok"},
		},
		{
			name:     "a minutes label that does not parse",
			labels:   []string{"sp-minutes:soon"},
			wantWarn: `"sp-minutes:soon" is unreadable as sp-minutes`,
		},
		{
			name:     "two minutes labels that disagree",
			labels:   []string{"sp-minutes:12", "sp-minutes:14"},
			wantWarn: `"sp-minutes:12" and "sp-minutes:14" disagree`,
		},
		{
			name:     "an upper-case prefix that does not parse",
			labels:   []string{"SP-MINUTES:soon"},
			wantWarn: `"SP-MINUTES:soon" is unreadable as sp-minutes`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := sizedSpec("one worker, one MR", tc.labels...)
			v := LintWith(s, defaultTemplate(), LintOptions{LintOverMinutes: over})
			if !tc.refuse {
				if !v.Clean() {
					t.Fatalf("bead %v refused: %s", tc.labels, v.Line(s.ID))
				}
				if tc.wantWarn == "" {
					if len(v.Warnings) != 0 {
						t.Errorf("warnings %q, want none", v.Warnings)
					}
					return
				}
				if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], tc.wantWarn) {
					t.Fatalf("warnings %q, want one naming %q", v.Warnings, tc.wantWarn)
				}
				if !strings.Contains(v.Warnings[0], "the size gate did not run") {
					t.Errorf("warning %q does not say the gate did not run", v.Warnings[0])
				}
				return
			}
			if v.Route != RouteRefuse {
				t.Fatalf("bead %v got route %q, want %q: %s", tc.labels, v.Route, RouteRefuse, v.Line(s.ID))
			}
			if v.Field != "size" {
				t.Errorf("Field = %q, want size", v.Field)
			}
			for _, phrase := range tc.wantReason {
				if !strings.Contains(v.Reason, phrase) {
					t.Errorf("Reason %q does not name %q", v.Reason, phrase)
				}
			}
			if len(v.Warnings) != 0 {
				t.Errorf("refused bead's warnings = %q, want none", v.Warnings)
			}
		})
	}
}

// The gate reads the setting at its own decimal precision, so a fractional
// lint_over_minutes is named as it was set rather than rounded to a whole
// minute the operator did not write (gt-mtzx6s).
func TestStoryPointsLintGateNamesAFractionalSetting(t *testing.T) {
	t.Parallel()
	s := sizedSpec("one worker, one MR", "sp-minutes:11.5")
	v := LintWith(s, defaultTemplate(), LintOptions{LintOverMinutes: 11.5})
	if v.Route != RouteRefuse || !strings.Contains(v.Reason, "lint_over_minutes 11.5") {
		t.Fatalf("verdict %q does not name the fractional setting: %s", v.Reason, v.Line(s.ID))
	}
}

// The size gate is a refusal of a work bead, not of a display: LintShape is
// asked where no dispatch is decided and takes no settings, so the over-size
// bead keeps the clean shape it shows on a queue badge (gt-mtzx6s).
func TestStoryPointsLintGateIsNotAskedOfLintShape(t *testing.T) {
	t.Parallel()
	s := sizedSpec("one worker, one MR", "sp-minutes:12.3")
	v := LintShape(s, defaultTemplate())
	if !v.Clean() {
		t.Fatalf("LintShape refused: %s", v.Line(s.ID))
	}
	if len(v.Warnings) != 0 {
		t.Errorf("LintShape warnings = %q, want none", v.Warnings)
	}
}

// A bead whose shape already fails still lists the size refusal, so one gt
// spec lint --json prints everything the filer has to fix at once
// (gt-mtzx6s).
func TestStoryPointsLintGateJoinsTheShapeRefusals(t *testing.T) {
	t.Parallel()
	s := goodSpec()
	s.Description = strings.Replace(s.Description, "## Gate\nmake gate\n", "", 1)
	s.Labels = []string{"sp-minutes:12.3"}
	v := LintWith(s, defaultTemplate(), LintOptions{LintOverMinutes: 11})
	fields := make([]string, 0, len(v.Refusals))
	for _, r := range v.Refusals {
		fields = append(fields, r.Field)
	}
	for _, want := range []string{"## Gate", "size"} {
		if !slices.Contains(fields, want) {
			t.Errorf("refusals %v do not name %q", fields, want)
		}
	}
}
