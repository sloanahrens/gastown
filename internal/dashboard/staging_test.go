package dashboard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// stagingStub answers the two calls the staging ship reader makes, from
// fixtures: each repo's runs, and whether a run's commit contains a landing.
// No Forgejo and no git.
type stagingStub struct {
	runs     map[string][]forgejo.ActionRun
	runsErr  map[string]error
	covers   map[string]bool
	cmpErr   error
	runCalls int
	cmpCalls int
}

func (s *stagingStub) ListRuns(_ context.Context, owner, repo string, _ forgejo.RunFilter) (*forgejo.RunList, error) {
	s.runCalls++
	name := owner + "/" + repo
	if err := s.runsErr[name]; err != nil {
		return nil, err
	}
	return &forgejo.RunList{Runs: s.runs[name]}, nil
}

func (s *stagingStub) CompareCommits(_ context.Context, owner, repo, base, head string) (*forgejo.CompareInfo, error) {
	s.cmpCalls++
	if s.cmpErr != nil {
		return nil, s.cmpErr
	}
	info := &forgejo.CompareInfo{}
	if !s.covers[owner+"/"+repo+"\x00"+base+"\x00"+head] {
		info.TotalCommits = 1
	}
	return info, nil
}

// stagingRun is one fixture run: the commit it built, its own state and when it
// was created and ended.
func stagingRun(id int64, commit, status, created, stopped string) forgejo.ActionRun {
	ref := "main"
	return forgejo.ActionRun{ID: id, CommitSHA: commit, Status: status, PrettyRef: ref, Created: created, Stopped: stopped}
}

func stagingTestReader(stub *stagingStub, repos map[string]string) *StagingReader {
	r := NewStagingReader(stub, repos)
	r.now = func() time.Time { return time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC) }
	return r
}

// TestStagingShipCoveredByItsOwnRun: a landing whose own staging run finished
// well is shipped at that run's finish.
func TestStagingShipCoveredByItsOwnRun(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {stagingRun(1, "aaaa", "success", "2026-10-08T17:00:00Z", "2026-10-08T17:02:00Z")},
		},
		covers: map[string]bool{"sloan/fractals\x00aaaa\x00aaaa": true},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 16, 59, 0, 0, time.UTC))
	if !ok || ship.State != StagingDeployed {
		t.Fatalf("ship = %+v ok %v, want deployed", ship, ok)
	}
	if !ship.At.Equal(time.Date(2026, 10, 8, 17, 2, 0, 0, time.UTC)) {
		t.Fatalf("ship time = %v, want the run's finish", ship.At)
	}
}

// TestStagingShipCoveredByALaterRunAfterASkippedOne: the landing's own run was
// skipped by the workflow's stale-main check, so the landing is dated by the
// later run whose commit contains it.
func TestStagingShipCoveredByALaterRunAfterASkippedOne(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {
				stagingRun(2, "bbbb", "success", "2026-10-08T17:04:00Z", "2026-10-08T17:06:00Z"),
				stagingRun(1, "aaaa", "skipped", "2026-10-08T17:00:00Z", "2026-10-08T17:00:10Z"),
			},
		},
		covers: map[string]bool{
			"sloan/fractals\x00aaaa\x00aaaa": true,
			"sloan/fractals\x00bbbb\x00aaaa": true,
		},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 16, 59, 0, 0, time.UTC))
	if !ok || ship.State != StagingDeployed {
		t.Fatalf("ship = %+v ok %v, want deployed", ship, ok)
	}
	if !ship.At.Equal(time.Date(2026, 10, 8, 17, 6, 0, 0, time.UTC)) {
		t.Fatalf("ship time = %v, want the later run's finish", ship.At)
	}
}

// TestStagingShipEarliestSuccessfulRunDatesTheLanding: two successful runs
// cover the landing, and the earlier one is the deploy the landing waited for.
func TestStagingShipEarliestSuccessfulRunDatesTheLanding(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {
				stagingRun(2, "cccc", "success", "2026-10-08T17:20:00Z", "2026-10-08T17:22:00Z"),
				stagingRun(1, "bbbb", "success", "2026-10-08T17:04:00Z", "2026-10-08T17:06:00Z"),
			},
		},
		covers: map[string]bool{
			"sloan/fractals\x00bbbb\x00aaaa": true,
			"sloan/fractals\x00cccc\x00aaaa": true,
		},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 16, 59, 0, 0, time.UTC))
	if !ok || ship.State != StagingDeployed {
		t.Fatalf("ship = %+v ok %v, want deployed", ship, ok)
	}
	if !ship.At.Equal(time.Date(2026, 10, 8, 17, 6, 0, 0, time.UTC)) {
		t.Fatalf("ship time = %v, want the earliest covering run's finish", ship.At)
	}
}

