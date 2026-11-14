package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// Background slings (gt-5h70s): the tick plans and hands its slings back, and
// later ticks are told which slings still run.

func backgroundEnv(f *fakeSpecTown) specDispatchEnv {
	env := f.env()
	env.Background = true
	env.PerTick = 0 // the free seats
	return env
}

func TestBackgroundTickHandsBackItsSlingsAndRunsNone(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(
		cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 2, "2026-09-29T10:01:00Z"),
	)
	r := runSpecDispatchCycle(backgroundEnv(f))

	if len(f.slung) != 0 {
		t.Fatalf("the tick slung %v itself, want it to hand every sling back", f.slung)
	}
	if len(r.Pending) != 2 || r.Pending[0].Bead != "gt-a" || r.Pending[1].Bead != "gt-b" {
		t.Fatalf("pending = %+v, want gt-a and gt-b in candidate order", r.Pending)
	}
	for _, job := range r.Pending {
		if job.Rig != "gastown" || job.Agent == "" {
			t.Errorf("job %+v lacks its rig or its chosen seat", job)
		}
	}
	// The tick's own accounting reads a tick that planned slings as one that
	// dispatched.
	if len(r.Dispatched) != 2 || !strings.Contains(r.Dispatched[0].Line, "started in the background") {
		t.Errorf("dispatched = %+v, want one 'started in the background' entry per job", r.Dispatched)
	}
	if len(r.Failed) != 0 || len(r.Errors) != 0 {
		t.Errorf("failed %+v errors %v, want none", r.Failed, r.Errors)
	}
}

// A sling still running holds its bead and its seat: the board still shows the
// bead ready until the sling hooks it, and the roster does not show the seat
// until the sling claims it.
func TestBackgroundTickNeverSlingsARunningBeadOrSeatTwice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		cap      int
		inFlight []specInFlight
		want     []string // beads handed back
	}{
		{"nothing running", 1, nil, []string{"gt-a"}},
		{"the running bead is not slung again", 2, []specInFlight{{Bead: "gt-a", Rig: "gastown", Agent: "deepseek-flash"}}, []string{"gt-b"}},
		{"the running sling's seat is taken", 1, []specInFlight{{Bead: "gt-x", Rig: "gastown", Agent: "deepseek-flash"}}, nil},
		{"a seat of the cap with one running has one left", 2, []specInFlight{{Bead: "gt-x", Rig: "gastown", Agent: "deepseek-flash"}}, []string{"gt-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(
				cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
				cleanSpec("gt-b", 2, "2026-09-29T10:01:00Z"),
			)
			f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: tc.cap}}
			env := backgroundEnv(f)
			env.InFlight = tc.inFlight
			r := runSpecDispatchCycle(env)

			var got []string
			for _, job := range r.Pending {
				got = append(got, job.Bead)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("handed back %v, want %v (skipped %+v)", got, tc.want, r.Skipped)
			}
			if len(f.slung) != 0 {
				t.Fatalf("the tick slung %v itself", f.slung)
			}
		})
	}
}

// The seats a running sling fills are in the roster the tick reports, named
// apart from the roster's own live count: the seat is the one the sling took,
// and the tick line says so rather than leaving the reading to arithmetic.
func TestBackgroundTickRosterCountsTheRunningSling(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 2}}
	env := backgroundEnv(f)
	env.InFlight = []specInFlight{{Bead: "gt-x", Rig: "gastown", Agent: "deepseek-flash"}}
	r := runSpecDispatchCycle(env)
	if want := "deepseek-flash 1/2 (0 live + 1 in flight (may double count))"; r.Roster != want {
		t.Fatalf("roster = %q, want %q", r.Roster, want)
	}
}

// TestSpecTickCountsSlingsRunningFromEarlierTicks pins behaviour that already
// exists. It is not a before-and-after test: it passes on the code it was
// written against. A sling still running from an earlier tick is in the in-flight
// set the tick is handed, and specLiveWithInFlight counts it on its seat before
// ChooseSeat runs, so the next tick admits no more slings than the seat has
// room for. The release side, a failed sling leaving the set, is pinned by
// TestAFailedBackgroundSlingIsLoggedLikeAForegroundFailure in
// internal/daemon/spec_slings_test.go.
func TestSpecTickCountsSlingsRunningFromEarlierTicks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		running int // slings still running from earlier ticks, all on the seat
		want    int // slings this tick hands back
	}{
		{"an empty seat takes both", 0, 2},
		{"one running sling leaves one place", 1, 1},
		{"a seat full of running slings takes none", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(
				cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
				cleanSpec("gt-b", 2, "2026-09-29T10:01:00Z"),
			)
			f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 2}}
			env := backgroundEnv(f)
			for i := range tc.running {
				env.InFlight = append(env.InFlight, specInFlight{Bead: "gt-run" + string(rune('a'+i)), Rig: "gastown", Agent: "deepseek-flash"})
			}
			r := runSpecDispatchCycle(env)
			if len(r.Pending) != tc.want {
				t.Fatalf("handed back %d slings, want %d; roster %q", len(r.Pending), tc.want, r.Roster)
			}
		})
	}
}

