package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The dispatchable-ready-bead predicate and the pool seat picture outlive the
// idle-seat dispatch check that introduced them (gt-rwp7z.3): the spec
// dispatcher's roster and the pool's admission read the seat helpers, and the
// ready board reads the predicate.

func TestIsActionableReadyBead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		issue *beads.Issue
		want  bool
	}{
		{"plain P2 task", &beads.Issue{ID: "gt-1", Title: "fix the thing", Priority: 2}, true},
		{"plain P0 bug", &beads.Issue{ID: "gt-2", Title: "[bug] broken", Priority: 0}, true},
		{"nil issue", nil, false},
		{"P3 backlog", &beads.Issue{ID: "gt-3", Title: "someday", Priority: 3}, false},
		{"unset priority", &beads.Issue{ID: "gt-4", Title: "unscored", Priority: -1}, false},
		{
			"agent bead",
			&beads.Issue{ID: "gt-gastown-polecat-flint", Title: "gt-gastown-polecat-flint", Priority: 2, Labels: []string{"gt:agent"}},
			false,
		},
		{
			"merge slot",
			&beads.Issue{ID: "gt-zdcv", Title: "merge-slot", Priority: 0, Labels: []string{"gt:merge-slot"}},
			false,
		},
		{
			"escalation",
			&beads.Issue{ID: "gt-5", Title: "[HIGH] disk full", Priority: 1, Labels: []string{"gt:escalation"}},
			false,
		},
		{
			"escalation titled without its label",
			&beads.Issue{ID: "gt-6", Title: "[CRITICAL] dolt is down", Priority: 0},
			false,
		},
		{
			"state collapse",
			&beads.Issue{ID: "gt-7", Title: "STATE_COLLAPSE gt-abc closed, MR still open", Priority: 1},
			false,
		},
		{
			"epic container",
			&beads.Issue{ID: "gt-8", Title: "[epic] de-flake the suite", Priority: 1, Type: "epic"},
			false,
		},
		{
			"label case and padding",
			&beads.Issue{ID: "gt-9", Title: "agent-ish", Priority: 2, Labels: []string{" GT:AGENT "}},
			false,
		},
		{
			// The operator's own work is not dispatchable: a sling of it
			// refuses (gt-21pl0).
			"operator-reserved bead",
			&beads.Issue{ID: "gt-10", Title: "Hand-run audit", Priority: 1, Labels: []string{"operator"}},
			false,
		},
		{
			"another label on the same bead shape",
			&beads.Issue{ID: "gt-11", Title: "Ordinary work", Priority: 1, Labels: []string{"run-blocker"}},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isActionableReadyBead(tc.issue, 2, nil); got != tc.want {
				t.Errorf("isActionableReadyBead = %v, want %v", got, tc.want)
			}
		})
	}
}

// The ceiling is the operator's, not a constant (gt-h2kyc): the same bead is
// backlog at the default and dispatchable work at a raised one, and an
// unscored bead is never dispatchable.
func TestIsActionableReadyBeadHonorsTheCeiling(t *testing.T) {
	t.Parallel()
	p3 := &beads.Issue{ID: "gt-3", Title: "someday", Priority: 3}
	if isActionableReadyBead(p3, 2, nil) {
		t.Error("a P3 bead is dispatchable at the default P2 ceiling")
	}
	if !isActionableReadyBead(p3, 4, nil) {
		t.Error("a P3 bead is backlog at a P4 ceiling")
	}
	if isActionableReadyBead(&beads.Issue{ID: "gt-u", Title: "unscored", Priority: -1}, 4, nil) {
		t.Error("an unscored bead is dispatchable")
	}
}

// TestIsActionableReadyBead_HoldsRedMainBeadWhileItsRevertIsInFlight is
// gt-1fiv4: a bead the rig's red-main owner filed is not work to dispatch
// while that owner is undoing the same breakage. The predicate is
// specdispatch.RedMainHold's, so the dispatcher and the fold hold the same
// beads (gt-zkdwt).
func TestIsActionableReadyBead_HoldsRedMainBeadWhileItsRevertIsInFlight(t *testing.T) {
	t.Parallel()
	redMain := &beads.Issue{
		ID: "gt-3u7zu", Title: "gastown post-land test red", Priority: 1,
		Labels: []string{specdispatch.LabelRedMain},
	}
	revert := &specdispatch.Revert{Culprit: "gt-cq5gb"}
	plain := &beads.Issue{ID: "gt-plain", Title: "ordinary work", Priority: 1}

	if isActionableReadyBead(redMain, 2, revert) {
		t.Error("a red-main bead is dispatchable while the rig's revert of the same breakage is in flight")
	}
	// The hold is the label's: a revert in flight does not hold the rig's
	// other work, and a red-main bead is work again once the revert is gone.
	if !isActionableReadyBead(plain, 2, revert) {
		t.Error("a bead without the red-main label is held by another bead's revert")
	}
	if !isActionableReadyBead(redMain, 2, nil) {
		t.Error("a red-main bead is held with no revert in flight")
	}
	if !isActionableReadyBead(redMain, 2, &specdispatch.Revert{}) {
		t.Error("a revert with no culprit is no hold")
	}
}

