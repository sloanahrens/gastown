package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// TestSpecLintSizeAdviceLine: a bead the estimate flagged sp-large gets one
// "advice:" line naming the minutes its estimate was made from and still exits
// 0, while a bead without the flag prints nothing extra (gt-9poneu).
func TestSpecLintSizeAdviceLine(t *testing.T) {
	t.Parallel()
	const want = "advice: predicted to land over 300 added lines (estimate 12.3 min); consider splitting by file group"
	for _, tc := range []struct {
		name   string
		labels []string
		want   string
	}{
		{name: "flagged", labels: []string{"sp:13", "sp-rung:all", "sp-minutes:12.3", "sp-large"}, want: want},
		{name: "not flagged", labels: []string{"sp:13", "sp-rung:all", "sp-minutes:12.3"}},
		{name: "flagged with no minutes label", labels: []string{"sp-large"}, want: "advice: predicted to land over 300 added lines (estimate unreadable); consider splitting by file group"},
		{name: "flagged with an unreadable minutes label", labels: []string{"sp-large", "sp-minutes:12.3", "sp-minutes:soon"}, want: "advice: predicted to land over 300 added lines (estimate unreadable); consider splitting by file group"},
		{name: "flagged with disagreeing minutes labels", labels: []string{"sp-large", "sp-minutes:12.3", "sp-minutes:14"}, want: "advice: predicted to land over 300 added lines (estimate unreadable); consider splitting by file group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := cleanSpec("gt-large", 2, "2026-01-01")
			spec.Labels = tc.labels
			var out, errOut bytes.Buffer
			if err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), specLintOpts{}); err != nil {
				t.Fatalf("clean bead returned %v, want exit 0 (out %q, err %q)", err, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), "spec lint ok") {
				t.Errorf("verdict %q, want the clean line", out.String())
			}
			if tc.want == "" {
				if strings.Contains(errOut.String(), "predicted to land over") {
					t.Errorf("a bead without sp-large got the size advice:\n%s", errOut.String())
				}
				return
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("advice is missing %q:\n%s", tc.want, errOut.String())
			}
		})
	}
}

// TestSpecLintSizeAdviceReadsTheEstimateRun: with --estimate the advice follows
// the labels that run wrote, not the ones the bead wore before it — a run that
// predicts a small landing removes the stale sp-large and prints no advice, and
// one that predicts a large landing adds the flag and names the new minutes.
func TestSpecLintSizeAdviceReadsTheEstimateRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		seed   []string
		spec   []string
		result estimate.Result
		want   string
	}{
		{
			name:   "the run adds the flag",
			seed:   []string{"sp:8", "sp-rung:all", "sp-minutes:8.8"},
			spec:   []string{"sp:8", "sp-rung:all", "sp-minutes:8.8"},
			result: estimate.Result{Points: 13, Minutes: 12.3, Inputs: []string{"shape", "jev"}, Rung: "all"},
			want:   "advice: predicted to land over 300 added lines (estimate 12.3 min); consider splitting by file group",
		},
		{
			name:   "the run removes the stale flag",
			seed:   []string{"sp:13", "sp-rung:all", "sp-minutes:12.3", "sp-large"},
			spec:   []string{"sp:13", "sp-rung:all", "sp-minutes:12.3", "sp-large"},
			result: estimate.Result{Points: 8, Minutes: 8.8, Inputs: []string{"shape", "jev"}, Rung: "all"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &specEstimateStub{result: tc.result}
			opts := specLintOpts{Estimate: specLintEstimateRunFor(
				func(string) specEstimateStore { return specEstimateSeed(tc.seed...) },
				func(string) specEstimateRunner { return specEstimateRunnerFor(stub) })}
			spec := cleanSpec("gt-est", 2, "2026-01-01")
			spec.Labels = tc.spec
			var out, errOut bytes.Buffer
			if err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts); err != nil {
				t.Fatalf("lint returned %v, want exit 0 (out %q, err %q)", err, out.String(), errOut.String())
			}
			if stub.calls != 1 {
				t.Fatalf("the estimate ran %d times, want once", stub.calls)
			}
			if tc.want == "" {
				if strings.Contains(errOut.String(), "predicted to land over") {
					t.Errorf("a run that removed the stale sp-large still printed the size advice:\n%s", errOut.String())
				}
				return
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("advice is missing %q:\n%s", tc.want, errOut.String())
			}
		})
	}
}