// A sling in its first minute has no session, so the roster shows no spawn. The
// stagger must still hold the next sling until the running one's start is a gap
// old (gt-vames).
func TestSpecDispatchInFlightHonorsMinSpawnGap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		started time.Duration // the running sling's age at the tick
		want    int           // slings handed back
	}{
		{"started inside the gap", -60 * time.Second, 0},
		{"started before the gap", -91 * time.Second, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(
				cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
				cleanSpec("gt-b", 2, "2026-09-29T10:01:00Z"),
			)
			env := backgroundEnv(f)
			env.Budget.MinSpawnGap = 90 * time.Second
			env.InFlight = []specInFlight{{Bead: "gt-x", Rig: "gastown", Agent: "deepseek-flash", Started: specTestNow.Add(tc.started)}}
			r := runSpecDispatchCycle(env)

			if len(r.Pending) != tc.want {
				t.Fatalf("handed back %+v, want %d (skipped %+v)", r.Pending, tc.want, r.Skipped)
			}
			if len(f.slung) != 0 {
				t.Fatalf("the tick slung %v itself", f.slung)
			}
		})
	}
}

// An in-flight entry written without a start still parses and takes its seat:
// the field is new, and a daemon payload from before it carries none.
func TestSpecInFlightWithoutAStartStillParses(t *testing.T) {
	t.Parallel()
	got, err := parseSpecInFlight(`[{"bead":"gt-x","rig":"gastown","agent":"deepseek-flash"}]`)
	if err != nil || len(got) != 1 || !got[0].Started.IsZero() || got[0].Agent != "deepseek-flash" {
		t.Fatalf("parseSpecInFlight = %+v, %v; want gt-x on deepseek-flash with no start", got, err)
	}
}

// A candidate that names the files a running sling's bead names is not held
// for the overlap: the tick hands it back as a pending sling and says nothing
// about a hold (gt-e1ufo).
func TestBackgroundTickSlingsACandidateOverlappingARunningSling(t *testing.T) {
	t.Parallel()
	const guard = "internal/daemon/overrun_guard.go"
	second := cleanSpec("gt-second", 2, "2026-09-29T10:04:00Z")
	second.Title = "overrun guard nudge uses wait-idle under a 30s context"
	second.Description = specTestDescription + "\n\nToday " + guard + " (~254) calls Nudge with a 30 s context."
	f := newFakeSpecTown(second)
	env := backgroundEnv(f)
	env.InFlight = []specInFlight{{Bead: "gt-first", Rig: "gastown", Agent: "claude-sonnet"}}
	r := runSpecDispatchCycle(env)

	if len(r.Pending) != 1 || r.Pending[0].Bead != "gt-second" {
		t.Fatalf("handed back %+v, want the overlapping candidate handed back to run", r.Pending)
	}
	if len(r.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want no hold line for the overlap", r.Skipped)
	}
}

// The job the tick prints is the job the sling runs: the seat, the resume
// branch and the formula the bead declared survive the report.
func TestBackgroundSlingRunsTheJobTheTickPrinted(t *testing.T) {
	t.Parallel()
	declared := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	declared.Labels = []string{"formula:mol-custom"}
	f := newFakeSpecTown(declared)
	f.seats = []specdispatch.Seat{{Agent: "claude-sonnet", Cap: 1}}
	f.originBranches["gastown/polecat/old"] = true
	f.specs["gt-a"] = withNote(f.specs["gt-a"], "resume_branch: polecat/old")
	r := runSpecDispatchCycle(backgroundEnv(f))
	if len(r.Pending) != 1 {
		t.Fatalf("pending = %+v (skipped %+v, errors %v)", r.Pending, r.Skipped, r.Errors)
	}

	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct{ Pending []specSlingJob }
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Pending) != 1 {
		t.Fatalf("report did not carry the job: %v %s", err, raw)
	}
	job := wire.Pending[0]

	sling := runSpecSlingJob(f.env(), job)
	if len(sling.Dispatched) != 1 || !strings.Contains(sling.Dispatched[0].Line, "gt-a: slung to gastown/p on claude-sonnet") {
		t.Fatalf("sling report = %+v, want gt-a slung to the seat the tick chose", sling)
	}
	if !slices.Equal(f.slung, []string{"gt-a"}) || f.slingSeats[0].Agent != "claude-sonnet" {
		t.Fatalf("slung %v on %+v, want gt-a on claude-sonnet", f.slung, f.slingSeats)
	}
	if f.slungRuns[0].Formula != "mol-custom" {
		t.Fatalf("formula = %q, want the bead's declared mol-custom", f.slungRuns[0].Formula)
	}
	if f.slungResume[0] != "polecat/old" {
		t.Fatalf("resume = %q, want the recorded branch polecat/old", f.slungResume[0])
	}
}

