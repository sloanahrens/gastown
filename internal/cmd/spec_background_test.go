package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
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

// The seats a running sling fills are in the roster the tick reports.
func TestBackgroundTickRosterCountsTheRunningSling(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 2}}
	env := backgroundEnv(f)
	env.InFlight = []specInFlight{{Bead: "gt-x", Rig: "gastown", Agent: "deepseek-flash"}}
	r := runSpecDispatchCycle(env)
	if !strings.Contains(r.Roster, "deepseek-flash 1/2") {
		t.Fatalf("roster = %q, want the running sling counted on its seat", r.Roster)
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
		Since: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), By: "sloan", Reason: "drain the landing backlog",
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