// TestIsActionableReadyBead_AbsentRedMainStateCountsAsToday is the compat half
// of gt-1fiv4: a rig with no red-main state file reads as no revert, so the
// predicate counts its beads exactly as it did before the hold existed.
func TestIsActionableReadyBead_AbsentRedMainStateCountsAsToday(t *testing.T) {
	t.Parallel()
	bead := &beads.Issue{
		ID: "gt-3u7zu", Title: "gastown post-land test red", Priority: 1,
		Labels: []string{specdispatch.LabelRedMain},
	}

	rv := rigRevertInFlight(t.TempDir(), "gastown", func(string) bool { return false })
	if rv != nil {
		t.Fatalf("absent red-main state = %+v, want no revert", rv)
	}
	if !isActionableReadyBead(bead, 2, rv) {
		t.Error("a red-main bead is held by a rig whose red-main state file does not exist")
	}
}

// ── The seat picture's sources (gt-thy6r) ───────────────────────────────────

// writeLandingSeat stages a seat mid-landing: the polecat's directory, so the
// seat exists, and the intent record `gt done` leaves behind — desired
// submitted, naming the work bead it handed to the landing worker.
func writeLandingSeat(t *testing.T, townRoot, rigName, polecatName, workBead string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, rigName, "polecats", polecatName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentRecord(t, intent.Seat{Rig: rigName, Role: constants.RolePolecat, Name: polecatName}, townRoot,
		intent.Record{Desired: intent.DesiredSubmitted, WorkBead: workBead})
}