// A background sling that fails labels and annotates its bead as the tick's
// foreground sling does (gt-pto2f, spec-dispatch-failed rules unchanged).
func TestBackgroundSlingFailureLabelsTheBeadLikeAForegroundOne(t *testing.T) {
	t.Parallel()
	slingErr := errors.New("spawn: tmux: boom")
	mk := func() *fakeSpecTown {
		f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
		f.slingErrs["gt-a"] = []error{slingErr}
		return f
	}

	fg := mk()
	fgEnv := fg.env()
	fgReport := runSpecDispatchCycle(fgEnv)

	bg := mk()
	bgEnv := backgroundEnv(bg)
	planned := runSpecDispatchCycle(bgEnv)
	if len(planned.Pending) != 1 {
		t.Fatalf("pending = %+v", planned.Pending)
	}
	bgReport := runSpecSlingJob(bg.env(), planned.Pending[0])

	if len(bgReport.Failed) != 1 || len(fgReport.Failed) != 1 || bgReport.Failed[0].Line != fgReport.Failed[0].Line {
		t.Fatalf("background failed %+v, foreground failed %+v, want the same line", bgReport.Failed, fgReport.Failed)
	}
	if !slices.Equal(bg.labels["gt-a"], fg.labels["gt-a"]) || !slices.Contains(bg.labels["gt-a"], specdispatch.DispatchFailedLabel) {
		t.Fatalf("labels: background %v, foreground %v, want both %s", bg.labels["gt-a"], fg.labels["gt-a"], specdispatch.DispatchFailedLabel)
	}
	if !slices.Equal(bg.notes["gt-a"], fg.notes["gt-a"]) || len(bg.notes["gt-a"]) != 1 {
		t.Fatalf("notes: background %v, foreground %v, want the same one note", bg.notes["gt-a"], fg.notes["gt-a"])
	}
}

// censusJobSeat is the sandboxed seat the job's census tests read. The gateway
// listing below never names it, so a census read answers ErrSeatNotLive with no
// probe and a bare probe is the only other answer a read can give.
const censusJobSeat = "gt-gastown-p0"

// censusJobTown is a town with one sandboxed seat and a gateway fake that lists
// only another sandbox. The job's census is handed the fake gateway's runner.
func censusJobTown(t *testing.T) (string, *sandbox.Fake) {
	t.Helper()
	town := t.TempDir()
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{
		Session: censusJobSeat, Rig: "gastown", Polecat: "p0", Gateway: "gw",
	}); err != nil {
		t.Fatal(err)
	}
	gateway := &sandbox.Fake{Reply: map[string]string{
		"list": `{"sandboxes":[{"name":"gt-gastown-other","phase":"Ready"}]}`,
	}}
	return town, gateway
}

// censusJobEnv is the env a job runs step as its sling with, and the fake
// town's stubs for the label and note a failed sling writes.
func censusJobEnv(t *testing.T, step func() error) specDispatchEnv {
	t.Helper()
	env := newFakeSpecTown().env()
	env.Sling = func(specCandidate, string, specdispatch.SeatChoice) (string, error) {
		if err := step(); err != nil {
			return "", err
		}
		return "p", nil
	}
	return env
}

// censusSeatRead answers the seat's has-session read and reports the probes it
// made.
func censusSeatRead() (probes int, err error) {
	_, err = session.SandboxCensusRead(censusJobSeat, "has-session", nil, func() (bool, error) {
		probes++
		return true, nil
	})
	return probes, err
}

