package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// TestSpecLintEstimateFlag: gt spec lint --estimate runs the estimate through
// the injected estimator before the check, and the sp-minutes label that run
// wrote decides the verdict against lint_over_minutes (gt-5ynb86).
func TestSpecLintEstimateFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		minutes float64
		want    int
		// wantOut are phrases the verdict line must carry.
		wantOut string
	}{
		{name: "the estimate is at or over the setting", minutes: 12.3, want: specLintExitRefused, wantOut: "sp-minutes:12.3"},
		{name: "the estimate is under the setting", minutes: 8.8, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &specEstimateStub{result: estimate.Result{Points: 13, Minutes: tc.minutes, Inputs: []string{"shape", "jev"}, Rung: "all"}}
			opts := specLintOpts{
				LintOverMinutes: 11,
				Estimate: specLintEstimateRunFor(
					func(string) specEstimateStore { return specEstimateSeed() },
					func(string) specEstimateRunner { return specEstimateRunnerFor(stub) }),
			}
			spec := cleanSpec("gt-est", 2, "2026-01-01")
			var out, errOut bytes.Buffer
			err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts)
			if stub.calls != 1 {
				t.Fatalf("the estimate ran %d times, want once before the check", stub.calls)
			}
			if code, _ := IsSilentExit(err); code != tc.want {
				t.Fatalf("exit code = %d, want %d (err %v, out %q)", code, tc.want, err, out.String())
			}
			if tc.wantOut == "" {
				if !strings.Contains(out.String(), "spec lint ok") {
					t.Errorf("verdict %q, want the clean line", out.String())
				}
				return
			}
			for _, want := range []string{tc.wantOut, "size:", "lint_over_minutes 11", "split the bead into beads of about twenty minutes each"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("verdict %q does not name %q", out.String(), want)
				}
			}
		})
	}
}

// TestSpecLintEstimateFailureIsReported: an estimate that makes none, and one
// whose labels cannot be written, are refused with the run's error and exit 1 —
// the bead has not been judged, so it must not be read as a small one
// (gt-5ynb86).
func TestSpecLintEstimateFailureIsReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		store  func(string) specEstimateStore
		runner func(string) specEstimateRunner
		want   string
	}{
		{
			name:  "the estimator fails",
			store: func(string) specEstimateStore { return specEstimateSeed() },
			runner: func(string) specEstimateRunner {
				return specEstimateRunnerFor(&specEstimateStub{err: errors.New("jev unreachable")})
			},
			want: "no estimate: jev unreachable",
		},
		{
			name:   "the labels cannot be written",
			store:  func(string) specEstimateStore { return specEstimateFailUpdate{specEstimateSeed()} },
			runner: func(string) specEstimateRunner { return specEstimateRunnerFor(specEstimateOK()) },
			want:   "bd update refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := specLintOpts{LintOverMinutes: 11, Estimate: specLintEstimateRunFor(tc.store, tc.runner)}
			spec := cleanSpec("gt-est", 2, "2026-01-01")
			var out, errOut bytes.Buffer
			err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts)
			if code, _ := IsSilentExit(err); code != specLintExitRefused {
				t.Fatalf("exit code = %d, want %d (err %v, out %q)", code, specLintExitRefused, err, out.String())
			}
			if !strings.Contains(out.String(), "refused: estimate:") || !strings.Contains(out.String(), tc.want) {
				t.Errorf("verdict %q does not report the estimate failure %q", out.String(), tc.want)
			}
			if strings.Contains(out.String(), "spec lint ok") {
				t.Errorf("a bead the estimate could not judge was read as clean: %q", out.String())
			}
		})
	}
}

// TestSpecLintEstimateRecordedNoneIsRefused: a bead whose last estimate
// recorded that none could be made is refused when --estimate is asked for,
// naming that reason, even though the unchanged run estimates nothing new — the
// missing label is not read as a small bead (gt-5ynb86).
func TestSpecLintEstimateRecordedNoneIsRefused(t *testing.T) {
	t.Parallel()
	db := specEstimateSeed("sp-rung:none")
	note := "SP ESTIMATE v1 sp:none rung:none hash:" + specEstimateHashOf(t, db) + "\nno estimate: jev unreachable"
	if err := db.AddComment("gt-est", note); err != nil {
		t.Fatal(err)
	}
	stub := specEstimateOK()
	opts := specLintOpts{
		LintOverMinutes: 11,
		Estimate: specLintEstimateRunFor(
			func(string) specEstimateStore { return db },
			func(string) specEstimateRunner { return specEstimateRunnerFor(stub) }),
	}
	spec := cleanSpec("gt-est", 2, "2026-01-01")
	var out, errOut bytes.Buffer
	err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts)
	if stub.calls != 0 {
		t.Errorf("an unchanged bead was estimated %d times, want 0", stub.calls)
	}
	if code, _ := IsSilentExit(err); code != specLintExitRefused {
		t.Fatalf("exit code = %d, want %d (err %v, out %q)", code, specLintExitRefused, err, out.String())
	}
	if !strings.Contains(out.String(), "refused: estimate: no estimate: jev unreachable") {
		t.Errorf("verdict %q does not report the recorded no-estimate", out.String())
	}
}

