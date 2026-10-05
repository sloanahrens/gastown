package land

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// TestInfraSignaturesMatch walks every row of the table and the shapes that
// must not match: a red that started a step is the work's, and a log that came
// back truncated cannot say a step never started. Every row has to be matched by
// a case here, so adding a signature is a row and a case, nothing else
// (gt-fn9e6.30).
func TestInfraSignaturesMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		facts gateFacts
		// want is the matched signature's name; "" means the red is the work's.
		want string
	}{
		{
			name:  "a cancelled run ended without judging the work",
			facts: gateFacts{RunStatus: candidateRunCancelled},
			want:  "the run ended without judging the work",
		},
		{
			name:  "a skipped run ended without judging the work",
			facts: gateFacts{RunStatus: candidateRunSkipped},
			want:  "the run ended without judging the work",
		},
		{
			name:  "a failed run whose whole log has no step marker never started",
			facts: gateFacts{RunStatus: candidateRunFailure, Log: startFailureFixture(t), LogWhole: true},
			want:  "the runner failed before the first step: the job log carries no step marker (image pull or container start failure)",
		},
		{
			name:  "a failed run whose whole log has a step marker judged the work",
			facts: gateFacts{RunStatus: candidateRunFailure, Log: normalJobFixture(t), LogWhole: true},
		},
		{
			name:  "a truncated log cannot say a step never started",
			facts: gateFacts{RunStatus: candidateRunFailure, Log: strings.Repeat("make gate\n", 100), LogWhole: false},
		},
		{
			name:  "a failed run with no log is the work's red",
			facts: gateFacts{RunStatus: candidateRunFailure},
		},
		{
			name:  "a run still going is the work's red",
			facts: gateFacts{RunStatus: "running"},
		},
		{
			name:  "no run at all is the work's red",
			facts: gateFacts{},
		},
	}
	seen := map[string]bool{}
	for _, sig := range infraSignatures {
		if sig.name == "" || sig.match == nil {
			t.Fatalf("signature %+v has no name or matcher", sig)
		}
		if seen[sig.name] {
			t.Fatalf("signature %q appears twice in the table", sig.name)
		}
		seen[sig.name] = true
	}
	covered := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, ok := matchInfraSignature(tt.facts)
			if ok != (tt.want != "") {
				t.Fatalf("matchInfraSignature = (%q, %v), want a match: %v", sig.name, ok, tt.want != "")
			}
			if sig.name != tt.want {
				t.Fatalf("matched %q, want %q", sig.name, tt.want)
			}
			if ok {
				covered[sig.name] = true
			}
		})
	}
	for _, sig := range infraSignatures {
		if !covered[sig.name] {
			t.Errorf("signature %q is in the table but no case here matches it", sig.name)
		}
	}
}

// TestCandidateGateStartFailureIsInfrastructure drives the captured
// start-failure log through the real gate path: a job whose container image
// could not be pulled ended before any step ran, so the red is infrastructure
// and the error names the signature, never a rework for the work
// (gt-fn9e6.30).
func TestCandidateGateStartFailureIsInfrastructure(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{
		statuses: []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
		runs:     []forgejo.ActionRun{{ID: 7, CommitSHA: sha, Status: "failure"}},
		jobs:     []forgejo.ActionRunJob{{ID: 9, RunID: 7, Name: "gate", Status: "failure"}},
		log:      startFailureFixture(t),
	}
	res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.State != CandidateSilent {
		t.Fatalf("state = %v, want no verdict (err %v)", res.State, res.Err)
	}
	if !errors.Is(res.Err, ErrCISilence) {
		t.Fatalf("err = %v, want one wrapping ErrCISilence", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "no step marker") {
		t.Fatalf("err = %v; want it to name the start-failure signature", res.Err)
	}
	if res.RunStatus != candidateRunFailure {
		t.Fatalf("RunStatus = %q, want the failed run's status", res.RunStatus)
	}
}

// TestCandidateGateStartedStepKeepsTheRed: the head of a real normal job log,
// with a failing test appended, is a work red. The checkout step started, so no
// signature claims it and the polecat gets a rework with the failure
// (gt-fn9e6.30).
func TestCandidateGateStartedStepKeepsTheRed(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{
		statuses: []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
		runs:     []forgejo.ActionRun{{ID: 7, CommitSHA: sha, Status: "failure"}},
		jobs:     []forgejo.ActionRunJob{{ID: 9, RunID: 7, Name: "gate", Status: "failure"}},
		log:      normalJobFixture(t),
	}
	res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if res.State != CandidateFailed {
		t.Fatalf("state = %v, want failed", res.State)
	}
	if !strings.Contains(res.Tail, "--- FAIL: TestSomething") {
		t.Fatalf("tail %q; want the failing test", res.Tail)
	}
}

// normalJobFixture is the captured head of a normal job log -- container
// created, then the step marker -- with a failing test appended: the shape of a
// red the work caused.
func normalJobFixture(t *testing.T) string {
	t.Helper()
	return fixture(t, "normal-job-head.log") + "--- FAIL: TestSomething (0.01s)\n"
}

// startFailureFixture is the captured start-failure log: a job whose container
// image could not be pulled, ending at the Docker error with no step marker.
func startFailureFixture(t *testing.T) string {
	t.Helper()
	return fixture(t, "start-failure.log")
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the %s fixture: %v", name, err)
	}
	return string(b)
}
