package cmd

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

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

// A candidate that names the files a running sling's bead names is held, as it
// is held by a bead the sling has already hooked (gt-z80p9).
func TestBackgroundTickHoldsACandidateOverlappingARunningSling(t *testing.T) {
	t.Parallel()
	const guard = "internal/daemon/overrun_guard.go"
	second := cleanSpec("gt-second", 2, "2026-09-29T10:04:00Z")
	second.Title = "overrun guard nudge uses wait-idle under a 30s context"
	second.Description = specTestDescription + "\n\nToday " + guard + " (~254) calls Nudge with a 30 s context."
	f := newFakeSpecTown(second)
	env := backgroundEnv(f)
	env.InFlight = []specInFlight{{Bead: "gt-first", Rig: "gastown", Agent: "claude-sonnet", Paths: []string{guard}}}
	r := runSpecDispatchCycle(env)

	if len(r.Pending) != 0 {
		t.Fatalf("handed back %+v, want the overlapping candidate held", r.Pending)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "gt-second: held: gt-first (held by a sling still running on claude-sonnet") {
		t.Fatalf("skipped = %+v, want gt-second held on the running sling's bead", r.Skipped)
	}
	// The rig's own held read is still made: the note must not stand in for it.
	if f.heldRigs["gastown"] != 1 {
		t.Fatalf("held reads = %d, want the rig read once beside the running sling's note", f.heldRigs["gastown"])
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
	got, err := parseSpecInFlight(`[{"bead":"gt-a","rig":"gastown","agent":"claude-sonnet","paths":["x.go"]}]`)
	if err != nil || len(got) != 1 || got[0].Bead != "gt-a" || got[0].Agent != "claude-sonnet" || got[0].Paths[0] != "x.go" {
		t.Fatalf("parsed = %+v, %v", got, err)
	}
	if _, err := parseSpecInFlight("not json"); err == nil {
		t.Fatal("a value that is not JSON must be an error, not an empty set")
	}
}

func withNote(s specdispatch.Spec, note string) specdispatch.Spec {
	s.Notes = note
	return s
}