// TestStagingShipPendingUntilASuccessfulRunCovers: a landing no successful run
// has covered yet is pending, whether the run was created before it, does not
// contain it, or has not ended.
func TestStagingShipPendingUntilASuccessfulRunCovers(t *testing.T) {
	t.Parallel()
	landed := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		runs  []forgejo.ActionRun
		covrs map[string]bool
	}{
		{
			name: "a run that does not contain it",
			runs: []forgejo.ActionRun{stagingRun(1, "zzzz", "success", "2026-10-08T17:01:00Z", "2026-10-08T17:03:00Z")},
		},
		{
			name:  "a run created before it",
			runs:  []forgejo.ActionRun{stagingRun(1, "aaaa", "success", "2026-10-08T16:00:00Z", "2026-10-08T16:02:00Z")},
			covrs: map[string]bool{"sloan/fractals\x00aaaa\x00aaaa": true},
		},
		{
			name:  "a run that has not ended",
			runs:  []forgejo.ActionRun{stagingRun(1, "aaaa", "running", "2026-10-08T17:01:00Z", "1970-01-01T00:00:00Z")},
			covrs: map[string]bool{"sloan/fractals\x00aaaa\x00aaaa": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stagingStub{
				runs:   map[string][]forgejo.ActionRun{"sloan/fractals": tc.runs},
				covers: tc.covrs,
			}
			r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})
			ship, ok := r.Ship("fractals", "aaaa", landed)
			if !ok || ship.State != StagingPending {
				t.Fatalf("ship = %+v ok %v, want pending", ship, ok)
			}
		})
	}
}

// TestStagingShipFailedWhenTheNewestCoveringRunFailed: the landing is still
// pending, and the cell is told the newest covering run's own state.
func TestStagingShipFailedWhenTheNewestCoveringRunFailed(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {stagingRun(1, "aaaa", "failure", "2026-10-08T17:01:00Z", "2026-10-08T17:02:00Z")},
		},
		covers: map[string]bool{"sloan/fractals\x00aaaa\x00aaaa": true},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC))
	if !ok || ship.State != StagingFailed || ship.RunState != "failure" {
		t.Fatalf("ship = %+v ok %v, want failed with the run's state", ship, ok)
	}
}

// TestStagingShipSuccessfulOlderRunBeatsANewerFailure: a landing a successful
// run already deployed keeps its ship time; a later failing run is a new
// failure, not a reason to un-ship it.
func TestStagingShipSuccessfulOlderRunBeatsANewerFailure(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {
				stagingRun(2, "bbbb", "failure", "2026-10-08T17:10:00Z", "2026-10-08T17:11:00Z"),
				stagingRun(1, "aaaa", "success", "2026-10-08T17:01:00Z", "2026-10-08T17:03:00Z"),
			},
		},
		covers: map[string]bool{
			"sloan/fractals\x00aaaa\x00aaaa": true,
			"sloan/fractals\x00bbbb\x00aaaa": true,
		},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC))
	if !ok || ship.State != StagingDeployed {
		t.Fatalf("ship = %+v ok %v, want deployed", ship, ok)
	}
}

// TestStagingShipNoDefinitionWithoutStagingRuns: a repository with no staging
// runs — devops today, a library and scripts repository — has no ship
// definition, so the landing carries no ship time and never reads as pending.
func TestStagingShipNoDefinitionWithoutStagingRuns(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{runs: map[string][]forgejo.ActionRun{"sloan/devops": nil}}
	r := stagingTestReader(stub, map[string]string{"devops": "sloan/devops"})

	if ship, ok := r.Ship("devops", "aaaa", time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("ship = %+v; want no ship definition", ship)
	}
}

// TestStagingShipRunsOnAnotherBranchAreNotTheStagingDeploy: the block follows
// the workflow that deploys main, so a run of it on another ref is not one.
func TestStagingShipRunsOnAnotherBranchAreNotTheStagingDeploy(t *testing.T) {
	t.Parallel()
	run := stagingRun(1, "aaaa", "success", "2026-10-08T17:01:00Z", "2026-10-08T17:02:00Z")
	run.PrettyRef = "land/fr-1ab"
	stub := &stagingStub{
		runs:   map[string][]forgejo.ActionRun{"sloan/fractals": {run}},
		covers: map[string]bool{"sloan/fractals\x00aaaa\x00aaaa": true},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	if ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("ship = %+v; want no ship definition from a non-main run", ship)
	}
}

// TestStagingShipUnreadableRepoIsNoDefinitionNotAnError: a repository the
// viewer cannot read shows no ship time, and never reports an error.
func TestStagingShipUnreadableRepoIsNoDefinitionNotAnError(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{runsErr: map[string]error{"sloan/fractals": errors.New("403")}}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	if ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("ship = %+v; want no ship definition", ship)
	}
}