// TestSpecLintWithoutEstimateNeverEstimates: --estimate is what attaches an
// estimate run, and without it a bead with no sp-minutes label is linted on the
// labels it has — the estimate is never made by the lint itself (gt-5ynb86).
func TestSpecLintWithoutEstimateNeverEstimates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if specLintOptsFor(nil, root, false, io.Discard).Estimate != nil {
		t.Fatal("--estimate off attached an estimate run")
	}
	if specLintOptsFor(nil, root, true, io.Discard).Estimate == nil {
		t.Fatal("--estimate on attached no estimate run")
	}
	spec := cleanSpec("gt-noestimate", 2, "2026-01-01")
	opts := specLintOptsFor(nil, root, false, io.Discard)
	opts.LintOverMinutes = 11
	var out, errOut bytes.Buffer
	if err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts); err != nil {
		t.Fatalf("lint without --estimate = %v, out %q", err, out.String())
	}
	if !strings.Contains(out.String(), "spec lint ok") {
		t.Errorf("verdict %q, want the clean line for a bead with no estimate label", out.String())
	}
}

// TestSpecLintHelpNamesEstimateFlag: the help names --estimate and the run it
// makes, so a filer knows the estimate is opt-in and what it buys (gt-5ynb86).
func TestSpecLintHelpNamesEstimateFlag(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"--estimate", "gt spec estimate"} {
		if !strings.Contains(specLintCmd.Long, want) {
			t.Errorf("gt spec lint --help does not name %q", want)
		}
	}
	flag := specLintCmd.Flags().Lookup("estimate")
	if flag == nil {
		t.Fatal("gt spec lint has no --estimate flag")
	}
	for _, want := range []string{"estimate", "gt spec estimate"} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("the --estimate usage %q does not name %q", flag.Usage, want)
		}
	}
	// The help states what the flag does, not how long an estimate took when it
	// was measured (gt-floskq).
	for _, got := range []struct{ name, text string }{
		{"gt spec lint --help", specLintCmd.Long},
		{"the --estimate usage", flag.Usage},
	} {
		if strings.Contains(got.text, "seconds") {
			t.Errorf("%s still claims the estimate's duration: %q", got.name, got.text)
		}
	}
}

// specEstimateRunnerForContext is an estimate run that observes the deadline of
// the context it is given, so a test sees what bounded the run.
func specEstimateRunnerForContext(fn func(context.Context, *beads.Issue) (estimate.Result, error)) specEstimateRunner {
	return specEstimateRunner{
		Estimate: func(ctx context.Context, _ estimate.Deps, issue *beads.Issue) (estimate.Result, error) {
			return fn(ctx, issue)
		},
		Deps: func() (estimate.Deps, error) { return estimate.Deps{}, nil },
	}
}

// TestSpecLintEstimateRunsUnderTheLintsBound: --estimate's run is bounded by
// the lint's own wait rather than left under an unbounded context, so the
// estimator sees a deadline no later than EstimateWait (gt-floskq).
func TestSpecLintEstimateRunsUnderTheLintsBound(t *testing.T) {
	t.Parallel()
	const wait = 2 * time.Second
	run := specEstimateRunnerForContext(func(ctx context.Context, issue *beads.Issue) (estimate.Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > wait {
			return estimate.Result{}, errors.New("the estimate ran without the lint's bound")
		}
		return estimate.Result{Points: 5, Minutes: 6.33, Inputs: []string{"shape"}, Rung: "shape", Hash: estimate.SectionHash(issue)}, nil
	})
	opts := specLintOpts{
		LintOverMinutes: 11,
		EstimateWait:    wait,
		Estimate: specLintEstimateRunFor(
			func(string) specEstimateStore { return specEstimateSeed() },
			func(string) specEstimateRunner { return run }),
	}
	spec := cleanSpec("gt-est", 2, "2026-01-01")
	var out, errOut bytes.Buffer
	err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts)
	if err != nil {
		t.Fatalf("lint under the bound = %v, out %q", err, out.String())
	}
	if !strings.Contains(out.String(), "spec lint ok") {
		t.Errorf("verdict %q, want the clean line", out.String())
	}
}

// TestSpecLintEstimateTimeoutIsRefused: an estimate that waits past the lint's
// bound is refused with the context error, never passed on a label it did not
// write (gt-floskq).
func TestSpecLintEstimateTimeoutIsRefused(t *testing.T) {
	t.Parallel()
	run := specEstimateRunnerForContext(func(ctx context.Context, _ *beads.Issue) (estimate.Result, error) {
		<-ctx.Done()
		return estimate.Result{}, ctx.Err()
	})
	opts := specLintOpts{
		LintOverMinutes: 11,
		EstimateWait:    20 * time.Millisecond,
		Estimate: specLintEstimateRunFor(
			func(string) specEstimateStore { return specEstimateSeed() },
			func(string) specEstimateRunner { return run }),
	}
	spec := cleanSpec("gt-est", 2, "2026-01-01")
	var out, errOut bytes.Buffer
	err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts)
	if code, _ := IsSilentExit(err); code != specLintExitRefused {
		t.Fatalf("exit code = %d, want %d (err %v, out %q)", code, specLintExitRefused, err, out.String())
	}
	if !strings.Contains(out.String(), "refused: estimate:") || !strings.Contains(out.String(), "context deadline exceeded") {
		t.Errorf("verdict %q does not report the expired estimate", out.String())
	}
	if strings.Contains(out.String(), "spec lint ok") {
		t.Errorf("a bead whose estimate timed out was read as clean: %q", out.String())
	}
}

// TestSpecLintEstimateWithGraphIsRefused: --graph checks an epic's children, so
// --estimate has no one bead to estimate; the pair is refused rather than
// silently dropping the estimate (gt-5ynb86).
func TestSpecLintEstimateWithGraphIsRefused(t *testing.T) {
	t.Parallel()
	err := runSpecLintOnWith(&cobra.Command{}, []string{"gt-epic"}, t.TempDir(), true, slingStores{}, true)
	if err == nil || !strings.Contains(err.Error(), "--estimate") {
		t.Fatalf("runSpecLintOnWith(--graph --estimate) = %v, want a refusal naming --estimate", err)
	}
}