// writeIntentRecord writes an intent record at its own seat path.
func writeIntentRecord(t *testing.T, seat intent.Seat, townRoot string, rec intent.Record) {
	t.Helper()
	path := seat.Path(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSeatClaim stages the claim a sling holds on a seat while its session is
// still starting, at the path and in the shape poolSeatClaimSessions reads.
func writeSeatClaim(t *testing.T, townRoot, id, agent string) {
	t.Helper()
	dir := poolSeatClaimDir(townRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(poolSeatClaim{ID: id, PID: os.Getpid(), Agent: agent, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// submittedWork answers the batched seat-work read with the named beads as
// they look while they are still waiting to land.
func submittedWork(ids ...string) poolSeatWorkFunc {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	return func(got []string) (map[string]*beads.Issue, error) {
		out := map[string]*beads.Issue{}
		for _, id := range got {
			if want[id] {
				out[id] = &beads.Issue{ID: id, Status: string(beads.StatusOpen), Labels: []string{land.LabelReadyToLand}}
			}
		}
		return out, nil
	}
}

// TestPoolSeatSessionsCountsEveryKindOfTakenSeat pins the three sources the
// picture is built from — the live session, the in-flight claim a sling writes
// before its session exists, and the seat mid-landing — against the fourth
// state, a seat that is simply free. The mid-landing case is the one the
// deleted seat-refill plugin and the first cut of the Go fold both counted as
// free (gt-thy6r, gt-59o9, gt-t8q5).
func TestPoolSeatSessionsCountsEveryKindOfTakenSeat(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)
	live := &fakeLister{sessions: map[string]map[string]string{
		"gt-jade": {"GT_ROLE": "gastown/polecats/jade", "GT_AGENT": "deepseek-flash"},
	}}

	cases := []struct {
		name    string
		lister  sessionLister
		claim   bool
		landing bool
		want    int
	}{
		{"a live session holds the seat", live, false, false, 1},
		{"an in-flight claim holds the seat", &fakeLister{}, true, false, 1},
		{"a seat mid-landing holds the seat", &fakeLister{}, false, true, 1},
		{"no session, no claim, no submission: free", &fakeLister{}, false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			work := submittedWork()
			if c.claim {
				writeSeatClaim(t, town, "claim-1", "deepseek-flash")
			}
			if c.landing {
				writeLandingSeat(t, town, "gastown", "ruby", "gt-ruby")
				work = submittedWork("gt-ruby")
			}

			got, err := poolSeatSessionsWith(c.lister, town, nil, work, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			if n := poolSeatCount("deepseek-flash", got); n != c.want {
				t.Fatalf("seat count = %d, want %d (%+v)", n, c.want, got)
			}
		})
	}
}

// TestPoolSeatSessionsCountsDeadHookedSeats pins gt-tldj4: a polecat whose tmux
// session is gone but whose bead is still hooked — its supervised restart is
// due — holds the pool's seat, the way `gt polecat list` counts it, while a
// seat whose bead is terminal or that has no hooked bead does not (gt-b9ud0
// unchanged). The last case is the restart transition one step on: the same
// seat, live again, is one seat, so the restart cannot push the town over the
// cap.
func TestPoolSeatSessionsCountsDeadHookedSeats(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)

	held := polecat.WorkstateDisposition{
		Verdict:              polecat.WorkstateVerdictNeedsRecovery,
		Reason:               "hook-still-set",
		NeedsRecovery:        true,
		CountsTowardCapacity: true,
		ReuseStatus:          "idle-recovery-needed",
	}
	finished := polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictSafeToNuke}

	cases := []struct {
		name     string
		state    poolSeatState
		sessions map[string]map[string]string
		want     int
	}{
		{
			name:  "dead session with the bead still hooked holds the seat",
			state: poolSeatState{Disposition: held, HookHeld: true},
			want:  1,
		},
		{
			name: "a terminal bead is not held by a dead session",
			// CountsTowardCapacity is true on purpose: the terminal bead alone
			// must keep the seat out, not the capacity field.
			state: poolSeatState{Disposition: polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictNeedsRecovery, CountsTowardCapacity: true}, HookHeld: false},
			want:  0,
		},
		{
			name:  "no hooked bead is not held by a dead session",
			state: poolSeatState{Disposition: finished, HookHeld: false},
			want:  0,
		},
		{
			name:  "a hooked seat whose state does not count toward capacity is not held",
			state: poolSeatState{Disposition: finished, HookHeld: true},
			want:  0,
		},
		{
			name:  "the same seat live again is still exactly one seat",
			state: poolSeatState{Disposition: held, HookHeld: true},
			sessions: map[string]map[string]string{
				"gt-ember": {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"},
			},
			want: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "ember")

			disposition := func(rigName, polecatName string) (poolSeatState, error) {
				if rigName != "gastown" || polecatName != "ember" {
					t.Fatalf("unexpected seat read %s/%s", rigName, polecatName)
				}
				return c.state, nil
			}
			got, err := poolSeatSessionsWith(&fakeLister{sessions: c.sessions}, town, disposition, nil, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			if n := poolSeatCount("deepseek-flash", got); n != c.want {
				t.Fatalf("seat count = %d, want %d (%+v)", n, c.want, got)
			}
		})
	}
}

// A sandbox in a terminal failure phase is not a working seat even when its tmux
// still answers for it: the roster counts it dead-hooked while its bead is
// hooked, so a full pool says why it is full and no sling reads the seat free
// (gt-tj446). A seat it holds no work on is free, and a caller with no town (no
// sandbox registry to read) keeps every session as before.
func TestPoolSeatSessionsCountsADeadSandboxSeatAsDeadHooked(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)
	held := poolSeatState{
		Disposition: polecat.WorkstateDisposition{
			Verdict: polecat.WorkstateVerdictNeedsRecovery, NeedsRecovery: true, CountsTowardCapacity: true,
		},
		HookHeld: true,
	}
	free := poolSeatState{Disposition: polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictSafeToNuke}}
	const sandbox = "gt-ember"
	answering := &fakeLister{sessions: map[string]map[string]string{
		sandbox: {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"},
	}}

	cases := []struct {
		name     string
		state    poolSeatState
		dead     sandboxSeatDeadFunc
		want     int
		wantHeld bool
	}{
		{"a dead sandbox seat holding work is dead-hooked", held, func(string) bool { return true }, 1, true},
		{"a dead sandbox seat holding nothing is free", free, func(string) bool { return true }, 0, false},
		{"a live sandbox seat is working", held, func(string) bool { return false }, 1, false},
		{"no town means no phase read, so the seat is kept", held, nil, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "ember")
			disposition := func(rigName, polecatName string) (poolSeatState, error) {
				if rigName != "gastown" || polecatName != "ember" {
					t.Fatalf("unexpected seat read %s/%s", rigName, polecatName)
				}
				return c.state, nil
			}

			got, err := poolOccupiedSessions(answering, town, disposition, nil, pool, time.Now, c.dead)
			if err != nil {
				t.Fatalf("poolOccupiedSessions: %v", err)
			}
			if n := poolSeatCount("deepseek-flash", got); n != c.want {
				t.Fatalf("seat count = %d, want %d (%+v)", n, c.want, got)
			}
			roster := specRosterFrom(got, nil)
			if deadHooked := roster.DeadHooked["deepseek-flash"]; (deadHooked > 0) != c.wantHeld {
				t.Fatalf("roster dead-hooked = %d, want held=%v (%+v)", deadHooked, c.wantHeld, roster)
			}
			for _, s := range got {
				if s.deadHooked != (c.wantHeld && s.rig == "gastown") {
					t.Fatalf("session %+v deadHooked = %v, want %v", s, s.deadHooked, c.wantHeld)
				}
			}
		})
	}
}