// TestStagingShipUnansweredCompareLeavesTheLandingPending: an ancestry the
// compare could not answer is not coverage, the same way an unanswered
// ancestry leaves an install's landing waiting.
func TestStagingShipUnansweredCompareLeavesTheLandingPending(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {stagingRun(1, "aaaa", "success", "2026-10-08T17:01:00Z", "2026-10-08T17:02:00Z")},
		},
		cmpErr: errors.New("boom"),
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC))
	if !ok || ship.State != StagingPending {
		t.Fatalf("ship = %+v ok %v, want pending", ship, ok)
	}
}

// TestStagingShipUnknownRigHasNoDefinition: a rig the reader was not given a
// repository for is not read at all.
func TestStagingShipUnknownRigHasNoDefinition(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	if _, ok := r.Ship("devops", "aaaa", time.Now()); ok {
		t.Fatal("an unmapped rig was given a ship definition")
	}
	if stub.runCalls != 0 {
		t.Fatalf("run reads = %d; want none for an unmapped rig", stub.runCalls)
	}
}

// TestStagingShipReusesAReadWithinTheTTL: the Landings table asks once per row
// on every poll, so a rig's runs and a landing's answer are each read once per
// window.
func TestStagingShipReusesAReadWithinTheTTL(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {stagingRun(1, "aaaa", "success", "2026-10-08T17:01:00Z", "2026-10-08T17:02:00Z")},
		},
		covers: map[string]bool{"sloan/fractals\x00aaaa\x00aaaa": true},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})
	landed := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		if _, ok := r.Ship("fractals", "aaaa", landed); !ok {
			t.Fatal("no ship definition")
		}
	}
	if stub.runCalls != 1 || stub.cmpCalls != 1 {
		t.Fatalf("read %d run lists and %d compares; want one of each", stub.runCalls, stub.cmpCalls)
	}

	// A second landing in the same repo reuses the run list but asks its own
	// ancestry question: the cache is per landing, not per repo.
	if _, ok := r.Ship("fractals", "bbbb", landed); !ok {
		t.Fatal("no ship definition")
	}
	if stub.runCalls != 1 || stub.cmpCalls != 2 {
		t.Fatalf("after a second landing: %d run lists, %d compares; want the run list reused",
			stub.runCalls, stub.cmpCalls)
	}
}

// TestStagingShipRunKeysArePerRepo: two rigs' identical commit names do not
// share an answer.
func TestStagingShipRunKeysArePerRepo(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {stagingRun(1, "aaaa", "success", "2026-10-08T17:01:00Z", "2026-10-08T17:02:00Z")},
			"sloan/beaver":   {stagingRun(1, "aaaa", "success", "2026-10-08T17:01:00Z", "2026-10-08T17:05:00Z")},
		},
		covers: map[string]bool{
			"sloan/fractals\x00aaaa\x00aaaa": true,
			"sloan/beaver\x00aaaa\x00aaaa":   true,
		},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals", "beaver": "sloan/beaver"})
	landed := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)

	fr, _ := r.Ship("fractals", "aaaa", landed)
	bv, _ := r.Ship("beaver", "aaaa", landed)
	if !fr.At.Equal(time.Date(2026, 10, 8, 17, 2, 0, 0, time.UTC)) || !bv.At.Equal(time.Date(2026, 10, 8, 17, 5, 0, 0, time.UTC)) {
		t.Fatalf("fractals %v beaver %v; want each repo's own run", fr.At, bv.At)
	}
}

// TestStagingReaderIsSafeOnNil: the tracker holds a nil reader before the
// dashboard wires one, and every call has to answer "no ship definition"
// rather than panic.
func TestStagingReaderIsSafeOnNil(t *testing.T) {
	t.Parallel()
	var r *StagingReader
	if _, ok := r.Ship("fractals", "aaaa", time.Now()); ok {
		t.Fatal("a nil reader gave a ship definition")
	}
}

// TestStagingShipRunsAreOrderedByTheirOwnStamps: the reader orders the walk by
// the created stamp it parsed, so the API's order does not decide which run is
// the earliest.
func TestStagingShipRunsAreOrderedByTheirOwnStamps(t *testing.T) {
	t.Parallel()
	stub := &stagingStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals": {
				stagingRun(1, "aaaa", "failure", "2026-10-08T17:01:00Z", "2026-10-08T17:02:00Z"),
				stagingRun(2, "bbbb", "success", "2026-10-08T17:04:00Z", "2026-10-08T17:06:00Z"),
			},
		},
		covers: map[string]bool{
			"sloan/fractals\x00aaaa\x00aaaa": true,
			"sloan/fractals\x00bbbb\x00aaaa": true,
		},
	}
	r := stagingTestReader(stub, map[string]string{"fractals": "sloan/fractals"})

	ship, ok := r.Ship("fractals", "aaaa", time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC))
	if !ok || ship.State != StagingDeployed {
		t.Fatalf("ship = %+v ok %v, want the later successful run to deploy it", ship, ok)
	}
	if !ship.At.Equal(time.Date(2026, 10, 8, 17, 6, 0, 0, time.UTC)) {
		t.Fatalf("ship time = %v, want the successful run's finish", ship.At)
	}
}
