package land

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCandidateGateRendersTheRun: candidateGate hands the run it made to
// renderCandidate and returns that run beside the rendered verdict. Each row
// is one outcome renderCandidate's switch names, plus the error that wins over
// the state.
func TestCandidateGateRendersTheRun(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("container slot unavailable")
	tests := []struct {
		name string
		res  CandidateResult
		// nilHook leaves the CI-failure watch unset, which must not panic.
		nilHook    bool
		wantPassed bool
		wantExit   int
		wantInfra  bool
		wantErr    error
		wantHook   int
		// wantLog is the substring the Out log must carry; empty means no line.
		wantLog string
	}{
		{
			name:       "passed run renders a passed verdict",
			res:        CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"},
			wantPassed: true,
		},
		{
			name:     "red run hands the log tail to the watch and logs the failure",
			res:      CandidateResult{State: CandidateFailed, Context: "ci / gate (push)", Tail: "--- FAIL: TestThing\n"},
			wantExit: 1,
			wantHook: 1,
			wantLog:  "failed on the candidate",
		},
		{
			name:     "red run with no watch set still logs and returns no error",
			res:      CandidateResult{State: CandidateFailed, Context: "ci / gate (push)"},
			nilHook:  true,
			wantExit: 1,
			wantLog:  "failed on the candidate",
		},
		{
			name:      "silent run is an infrastructure error wrapping ErrCISilence",
			res:       CandidateResult{State: CandidateSilent, Context: "ci / gate (push)"},
			wantInfra: true,
			wantErr:   ErrCISilence,
		},
		{
			name:      "a run that errs is an infrastructure error even when its state reads red",
			res:       CandidateResult{State: CandidateFailed, Context: "ci / gate (push)", Err: errBoom},
			wantInfra: true,
			wantErr:   errBoom,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			l.Candidate = &fakeCandidate{res: tc.res}
			var hooks int
			if !tc.nilHook {
				l.CIFailure = func(CIFailure) { hooks++ }
			}
			var out bytes.Buffer
			l.Out = &out

			cres, gres, err := l.candidateGate(context.Background(), nil, f.repo, f.work, "merged")

			if cres.State != tc.res.State {
				t.Errorf("candidateGate returned state %v, want the run's %v", cres.State, tc.res.State)
			}
			if gres.Passed != tc.wantPassed {
				t.Errorf("Passed = %v, want %v", gres.Passed, tc.wantPassed)
			}
			if len(gres.Steps) != 1 {
				t.Fatalf("steps = %d, want the one CI step", len(gres.Steps))
			}
			step := gres.Steps[0]
			if step.Name != StageCI || step.Command != tc.res.Context || step.Tail != tc.res.Tail {
				t.Errorf("step = %+v, want the CI step named for the run's context and tail", step)
			}
			if step.ExitCode != tc.wantExit {
				t.Errorf("ExitCode = %d, want %d", step.ExitCode, tc.wantExit)
			}
			if tc.wantInfra {
				var infra *InfraError
				if !errors.As(err, &infra) || infra.Stage != StageCI {
					t.Fatalf("error = %T %v, want an *InfraError at the CI stage", err, err)
				}
			} else if err != nil {
				t.Fatalf("error = %v, want none: a red run is a gate result", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !tc.nilHook && hooks != tc.wantHook {
				t.Errorf("CI-failure watch called %d time(s), want %d", hooks, tc.wantHook)
			}
			if tc.wantLog == "" {
				if out.Len() != 0 {
					t.Errorf("log = %q, want no line", out.String())
				}
			} else if !strings.Contains(out.String(), tc.wantLog) {
				t.Errorf("log = %q, want it to carry %q", out.String(), tc.wantLog)
			}
		})
	}
}