// TestPoolSeatSessionsRestartKeepsTheCount walks the transition the fix is for:
// the crashed seat is counted while its session is gone, and stays counted when
// the supervisor restarts it. The count is the same on both sides, so the
// restart can no longer be the tick that pushes the town past its cap.
func TestPoolSeatSessionsRestartKeepsTheCount(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeTownPolecatDir(t, town, "gastown", "ember")

	disposition := func(rigName, polecatName string) (poolSeatState, error) {
		return poolSeatState{
			Disposition: polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictNeedsRecovery, NeedsRecovery: true, CountsTowardCapacity: true},
			HookHeld:    true,
		}, nil
	}

	crashed, err := poolSeatSessionsWith(&fakeLister{}, town, disposition, nil, pool)
	if err != nil {
		t.Fatalf("poolSeatSessionsWith (crashed): %v", err)
	}
	if n := poolSeatCount("deepseek-flash", crashed); n != 1 {
		t.Fatalf("a crashed seat holds its seat: count = %d (%+v)", n, crashed)
	}

	restarted := &fakeLister{sessions: map[string]map[string]string{
		"gt-ember": {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"},
	}}
	after, err := poolSeatSessionsWith(restarted, town, disposition, nil, pool)
	if err != nil {
		t.Fatalf("poolSeatSessionsWith (restarted): %v", err)
	}
	if n := poolSeatCount("deepseek-flash", after); n != 1 {
		t.Fatalf("the restart must not change the count: got %d (%+v)", n, after)
	}
}

// writeTownPolecatDir makes one rig's polecat worktree directory, the seat
// enumeration poolDeadHookedSessions reads.
func writeTownPolecatDir(t *testing.T, townRoot, rigName, polecatName string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, rigName, "polecats", polecatName), 0o755); err != nil {
		t.Fatalf("mkdir polecat %s/%s: %v", rigName, polecatName, err)
	}
}

// TestPoolLandingSeatEndsWithTheLabel pins that the intent record is only the
// cheap half of the signal: the bead decides. A submission pulled back for
// rework, or already landed, leaves a record the patrol scan has not corrected
// yet (gt-xs1ni); its bead no longer carries gt:ready-to-land, so the seat is
// free rather than held for a wait that is over.
func TestPoolLandingSeatEndsWithTheLabel(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)
	town := t.TempDir()
	writeLandingSeat(t, town, "gastown", "ruby", "gt-ruby")

	// The record says submitted, the bead says otherwise.
	over := func(got []string) (map[string]*beads.Issue, error) {
		out := map[string]*beads.Issue{}
		for _, id := range got {
			out[id] = &beads.Issue{ID: id, Status: string(beads.StatusOpen)}
		}
		return out, nil
	}
	got, err := poolSeatSessionsWith(&fakeLister{}, town, nil, over, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no occupied seat for a bead that is no longer submitted, got %+v", got)
	}

	// A record with no work bead names nothing to confirm, so it holds nothing.
	writeLandingSeat(t, town, "gastown", "opal", "")
	got, err = poolSeatSessionsWith(&fakeLister{}, town, nil, over, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no occupied seat for a submission with no bead, got %+v", got)
	}
}

// TestPoolLandingSeatSkipsASeatThatIsGone: an intent record outlives the seat
// it belongs to, and a seat that is gone holds nothing. Records that are not a
// polecat's — a witness, refinery or crew seat — hold no pool seat either.
func TestPoolLandingSeatSkipsASeatThatIsGone(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)
	town := t.TempDir()

	// A submitted record with no polecat directory behind it.
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ghost"}, town,
		intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ghost"})
	// A submitted record for a seat that is not a polecat's.
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: "witness"}, town,
		intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-witness"})

	got, err := poolSeatSessionsWith(&fakeLister{}, town, nil, submittedWork("gt-ghost", "gt-witness"), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no occupied seat, got %+v", got)
	}
}