// runCensusJob runs one job through the job run's inner entry point, with the
// fake gateway's runner and env.
func runCensusJob(t *testing.T, town string, gateway *sandbox.Fake, env specDispatchEnv) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	envFor := func(string, string, *specSlingTraces) specDispatchEnv { return env }
	if err := runSpecSlingJobInTown(cmd, town, censusSlingJob, "", nil, gateway.Runner(), envFor); err != nil {
		t.Fatalf("runSpecSlingJobInTown: %v\n%s", err, out.String())
	}
}

// A `gt spec sling` job runs under the per-sling sandbox census the foreground
// sling starts (gt-4pg3uu): a read inside its sling step goes through the
// census, and the census is gone when the job returns, including when the sling
// fails.
//
//testpolicy:allow parallel — swaps the process-wide sandbox routing, which another test must not see mid-run
func TestSpecSlingJobRunsUnderASandboxCensus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		slingErr error
	}{
		{name: "the job succeeds"},
		{name: "the job fails", slingErr: errors.New("spawn: tmux: boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			town, gateway := censusJobTown(t)
			var probes int
			var readErr error
			env := censusJobEnv(t, func() error {
				probes, readErr = censusSeatRead()
				return tc.slingErr
			})
			runCensusJob(t, town, gateway, env)

			if !errors.Is(readErr, session.ErrSeatNotLive) || probes != 0 {
				t.Errorf("the read inside the job = %d probes, %v; want the census's not-live answer and no probe", probes, readErr)
			}
			listCalls := len(gateway.Calls())
			probes, readErr = censusSeatRead()
			if readErr != nil || probes != 1 {
				t.Errorf("the read after the job = %d probes, %v; want the bare probe", probes, readErr)
			}
			if after := len(gateway.Calls()); after != listCalls {
				t.Errorf("a read after the job listed the gateway (%d calls, was %d); the census must end with the job", after, listCalls)
			}
		})
	}
}

// Each `gt spec sling` job starts its own census: the listing the first job
// fetched is not seen by the second (gt-4pg3uu).
//
//testpolicy:allow parallel — swaps the process-wide sandbox routing, which another test must not see mid-run
func TestSpecSlingJobsDoNotShareACensus(t *testing.T) {
	town, gateway := censusJobTown(t)
	underCensus := 0
	env := censusJobEnv(t, func() error {
		if probes, err := censusSeatRead(); errors.Is(err, session.ErrSeatNotLive) && probes == 0 {
			underCensus++
		}
		return nil
	})
	runCensusJob(t, town, gateway, env)
	runCensusJob(t, town, gateway, env)

	if underCensus != 2 {
		t.Errorf("reads under a census = %d, want one per job", underCensus)
	}
	if listCalls := len(gateway.Calls()); listCalls != 2 {
		t.Errorf("gateway listings = %d, want one per job: the first job's listing must not be seen by the second", listCalls)
	}
}

// A background sling continues the assignee-lock-timeout run the tick read: the
// count rides the job through the wire, so a bead whose timeouts arrive through
// `gt spec sling --job` labels on the third one as a foreground sling's does
// (gt-a4cdcd).
func TestBackgroundSlingCarriesTheLockTimeoutCount(t *testing.T) {
	t.Parallel()
	lockTimeout := fmt.Errorf("serializing hook write for gastown/polecats/sbx-flash-1: %w", ErrAssigneeLockTimeout)
	f := newFakeSpecTown(withLockTimeouts(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"), 2))
	f.seats = []specdispatch.Seat{{Agent: "sbx-flash", Cap: 1}}
	f.slingErrs["gt-a"] = []error{lockTimeout}

	planned := runSpecDispatchCycle(backgroundEnv(f))
	if len(planned.Pending) != 1 || planned.Pending[0].LockTimeouts != 2 {
		t.Fatalf("pending = %+v, want one job carrying the count 2", planned.Pending)
	}
	raw, err := json.Marshal(planned)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct{ Pending []specSlingJob }
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Pending) != 1 {
		t.Fatalf("report did not carry the job: %v %s", err, raw)
	}

	report := runSpecSlingJob(f.env(), wire.Pending[0])
	if len(report.Failed) != 1 {
		t.Fatalf("sling report = %+v, want the third timeout to fail the sling", report)
	}
	if !slices.Contains(f.labels["gt-a"], specdispatch.DispatchFailedLabel) {
		t.Fatalf("labels = %v, want %s: the count the tick read reached the sling", f.labels["gt-a"], specdispatch.DispatchFailedLabel)
	}
}

func TestParseSpecInFlight(t *testing.T) {
	t.Parallel()
	if got, err := parseSpecInFlight(""); err != nil || got != nil {
		t.Fatalf("empty = %v, %v, want none", got, err)
	}
	got, err := parseSpecInFlight(`[{"bead":"gt-a","rig":"gastown","agent":"claude-sonnet"}]`)
	if err != nil || len(got) != 1 || got[0].Bead != "gt-a" || got[0].Agent != "claude-sonnet" {
		t.Fatalf("parsed = %+v, %v", got, err)
	}
	if _, err := parseSpecInFlight("not json"); err == nil {
		t.Fatal("a value that is not JSON must be an error, not an empty set")
	}
}

func TestParseSpecSlingJob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		raw     string
		wantErr string // empty: the job parses
	}{
		{"complete", `{"bead":"gt-a","rig":"gastown","agent":"claude-sonnet"}`, ""},
		{"not json", `not json`, "--job is not a sling job: invalid character"},
		{"empty", ``, "--job is not a sling job: unexpected end of JSON input"},
		{"no bead", `{"rig":"gastown","agent":"claude-sonnet"}`, "--job is not a sling job: no bead"},
		{"no rig or agent", `{"bead":"gt-a"}`, "--job is not a sling job: no rig, agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job, err := parseSpecSlingJob(tc.raw)
			if tc.wantErr == "" {
				if err != nil || job.Bead != "gt-a" || job.Rig != "gastown" || job.Agent != "claude-sonnet" {
					t.Fatalf("parsed = %+v, %v, want the job", job, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "nil") {
				t.Errorf("err = %q, must not print a nil error", err)
			}
		})
	}
}

