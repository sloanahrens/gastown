package land

import (
	"context"
	"reflect"
	"testing"
	"time"

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
		{Package: "example.test/a", Passed: true, Elapsed: 100 * time.Millisecond},
		{Package: "example.test/b", Passed: false, Elapsed: 200 * time.Millisecond},
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

// The Forgejo job log API prefixes every line with a timestamp. The parser
// read none of those lines while its pattern was anchored at the line start,
// so every landing's per-package results came back empty (gt-6hp28).
func TestParseGoTestOutputReadsAForgejoJobLog(t *testing.T) {
	t.Parallel()
	log := "2026-10-21T14:02:11.1234567Z gate: unit tier (every package)\r\n" +
		"2026-10-21T14:03:40.2345678Z ok  \texample.test/a\t0.412s\r\n" +
		"2026-10-21T14:03:41Z ok  \texample.test/b\t(cached)\r\n" +
		"2026-10-21T14:03:42.5Z ?   \texample.test/c\t[no test files]\r\n" +
		"2026-10-21T14:03:43.3456789Z --- FAIL: TestX (0.01s)\r\n" +
		"2026-10-21T14:03:44.4567890Z FAIL\r\n" +
		"2026-10-21T14:03:44.5678901Z FAIL\texample.test/d\t0.2s\r\n" +
		"2026-10-21T14:03:45.6789012Z ok  \texample.test/e\t0.3s\tcoverage: 12.3% of statements\r\n"

	got := parseGoTestOutput(log)

	want := []PackageResult{
		{Package: "example.test/a", Passed: true, Elapsed: 412 * time.Millisecond},
		{Package: "example.test/b", Passed: true},
		{Package: "example.test/d", Passed: false, Elapsed: 200 * time.Millisecond},
		{Package: "example.test/e", Passed: true, Elapsed: 300 * time.Millisecond},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGoTestOutput = %+v, want %+v", got, want)
	}
}

// Packages reads the timestamped log the server returns, ok and FAIL both.
func TestCandidateGatePackagesReadsTimestampedLines(t *testing.T) {
	t.Parallel()
	dir := writeGateWorkflow(t, gateWorkflowYAML)
	client := &fakeForgejo{
		runs: []forgejo.ActionRun{{ID: 7, CommitSHA: "abc123"}},
		jobs: []forgejo.ActionRunJob{{ID: 3, Name: "gate"}},
		fullLog: "2026-10-21T14:03:40.2345678Z ok  \texample.test/a\t0.1s\n" +
			"2026-10-21T14:03:41.2345678Z FAIL\texample.test/b\t0.2s\n",
	}

	got, err := fastGate(client).Packages(context.Background(), dir, CandidateResult{SHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}

	want := []PackageResult{
		{Package: "example.test/a", Passed: true, Elapsed: 100 * time.Millisecond},
		{Package: "example.test/b", Passed: false, Elapsed: 200 * time.Millisecond},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Packages = %+v, want %+v", got, want)
	}
}