// TestSpecRosterCountsACompletingSeatOnce is gt-xmcyk: `gt done` writes its
// Submitted record and then ends the session, and for those seconds the seat
// sits in both of the roster's sources at once. Counting both read 4/3 on a
// full pool and refused the tick a seat it had (2026-10-04 19:17:35 and
// 19:28:34). The roster here is the spec dispatcher's — the same
// poolSeatSessions → specRosterFrom path the tick line prints.
func TestSpecRosterCountsACompletingSeatOnce(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)
	ts := config.NewTownSettings()
	ts.PolecatPool = pool

	jade := map[string]map[string]string{
		"gt-jade": {"GT_ROLE": "gastown/polecats/jade", "GT_AGENT": "deepseek-flash"},
	}
	threeLive := map[string]map[string]string{
		"gt-jade":  {"GT_ROLE": "gastown/polecats/jade", "GT_AGENT": "deepseek-flash"},
		"gt-ruby":  {"GT_ROLE": "gastown/polecats/ruby", "GT_AGENT": "deepseek-flash"},
		"gt-ember": {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"},
	}

	cases := []struct {
		name     string
		sessions map[string]map[string]string
		landing  bool
		want     int
	}{
		{"a seat live and still submitting counts once", jade, true, 1},
		{"a seat mid-landing with no session counts once", nil, true, 1},
		{"a live seat with no record counts once", jade, false, 1},
		{"the roster after gt done reads 3/3 while the session exits", threeLive, true, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			work := submittedWork()
			if c.landing {
				writeLandingSeat(t, town, "gastown", "jade", "gt-jade")
				work = submittedWork("gt-jade")
			}

			sessions, err := poolSeatSessionsWith(&fakeLister{sessions: c.sessions}, town, nil, work, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			if got := specRosterFrom(sessions, ts).Live["deepseek-flash"]; got != c.want {
				t.Fatalf("roster = %d/%d, want %d/%d (%+v)", got, 3, c.want, 3, sessions)
			}
		})
	}
}

// TestSessionlessSeatsCountOnTheAgentTheyWereSlung is gt-2q283: a seat with no
// tmux session — mid-landing after `gt done`, or dead-hooked and waiting for its
// supervised restart — is counted on the agent it was slung on, read from its
// intent record, rather than under the pool's own agent. The town is the shape
// of 2026-10-14: a two-seat claude-sonnet pool (the pool's only seat now, so the one
// a seat with no record falls back to) and a two-seat deepseek-flash hooked seat. A finished
// flash seat waiting to land read as Sonnet then, which made the tick line
// claude-sonnet 4/2 (1 dead-hooked), deepseek-flash 0/2 with two Sonnet sessions
// in the town.
func TestSessionlessSeatsCountOnTheAgentTheyWereSlung(t *testing.T) {
	t.Parallel()
	// The pool's only seat, which an unrecorded seat falls back to.
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{"claude-sonnet": {Cap: 2}}, MaxTotal: intPtr(4)}
	// A map of two seats, the town's default agent being one of them. A seat
	// that records no agent is on neither by its record, and its restart
	// starts it on the polecat role default (the default agent, now that
	// role_agents.polecat is retired), so that is the seat it holds.
	seatMapPool := &config.PolecatPool{
		Seats:    map[string]config.SeatConfig{"claude-sonnet": {Cap: 1}, "claude-haiku": {Cap: 1}},
		MaxTotal: intPtr(2),
	}
	ts := config.NewTownSettings()
	ts.PolecatPool = pool

	cases := []struct {
		name string
		// seatMap runs the case on seatMapPool instead of pool.
		seatMap bool
		// deadHooked stages the seat as a crashed polecat whose bead is still
		// hooked instead of one mid-landing; one is no session at all and the
		// other is a session that ended.
		deadHooked bool
		// slungAgent is the agent the seat's record names. Empty is a seat
		// slung before the record existed, or one the sling left to the
		// pool's only seat.
		slungAgent string
		want       string
	}{
		{
			name:       "a seat mid-landing slung on deepseek-flash counts on deepseek-flash",
			slungAgent: "deepseek-flash",
			want:       "deepseek-flash",
		},
		{
			name:       "a dead-hooked seat counts on the agent it was slung on",
			deadHooked: true,
			slungAgent: "deepseek-flash",
			want:       "deepseek-flash",
		},
		{
			name: "a sessionless seat with no agent record falls back to the pool's only seat",
			want: "claude-sonnet",
		},
		{
			name:       "a dead-hooked seat with no agent record falls back to the pool's only seat",
			deadHooked: true,
			want:       "claude-sonnet",
		},
		{
			name:       "in a multi-seat map a dead-hooked seat with no agent record counts on the role default seat",
			seatMap:    true,
			deadHooked: true,
			want:       "claude-sonnet",
		},
		{
			name:    "in a multi-seat map a seat mid-landing with no agent record counts on the role default seat",
			seatMap: true,
			want:    "claude-sonnet",
		},
		{
			name:       "in a multi-seat map a dead-hooked seat still counts on the agent it was slung on",
			seatMap:    true,
			deadHooked: true,
			slungAgent: "claude-haiku",
			want:       "claude-haiku",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "jade")
			pool, ts := pool, ts
			if c.seatMap {
				pool = seatMapPool
				// The restart reads the default agent from the town's settings.
				ts = config.NewTownSettings()
				ts.DefaultAgent = "claude-sonnet"
				ts.PolecatPool = seatMapPool
				if err := config.SaveTownSettings(config.TownSettingsPath(town), ts); err != nil {
					t.Fatalf("SaveTownSettings: %v", err)
				}
			}

			seat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "jade"}
			rec := intent.Record{Agent: c.slungAgent}
			var disposition polecatDispositionFunc
			var work poolSeatWorkFunc
			if c.deadHooked {
				// A crashed polecat whose bead is still hooked: the seat-state
				// read is what holds it, and the record adds only the agent.
				disposition = func(_, _ string) (poolSeatState, error) {
					return poolSeatState{
						Disposition: polecat.WorkstateDisposition{
							Verdict:              polecat.WorkstateVerdictNeedsRecovery,
							NeedsRecovery:        true,
							CountsTowardCapacity: true,
						},
						HookHeld: true,
					}, nil
				}
			} else {
				rec.Desired = intent.DesiredSubmitted
				rec.WorkBead = "gt-jade"
				work = submittedWork("gt-jade")
			}
			writeIntentRecord(t, seat, town, rec)

			sessions, err := poolSeatSessionsWith(&fakeLister{}, town, disposition, work, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			r := specRosterFrom(sessions, ts)
			if got := r.Live[c.want]; got != 1 {
				t.Fatalf("roster counted the seat %d times on %s, want 1 (%+v)\nthe sessionless seat read as: %+v",
					got, c.want, r.Live, sessions)
			}
			// Neither seat counted anywhere else: one seat is one seat, and the
			// pool's own seat is not where a seat another agent runs belongs.
			for agent, n := range r.Live {
				if n > 1 || (agent != c.want && n != 0) {
					t.Fatalf("the seat was counted on %s as well: %+v", agent, r.Live)
				}
			}
			if c.deadHooked && r.DeadHooked[c.want] != 1 {
				t.Fatalf("the dead-hooked breakdown lost the seat: %+v", r.DeadHooked)
			}
			// The pool's own seat cap is the count of polecats on its agent,
			// and cap arithmetic is unchanged: a seat another agent runs is
			// simply not one of its seats. The town's total still sees it.
			wantOnPoolSeat := 0
			if c.want == "claude-sonnet" {
				wantOnPoolSeat = 1
			}
			if n := poolSeatCount("claude-sonnet", sessions); n != wantOnPoolSeat {
				t.Fatalf("pool seat count = %d, want %d (%+v)", n, wantOnPoolSeat, sessions)
			}
			total := 0
			for _, n := range r.Live {
				total += n
			}
			if total != 1 {
				t.Fatalf("the town's total = %d, want 1 (%+v)", total, r.Live)
			}
		})
	}
}