// The operator hold stops a sling the tick planned before the hold was set.
func TestSpecSlingHoldStopsAPlannedSling(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if held := specSlingHold(town, ""); held != nil {
		t.Fatalf("hold = %+v in a town with no hold file, want none", held)
	}
	if err := os.WriteFile(dispatch.HoldFilePath(town), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	held := specSlingHold(town, "")
	if held == nil || !strings.Contains(held.Hold, "operator dispatch hold present") {
		t.Fatalf("hold = %+v, want the operator hold named", held)
	}
}

// A rig's dispatch hold stops a sling the tick planned before the hold was
// set, and only for the held rig: the sling reads the same record the tick
// does, so the hold covers the window between the plan and the sling
// (gt-vty1v).
func TestSpecSlingHoldStopsAPlannedSlingIntoaHeldRig(t *testing.T) {
	t.Parallel()
	town := parkTown(t)
	if held := specSlingHold(town, "testrig"); held != nil {
		t.Fatalf("hold = %+v for a rig with no hold record, want none", held)
	}
	var out bytes.Buffer
	if err := holdRigs(&out, town, []string{"testrig"}, config.RigDispatchHold{
		Since: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), By: "Test Operator", Reason: "drain the landing backlog",
	}); err != nil {
		t.Fatalf("holdRigs = %v\n%s", err, out.String())
	}

	held := specSlingHold(town, "testrig")
	if held == nil || !strings.Contains(held.Hold, "drain the landing backlog") ||
		!strings.Contains(held.Hold, "gt rig release testrig") {
		t.Fatalf("hold = %+v, want the rig's hold named with its reason and release command", held)
	}
	if held := specSlingHold(town, "otherrig"); held != nil {
		t.Fatalf("hold = %+v for an unheld rig beside the held one, want none", held)
	}
}

// A running sling adds one to its agent's live count whether or not the roster
// shows its session yet: the seat may be counted twice until the sling returns,
// which holds back a sling and never adds one.
func TestSpecLiveWithInFlight(t *testing.T) {
	t.Parallel()
	live := map[string]int{"claude-sonnet": 1}
	got := specLiveWithInFlight(live, []specInFlight{{Bead: "gt-a", Agent: "claude-sonnet"}, {Bead: "gt-b", Agent: "deepseek-flash"}})
	if got["claude-sonnet"] != 2 || got["deepseek-flash"] != 1 {
		t.Errorf("live = %v, want claude-sonnet 2 and deepseek-flash 1", got)
	}
	if live["claude-sonnet"] != 1 || len(live) != 1 {
		t.Errorf("the roster's own map = %v, want it left alone", live)
	}
	if got := specLiveWithInFlight(nil, nil); got != nil {
		t.Errorf("nothing in flight = %v, want the roster's map as it is", got)
	}
}

func withNote(s specdispatch.Spec, note string) specdispatch.Spec {
	s.Notes = note
	return s
}
