package land

import (
	"context"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// Packages reads the gate job's per-package ok and FAIL lines from the run that
// tested the candidate's commit, up to the full-log cap. It reads only: the
// verdict the gate already reached does not depend on it (gt-bp9qd).
func TestCandidateGatePackagesReadsTheGateJobLog(t *testing.T) {
	t.Parallel()
	dir := writeGateWorkflow(t, gateWorkflowYAML)
	client := &fakeForgejo{
		runs:    []forgejo.ActionRun{{ID: 7, CommitSHA: "abc123"}},
		jobs:    []forgejo.ActionRunJob{{ID: 3, Name: "gate"}},
		log:     "tail with no package lines\n",
		fullLog: "ok  \texample.test/a\t0.1s\nFAIL\texample.test/b\t0.2s\nFAIL\n",
	}

	got, err := fastGate(client).Packages(context.Background(), dir, CandidateResult{SHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}

	want := []PackageResult{
		{Package: "example.test/a", Passed: true},
		{Package: "example.test/b", Passed: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Packages = %+v, want %+v", got, want)
	}
	if last := client.limits[len(client.limits)-1]; last != candidateFullLogBytes {
		t.Errorf("the job log was read at %d bytes, want the full-log cap %d", last, candidateFullLogBytes)
	}
}

// A commit no run tested has no per-package results, and says so.
func TestCandidateGatePackagesNeedsTheRunThatTestedTheCommit(t *testing.T) {
	t.Parallel()
	dir := writeGateWorkflow(t, gateWorkflowYAML)
	client := &fakeForgejo{runs: []forgejo.ActionRun{{ID: 7, CommitSHA: "other"}}}

	if _, err := fastGate(client).Packages(context.Background(), dir, CandidateResult{SHA: "abc123"}); err == nil {
		t.Error("Packages read a run that tested another commit")
	}
}