// testServerMetadata names a database the way a tracked .beads/metadata.json
// does in gastown's server mode.
const testServerMetadata = `{"dolt_mode":"server","dolt_database":"beads_testrig"}`

func mkdirTestDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// TestHasBeadsDatabase pins the guard's contract: a rig has a database when one
// is reachable under the resolved beads directory, not when a tracked
// metadata.json merely names one.
func TestHasBeadsDatabase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, townRoot, rigPath string)
		want  bool
	}{
		{
			name:  "empty .beads directory",
			setup: func(*testing.T, string, string) {},
			want:  false,
		},
		{
			name: "tracked config.yaml without a database",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "config.yaml"), "status.custom: []\n")
			},
			want: false,
		},
		{
			name: "embedded dolt directory",
			setup: func(t *testing.T, _, rigPath string) {
				mkdirTestDir(t, filepath.Join(rigPath, ".beads", "dolt"))
			},
			want: true,
		},
		{
			// bd's embedded data root. A .beads directory carrying it needs no
			// metadata.json, so testing metadata.json alone would read the rig
			// as empty and report no ready work.
			name: "embeddeddolt directory",
			setup: func(t *testing.T, _, rigPath string) {
				mkdirTestDir(t, filepath.Join(rigPath, ".beads", "embeddeddolt", "beads", ".dolt"))
			},
			want: true,
		},
		{
			// A same-named regular file is not a database, and accepting it
			// would put the rig back on the store-open path.
			name: "dolt as a regular file",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "dolt"), "")
			},
			want: false,
		},
		{
			name: "unparseable metadata.json",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), "{not json")
			},
			want: true,
		},
		{
			name: "metadata.json with no server database reference",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), `{"dolt_mode":"embedded"}`)
			},
			want: true,
		},
		{
			name: "server mode naming a database the server has",
			setup: func(t *testing.T, townRoot, rigPath string) {
				mkdirTestDir(t, filepath.Join(townRoot, ".dolt-data", "beads_testrig"))
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), testServerMetadata)
			},
			want: true,
		},
		{
			// The documented stale-metadata.json case: the file is tracked from
			// a workspace whose Dolt server had the database, this one does not.
			// It is an uninitialized rig, not a read failure.
			name: "server mode naming a database the server lacks",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), testServerMetadata)
			},
			want: false,
		},
		{
			name: "server mode with no town to look in",
			setup: func(t *testing.T, townRoot, rigPath string) {
				if err := os.RemoveAll(filepath.Join(townRoot, "mayor")); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), testServerMetadata)
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			mkdirTestDir(t, filepath.Join(townRoot, "mayor"))
			writeTestFile(t, filepath.Join(townRoot, "mayor", "town.json"), "{}\n")
			rigPath := filepath.Join(townRoot, "testrig")
			mkdirTestDir(t, filepath.Join(rigPath, ".beads"))
			tt.setup(t, townRoot, rigPath)

			if got := hasBeadsDatabase(filepath.Join(rigPath, ".beads")); got != tt.want {
				t.Errorf("hasBeadsDatabase() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("missing directory", func(t *testing.T) {
		if hasBeadsDatabase(filepath.Join(t.TempDir(), ".beads")) {
			t.Error("hasBeadsDatabase() = true for a directory that does not exist")
		}
	})
}

// TestPoolSeatSessionsDropsAnOverrunStoppedSeatOnceItsAgentBeadIsReset pins
// gt-2u3pv: the overrun guard hands the bead back (open, unassigned) and stops
// the session, but the polecat's agent bead kept naming it as hook_bead, so the
// roster counted the seat as dead-hooked until an operator reset it by hand.
// The stop now resets the agent bead the way a nuke does; the same seat read
// over that bead counts nothing.
func TestPoolSeatSessionsDropsAnOverrunStoppedSeatOnceItsAgentBeadIsReset(t *testing.T) {
	t.Parallel()
	pool := seatPool("deepseek-flash", 3)
	handedBack := &beads.Issue{ID: "gt-0pt32", Status: "open", Labels: []string{polecat.NeedsSplitLabel}}

	cases := []struct {
		name   string
		fields *beads.AgentFields
		want   int
	}{
		{"agent bead still names the handed-back bead", &beads.AgentFields{AgentState: "working", HookBead: "gt-0pt32"}, 1},
		{"agent bead reset by the stop", &beads.AgentFields{AgentState: string(beads.AgentStateNuked)}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "nux")
			reads := poolSeatReads{
				agentBead: func(beads.Client, string) (*beads.Issue, *beads.AgentFields, error) {
					return &beads.Issue{ID: "gt-gastown-polecat-nux"}, c.fields, nil
				},
				assigned: func(assignedIssueLookup, string, string) polecatActiveWorkEvidence {
					return polecatActiveWorkEvidence{}
				},
				show: func(_ beads.Client, id string) (*beads.Issue, error) {
					if id != handedBack.ID {
						return nil, beads.ErrNotFound
					}
					return handedBack, nil
				},
			}
			sessions, err := poolSeatSessionsWith(&fakeLister{}, town, poolDispositionAnswering(town, reads), nil, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			if n := poolSeatCount("deepseek-flash", sessions); n != c.want {
				t.Fatalf("pool seat count = %d, want %d (%+v)", n, c.want, sessions)
			}
		})
	}
}

// parkSeatAgo parks the polecat the way the overrun guard does, then backdates
// the park: Pause stamps now, and the release is a matter of how old the stamp is.
func parkSeatAgo(t *testing.T, town, rig, name string, ago time.Duration) {
	t.Helper()
	if err := agentpause.Pause(town, rig, constants.RolePolecat, name, "overrun (20 minute rule)", "overrun-guard", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	seat := intent.Seat{Rig: rig, Role: constants.RolePolecat, Name: name}
	if _, err := intent.Update(town, seat, func(r *intent.Record) error {
		r.PausedAt = time.Now().UTC().Add(-ago)
		return nil
	}); err != nil {
		t.Fatalf("intent.Update: %v", err)
	}
}

// TestPoolDeadHookedSessions_ParkedSeatReleasesAfterThirtyMinutes is gt-t7k92:
// the overrun guard parks the seat it stops and crash detection skips a parked
// seat, so nothing restarts it and it held a seat for 23 hours. A sessionless
// park older than agentpause.ParkedSeatRelease stops being dead-hooked (it is parked-stale
// and counts nowhere); a younger one still holds its seat.
func TestPoolDeadHookedSessions_ParkedSeatReleasesAfterThirtyMinutes(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 1}}}
	hooked := func(_, _ string) (poolSeatState, error) {
		return poolSeatState{
			Disposition: polecat.WorkstateDisposition{CountsTowardCapacity: true},
			HookHeld:    true,
		}, nil
	}
	cases := []struct {
		name        string
		parkedAgo   time.Duration
		wantDead    int
		wantStale   int
		wantRefused bool
	}{
		{"parked 31 minutes ago releases the seat", 31 * time.Minute, 0, 1, false},
		{"parked 29 minutes ago still holds the seat", 29 * time.Minute, 1, 0, true},
		{"a park with no stamp keeps counting", 0, 1, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "mariner")
			if c.parkedAgo > 0 {
				parkSeatAgo(t, town, "gastown", "mariner", c.parkedAgo)
			} else {
				writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "mariner"}, town,
					intent.Record{Desired: intent.DesiredPark, Reason: "hand-edited"})
			}

			sessions, err := poolSeatSessionsWith(&fakeLister{}, town, hooked, nil, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			dead, stale := 0, 0
			for _, s := range sessions {
				if s.deadHooked {
					dead++
				}
				if s.parkedStale {
					stale++
				}
			}
			if dead != c.wantDead || stale != c.wantStale {
				t.Fatalf("dead-hooked %d, parked-stale %d, want %d and %d (%+v)", dead, stale, c.wantDead, c.wantStale, sessions)
			}
			// The sessions go to the admission decision as the occupancy
			// source returns them: no call-site filter drops a stale park.
			_, reason, refused, _ := choosePoolAgent(pool, "", sessions, poolTestNow)
			if refused != c.wantRefused {
				t.Fatalf("refused = %v, want %v (%s)", refused, c.wantRefused, reason)
			}
		})
	}
}

// TestPoolDeadHookedSessions_SupervisorFreezeHoldsSeatPastThirtyMinutes: a
// seat the supervisor froze on an exhausted restart budget has no session and
// still holds its hooked bead. An hour-old freeze is not a stale park: it stays
// dead-hooked and counted, so the pool does not admit a polecat over the cap
// (gt-0rvkw).
func TestPoolDeadHookedSessions_SupervisorFreezeHoldsSeatPastThirtyMinutes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeTownPolecatDir(t, town, "gastown", "mariner")
	frozenAt := time.Now().UTC().Add(-time.Hour)
	// The record the supervisor writes when the budget runs out
	// (supervisor.go restart path), not the Frozen flag alone.
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "mariner"}, town,
		intent.Record{
			Frozen:     true,
			Restarts:   []time.Time{frozenAt, frozenAt, frozenAt},
			Actor:      "supervisor",
			UpdatedAt:  frozenAt,
			Reason:     "restart budget exhausted: 3 restarts in 1h0m0s (last: crashed)",
			PausedBy:   "supervisor",
			PausedAt:   frozenAt,
			LastAction: &intent.Action{Verb: "restart", Reason: "crashed", Actor: "supervisor", Outcome: "refused", At: frozenAt},
		})
	// Control: an operator park of the same age is released, so the freeze
	// flag, not the age or the pause stamp, is what spares the first seat.
	writeTownPolecatDir(t, town, "gastown", "flint")
	parkSeatAgo(t, town, "gastown", "flint", time.Hour)
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 1}}}
	hooked := func(_, _ string) (poolSeatState, error) {
		return poolSeatState{
			Disposition: polecat.WorkstateDisposition{CountsTowardCapacity: true},
			HookHeld:    true,
		}, nil
	}

	sessions, err := poolSeatSessionsWith(&fakeLister{}, town, hooked, nil, pool)
	if err != nil {
		t.Fatalf("poolSeatSessionsWith: %v", err)
	}
	stale := 0
	for _, s := range sessions {
		if s.parkedStale {
			stale++
		}
	}
	if stale != 1 {
		t.Fatalf("parked-stale seats = %d, want 1: only the operator park is released (%+v)", stale, sessions)
	}
	if n := poolHeldCount(sessions); n != 1 {
		t.Fatalf("poolHeldCount = %d, want 1: a frozen seat still holds its seat (%+v)", n, sessions)
	}
}

// TestPoolCounts_SkipParkedStaleSeat: the pool's own tallies skip a released
// seat, the per-agent count and the max_total one alike, so no admission path
// has to filter the sessions first (gt-t7k92).
func TestPoolCounts_SkipParkedStaleSeat(t *testing.T) {
	t.Parallel()
	total := 1
	pool := &config.PolecatPool{
		Seats:    map[string]config.SeatConfig{"deepseek-flash": {Cap: 2}},
		MaxTotal: &total,
	}
	sessions := []poolSession{{name: "dead/gastown/mariner", agent: "deepseek-flash", parkedStale: true}}
	if n := poolSeatCount("deepseek-flash", sessions); n != 0 {
		t.Errorf("poolSeatCount = %d, want 0 for a released seat", n)
	}
	if n := poolHeldCount(sessions); n != 0 {
		t.Errorf("poolHeldCount = %d, want 0 for a released seat", n)
	}
	if _, reason, refused, _ := choosePoolAgent(pool, "deepseek-flash", sessions, poolTestNow); refused {
		t.Errorf("a released seat filled max_total: %s", reason)
	}
	held := append(sessions, poolSession{name: "live/ember", agent: "deepseek-flash"})
	if _, _, refused, _ := choosePoolAgent(pool, "deepseek-flash", held, poolTestNow); !refused {
		t.Error("a live session beside a released seat did not fill max_total 1")
	}
}
