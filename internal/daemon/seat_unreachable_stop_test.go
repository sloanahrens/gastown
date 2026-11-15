package daemon

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/stretchr/testify/require"
)

// releaseSeat is the seat the release tests act on.
var releaseSeat = overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-p4aqut"}

// releaseStart is when the seat's session first failed to answer.
var releaseStart = time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)

// releaseFake is the release's seams: it records the calls it receives and the
// note and branch each hand-back carried, and fails the steps named in failOn.
type releaseFake struct {
	calls     []string // "<step> <rig>/<polecat>"
	handBacks []string // "<bead> <branch> <elapsed>"
	notes     []string // the note each hand-back carried, in call order
	failOn    map[string]error

	// submitted is what the submittedNow re-check answers; branch is what the
	// seat's worktree branch reads; slung dates the bead's dispatch, by bead.
	submitted bool
	branch    string
	slung     map[string]time.Time

	// beadMoved makes the bead re-read before the hand-back report the bead no
	// longer the seat's; beadReadErr makes it fail; beadReads counts its calls.
	beadMoved   bool
	beadReadErr error
	beadReads   int
}

func (f *releaseFake) step(name string, seat overrunSeat) error {
	f.calls = append(f.calls, name+" "+seat.Rig+"/"+seat.Polecat)
	return f.failOn[name]
}

func (f *releaseFake) seams() seatUnreachableStopSeams {
	return seatUnreachableStopSeams{
		pause:         func(seat overrunSeat) error { return f.step("pause", seat) },
		removeSandbox: func(seat overrunSeat) error { return f.step("sandbox", seat) },
		beadStillHeld: func(overrunSeat) (bool, error) {
			f.beadReads++
			if f.beadReadErr != nil {
				return false, f.beadReadErr
			}
			return !f.beadMoved, nil
		},
		handBack: func(seat overrunSeat, elapsed time.Duration, note string) error {
			if err := f.step("hand-back", seat); err != nil {
				return err
			}
			f.handBacks = append(f.handBacks, seat.Bead+" "+seat.Branch+" "+elapsed.String())
			f.notes = append(f.notes, note)
			return nil
		},
		resetAgentBead: func(seat overrunSeat) error { return f.step("reset-agent-bead", seat) },
		recordCleanup:  func(seat overrunSeat) error { return f.step("record-cleanup", seat) },
		retire:         func(seat overrunSeat) error { return f.step("retire", seat) },
		submittedNow:   func(overrunSeat) bool { return f.submitted },
		slungAt: func(seat overrunSeat) (time.Time, bool) {
			at, ok := f.slung[seat.Bead]
			return at, ok
		},
		branch: func(overrunSeat) string { return f.branch },
	}
}

// unreachableReleaseTown is a daemon with a town root and a logger, for the
// release tests that need no patrol config.
func unreachableReleaseTown(t *testing.T) (*Daemon, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
	}
	return d, logs
}

// watchUnreachable hands the seat-unreachable watch one failed read at start, as
// the polecat-health pass does, so the tracker holds the seat's first-failure
// time.
func watchUnreachable(d *Daemon, start time.Time, bead string, err error) {
	set := &seatReadSet{}
	set.add(seatRead{Rig: releaseSeat.Rig, Polecat: releaseSeat.Polecat, Bead: bead, Err: err})
	d.seatUnreachable.observe(start, set)
}

// seatRecord reads the release seat's intent record.
func seatRecord(t *testing.T, d *Daemon) intent.Record {
	t.Helper()
	rec, err := intent.Read(d.config.TownRoot, intent.Seat{Rig: releaseSeat.Rig, Role: constants.RolePolecat, Name: releaseSeat.Polecat})
	require.NoError(t, err)
	return rec
}

// seatUnreachableBeadStillHeldFor runs the production bead re-read closure for
// releaseSeat over a bead store seeded with the bead's status and assignee.
func seatUnreachableBeadStillHeldFor(t *testing.T, bd *beadsfake.Fake) (bool, error) {
	t.Helper()
	d := &Daemon{
		config:           &Config{TownRoot: t.TempDir()},
		openOverrunBeads: func() overrunHandBackStore { return bd },
	}
	return d.overrunStopSeams(overrunSeams{}).beadStillHeld(releaseSeat)
}

// TestSeatUnreachableBeadStillHeld reads the bead through the production re-read
// the release's hand-back is gated on: only a bead still hooked to this seat is
// handed back, while an open bead (already returned to the pool, or reopened), a
// bead assigned to another seat, and a closed one are not (gt-wgim24).
func TestSeatUnreachableBeadStillHeld(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		status   string
		assignee string
		want     bool
	}{
		{"hooked to the seat", "hooked", "gastown/polecats/capable", true},
		{"in progress on the seat", "in_progress", "gastown/polecats/capable", true},
		{"open again", "open", "gastown/polecats/capable", false},
		{"open and unassigned", "open", "", false},
		{"another polecat", "hooked", "gastown/polecats/other", false},
		{"bare polecat name", "hooked", "capable", false},
		{"closed", "closed", "gastown/polecats/capable", false},
		{"tombstone", "tombstone", "gastown/polecats/capable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := beadsfake.New()
			bd.Seed(beads.Issue{ID: releaseSeat.Bead, Status: tc.status, Assignee: tc.assignee})
			got, err := seatUnreachableBeadStillHeldFor(t, bd)
			require.NoError(t, err)
			require.Equal(t, tc.want, got, "bead %s held by %q", tc.status, tc.assignee)
		})
	}
}

// TestSeatUnreachableBeadStillHeldRefusesAnUnreadableBead: a bead the read
// cannot answer is not the seat's, and the error reaches the release, which
// stops before the hand-back (gt-wgim24).
func TestSeatUnreachableBeadStillHeldRefusesAnUnreadableBead(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	bd.Seed(beads.Issue{ID: releaseSeat.Bead, Status: "hooked", Assignee: "gastown/polecats/capable"})
	bd.FailWith("show", errors.New("dolt timeout"))
	got, err := seatUnreachableBeadStillHeldFor(t, bd)
	require.Error(t, err)
	require.False(t, got, "an unreadable bead must not be handed back")
	require.Contains(t, err.Error(), releaseSeat.Bead)
}

// TestSeatUnreachableReleaseAtFifteenMinutes drives the overrun guard's tick,
// the path's only caller, over a seat whose session has answered nothing: one
// second short of fifteen minutes the seat is left alone, and at fifteen its
// bead is released through the overrun stop's own release steps with a note
// naming the seat, the minutes and the error (gt-wgim24).
func TestSeatUnreachableReleaseAtFifteenMinutes(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod gastown-capable is gone")
	stops := []string{
		"hand-back gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}
	steps := []struct {
		name string
		age  time.Duration
		want []string
	}{
		{"fourteen minutes leaves the seat alone", 14 * time.Minute, nil},
		{"one second short leaves the seat alone", 15*time.Minute - time.Second, nil},
		{"fifteen minutes releases the bead", 15 * time.Minute, stops},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			f := newOverrunFake(0)
			f.now = releaseStart.Add(step.age)
			watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)

			d.runOverrunGuard(f.seams())

			require.Equal(t, step.want, f.calls)
			if step.want == nil {
				require.Empty(t, f.handBackNotes, "a seat under fifteen minutes unreadable must keep its bead")
				return
			}
			require.Equal(t, []string{"seat gastown/capable unreachable for 15m: " + gone.Error()}, f.handBackNotes,
				"the note must name the seat, the minutes and the error")
			rec := seatRecord(t, d)
			require.Equal(t, intent.DesiredPark, rec.Desired, "the seat must be parked before it is released")
			require.Equal(t, seatUnreachableActor, rec.PausedBy, "the park must be the release's own")
		})
	}
}

// TestSeatUnreachableReleaseLeavesASeatThatAnswers drives the real
// polecat-health pass and the guard's tick together: a seat unreadable at its
// first check and answering again at minute ten has no outage by the time the
// guard asks twenty minutes later, so nothing is released (gt-wgim24).
func TestSeatUnreachableReleaseLeavesASeatThatAnswers(t *testing.T) {
	t.Parallel()
	d, tm, clk := unreachableSeatTown(t, hookedWorkBD(t, "gt-p4aqut", time.Hour))
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{Enabled: true}}}
	tm.hasErr = errors.New("sandbox pod myr-mycat is gone")

	d.checkPolecatSessionHealth() // the failed read starts the outage
	clk.Advance(10 * time.Minute)
	tm.hasErr = nil
	d.checkPolecatSessionHealth() // the seat answers; the outage ends

	// The guard asks twenty minutes after the answer: longer than the release
	// waits, so only the answer ending the outage can keep the bead held.
	f := newOverrunFake(0)
	f.now = clk.Now().Add(20 * time.Minute)
	d.runOverrunGuard(f.seams())

	require.Empty(t, f.calls, "a seat that answered at minute ten must not be released")
}

// TestSeatUnreachableReleaseLeavesAHeldSeatAlone: the tracker carries an outage
// through the passes that skip a held seat, so a seat an operator parks after
// its outage began still reads as unreachable; its bead must not be released
// (gt-wgim24).
func TestSeatUnreachableReleaseLeavesAHeldSeatAlone(t *testing.T) {
	t.Parallel()
	d, _ := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))
	require.NoError(t, agentpause.Pause(d.config.TownRoot, releaseSeat.Rig, constants.RolePolecat, releaseSeat.Polecat,
		"operator park", "operator", ""))

	f := &releaseFake{}
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

	require.Empty(t, f.calls, "a seat an operator holds must keep its bead")
}

// TestSeatUnreachableReleaseResumesItsOwnPause: a release that stopped after its
// pause left the seat parked by this path itself; refusing that park would hold
// the bead for good, so the next tick finishes the release (gt-wgim24).
func TestSeatUnreachableReleaseResumesItsOwnPause(t *testing.T) {
	t.Parallel()
	d, _ := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))
	require.NoError(t, agentpause.Pause(d.config.TownRoot, releaseSeat.Rig, constants.RolePolecat, releaseSeat.Polecat,
		seatUnreachablePauseReason, seatUnreachableActor, ""))

	f := &releaseFake{}
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

	require.Equal(t, []string{
		"pause gastown/capable",
		"sandbox gastown/capable",
		"hand-back gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}, f.calls, "the park this path wrote is not a hold")
}

// TestSeatUnreachableReleaseLeavesASubmittedSeatAlone: a seat whose work is with
// the landing worker answers nothing, but its bead is not the daemon's to hand
// back (gt-wgim24).
func TestSeatUnreachableReleaseLeavesASubmittedSeatAlone(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod gastown-capable is gone")
	steps := []struct {
		name         string
		record       bool
		submittedNow bool
	}{
		{"a record that says submitted", true, false},
		{"a submission read fresh at the release", false, true},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, _ := unreachableReleaseTown(t)
			watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)
			if step.record {
				require.NoError(t, intent.MarkSubmitted(d.config.TownRoot,
					intent.Seat{Rig: releaseSeat.Rig, Role: constants.RolePolecat, Name: releaseSeat.Polecat},
					releaseSeat.Bead, "gt done", releaseStart))
			}
			f := &releaseFake{submitted: step.submittedNow}

			d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

			require.Empty(t, f.calls, "a submitted seat's bead belongs to the landing worker")
		})
	}
}

// TestSeatUnreachableReleaseLeavesASeatWithNoBeadAlone: the pass can name a
// seat whose bead read failed, and there is nothing to hand back; the seat is
// left alone rather than released on a guess (gt-wgim24).
func TestSeatUnreachableReleaseLeavesASeatWithNoBeadAlone(t *testing.T) {
	t.Parallel()
	d, _ := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, "", errors.New("dolt: connection refused"))

	f := &releaseFake{}
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

	require.Empty(t, f.calls, "a seat with no bead read must not be released")
}

// TestSeatUnreachableReleaseLeavesAnUnreadableRecordAlone: intent records fail
// closed, so a record that cannot be read is a seat that may be held and its
// bead is not released (gt-wgim24).
func TestSeatUnreachableReleaseLeavesAnUnreadableRecordAlone(t *testing.T) {
	t.Parallel()
	d, logs := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))
	path := intent.Seat{Rig: releaseSeat.Rig, Role: constants.RolePolecat, Name: releaseSeat.Polecat}.Path(d.config.TownRoot)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	f := &releaseFake{}
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

	require.Empty(t, f.calls, "an unreadable record is a hold, not a release")
	require.Contains(t, logs.String(), "reading its intent record", "the unanswered read must be logged")
}

// TestSeatUnreachableReleaseDryRunLogsOnly: a dry run says what it would
// release and releases nothing (gt-wgim24).
func TestSeatUnreachableReleaseDryRunLogsOnly(t *testing.T) {
	t.Parallel()
	d, logs := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))

	f := &releaseFake{}
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), true)

	require.Empty(t, f.calls, "a dry run must not release anything")
	require.Contains(t, logs.String(), "would release gt-p4aqut held by gastown/capable")
}

// TestSeatUnreachableReleaseRetriesAFailedHandBack: a release that fails is left
// to the next tick, where the tracker still holds the outage and the release
// runs again from the pause (gt-wgim24).
func TestSeatUnreachableReleaseRetriesAFailedHandBack(t *testing.T) {
	t.Parallel()
	d, _ := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))
	f := &releaseFake{failOn: map[string]error{"hand-back": errors.New("bd: connection refused")}}

	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)
	require.Equal(t, []string{"pause gastown/capable", "sandbox gastown/capable", "hand-back gastown/capable"}, f.calls,
		"a failed hand-back must stop the release there")

	f.failOn = nil
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(17*time.Minute), false)
	require.Equal(t, []string{
		"pause gastown/capable",
		"sandbox gastown/capable",
		"hand-back gastown/capable",
		"pause gastown/capable",
		"sandbox gastown/capable",
		"hand-back gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}, f.calls, "the next tick must finish the release")
}

// TestSeatUnreachableReleaseStopsAtAFailedStep runs the release's steps in the
// overrun stop's order: a failed pause, hand-back, agent-bead reset or retire
// stops the release where it failed, so the next tick repeats the hand-back and
// the release, while a cleanup status that cannot be recorded holds nothing back
// (gt-wgim24).
func TestSeatUnreachableReleaseStopsAtAFailedStep(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod gastown-capable is gone")
	steps := []struct {
		name string
		fail string
		want []string
	}{
		{"a pause that fails releases nothing", "pause", []string{
			"pause gastown/capable",
		}},
		{"a sandbox that cannot be removed keeps the bead for the next tick", "sandbox", []string{
			"pause gastown/capable",
			"sandbox gastown/capable",
		}},
		{"a hand-back that fails keeps the rest for the next tick", "hand-back", []string{
			"pause gastown/capable",
			"sandbox gastown/capable",
			"hand-back gastown/capable",
		}},
		{"an agent bead that cannot be reset stops the release", "reset-agent-bead", []string{
			"pause gastown/capable",
			"sandbox gastown/capable",
			"hand-back gastown/capable",
			"reset-agent-bead gastown/capable",
		}},
		{"a cleanup status that cannot be recorded holds nothing back", "record-cleanup", []string{
			"pause gastown/capable",
			"sandbox gastown/capable",
			"hand-back gastown/capable",
			"reset-agent-bead gastown/capable",
			"record-cleanup gastown/capable",
			"retire gastown/capable",
		}},
		{"a seat that cannot be retired leaves the release to the next tick", "retire", []string{
			"pause gastown/capable",
			"sandbox gastown/capable",
			"hand-back gastown/capable",
			"reset-agent-bead gastown/capable",
			"record-cleanup gastown/capable",
			"retire gastown/capable",
		}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, logs := unreachableReleaseTown(t)
			watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)
			f := &releaseFake{failOn: map[string]error{step.fail: errors.New("refused")}}

			d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

			require.Equal(t, step.want, f.calls)
			if step.fail == "record-cleanup" {
				require.Contains(t, logs.String(), "cleanup status not recorded for the released seat gt-p4aqut")
			} else {
				require.Contains(t, logs.String(), "seat_unreachable: gastown/capable: ", "a failed step must be logged")
			}
		})
	}
}

// TestSeatUnreachableReleaseNamesTheKeptBranchAndRun: the hand-back comment
// carries the seat's worktree branch and the run's own length, not the age of
// the outage, when the bead's dispatch is known (gt-wgim24).
func TestSeatUnreachableReleaseNamesTheKeptBranchAndRun(t *testing.T) {
	t.Parallel()
	d, _ := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))
	f := &releaseFake{
		branch: "polecat/capable/gt-p4aqut+x1",
		slung:  map[string]time.Time{releaseSeat.Bead: releaseStart.Add(-time.Hour)},
	}

	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

	require.Equal(t, []string{"gt-p4aqut polecat/capable/gt-p4aqut+x1 1h16m0s"}, f.handBacks,
		"the comment must name the branch and the run's own length")
	require.Equal(t, []string{"seat gastown/capable unreachable for 16m: sandbox pod gastown-capable is gone"}, f.notes)
}

// TestSeatUnreachableReleaseDeletesTheGatewaySandbox drives the overrun guard's
// tick over a sandboxed seat unreadable for fifteen minutes: the release must
// delete the gateway sandbox object the seat ran in, through the stop's own
// removal and before the bead is handed back, or the next sling to the name
// fails on the stale object (gt-wgim24, operator decision 2026-11-14).
func TestSeatUnreachableReleaseDeletesTheGatewaySandbox(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(0)
	f.now = releaseStart.Add(15 * time.Minute)
	f.removeSandbox = func(overrunSeat) error { return nil }
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))

	d.runOverrunGuard(f.seams())

	require.Equal(t, []string{
		"sandbox gastown/capable",
		"hand-back gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}, f.calls, "the release must delete the sandbox, before it hands the bead back")
}

// TestSeatUnreachableReleaseRunsWhenTheSeatListFails: the unreachable seats come
// from the tracker, not from the working-seat list, so a tick whose list read
// fails must still release a seat due for it and log why the rest of the tick
// stopped (gt-wgim24).
func TestSeatUnreachableReleaseRunsWhenTheSeatListFails(t *testing.T) {
	t.Parallel()
	d, logs := newOverrunDaemon(t, false)
	f := newOverrunFake(0)
	f.now = releaseStart.Add(15 * time.Minute)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))
	seams := f.seams()
	seams.workingSeats = func() ([]overrunSeat, error) { return nil, errors.New("dolt: connection refused") }

	d.runOverrunGuard(seams)

	require.Equal(t, []string{
		"hand-back gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}, f.calls, "the list read failing must not keep a due seat from being released")
	require.Contains(t, logs.String(), "cannot list working seats", "the failed list read must still be logged")
}

// TestSeatUnreachableReleaseNoteIsOneBoundedLine: the note names the error its
// last read returned, and that error can carry the output of the command that
// failed, so the note gets one line of it and never the whole capture
// (gt-wgim24).
func TestSeatUnreachableReleaseNoteIsOneBoundedLine(t *testing.T) {
	t.Parallel()
	gone := errors.New("openshell sandbox get: exit status 1\ngateway: dial tcp 10.0.0.5:443\n\tconnection refused")
	d, _ := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)

	f := &releaseFake{}
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

	require.Equal(t, []string{
		"seat gastown/capable unreachable for 16m: openshell sandbox get: exit status 1 gateway: dial tcp 10.0.0.5:443 connection refused",
	}, f.notes, "the note must be one line, so a multi-line capture cannot reach the bead")
}

// TestSeatUnreachableReleaseHappensOncePerOutage: a release that ends marks the
// seat's outage, so the next tick hands nothing back while the seat stays
// unreadable and parked; a good read drops the outage and the mark with it, so a
// later outage is released afresh (gt-wgim24).
func TestSeatUnreachableReleaseHappensOncePerOutage(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod gastown-capable is gone")
	released := []string{
		"pause gastown/capable",
		"sandbox gastown/capable",
		"hand-back gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}
	releasedTwice := append(append([]string{}, released...), released...)
	steps := []struct {
		name string
		// goodReadThenFail drops the outage with an answer and starts a new
		// outage twenty minutes in; false leaves the first outage running.
		goodReadThenFail bool
		want             []string
	}{
		{"a second tick of the same outage hands nothing back", false, released},
		{"a new outage after a good read is released again", true, releasedTwice},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, _ := unreachableReleaseTown(t)
			watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)
			f := &releaseFake{}

			d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)
			require.Equal(t, released, f.calls, "the first due tick must release the seat")

			if step.goodReadThenFail {
				good := &seatReadSet{}
				good.add(seatRead{Rig: releaseSeat.Rig, Polecat: releaseSeat.Polecat, Bead: releaseSeat.Bead})
				d.seatUnreachable.observe(releaseStart.Add(17*time.Minute), good)
				watchUnreachable(d, releaseStart.Add(20*time.Minute), releaseSeat.Bead, gone)
				d.releaseUnreachableSeats(f.seams(), releaseStart.Add(36*time.Minute), false)
			} else {
				// The release parked the seat, so the next health pass skips it
				// and the tracker carries the outage through that skip.
				held := &seatReadSet{}
				held.skip(releaseSeat.Rig, releaseSeat.Polecat)
				d.seatUnreachable.observe(releaseStart.Add(17*time.Minute), held)
				d.releaseUnreachableSeats(f.seams(), releaseStart.Add(18*time.Minute), false)
			}

			require.Equal(t, step.want, f.calls, "only a new outage may be released again")
			require.Len(t, f.handBacks, len(step.want)/len(released), "each release hands the bead back once")
		})
	}
}

// TestSeatUnreachableReleaseMarkDoesNotOutliveItsOutage: the release's mark is
// keyed by the outage's first-failure time, so an outage that ends and a new one
// that starts while the release runs is not silenced by the finished one's mark
// (gt-wgim24).
func TestSeatUnreachableReleaseMarkDoesNotOutliveItsOutage(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod gastown-capable is gone")
	d, _ := unreachableReleaseTown(t)
	watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)
	f := &releaseFake{}
	seams := f.seams()
	newOutageAt := releaseStart.Add(16 * time.Minute)
	seams.handBack = func(seat overrunSeat, _ time.Duration, _ string) error {
		f.calls = append(f.calls, "hand-back "+seat.Rig+"/"+seat.Polecat)
		// The health pass answers for the seat while the release still runs: the
		// old outage ends and a new one starts.
		good := &seatReadSet{}
		good.add(seatRead{Rig: seat.Rig, Polecat: seat.Polecat, Bead: seat.Bead})
		d.seatUnreachable.observe(newOutageAt, good)
		failed := &seatReadSet{}
		failed.add(seatRead{Rig: seat.Rig, Polecat: seat.Polecat, Bead: seat.Bead, Err: gone})
		d.seatUnreachable.observe(newOutageAt, failed)
		return nil
	}

	d.releaseUnreachableSeats(seams, newOutageAt, false)
	d.releaseUnreachableSeats(seams, newOutageAt.Add(15*time.Minute), false)

	handBacks := 0
	for _, call := range f.calls {
		if call == "hand-back gastown/capable" {
			handBacks++
		}
	}
	require.Equal(t, 2, handBacks,
		"the new outage must be released even though the old one's release ended after it began")
}

// TestSeatUnreachableReleaseEndsAtABeadThatMoved: before it touches the seat the
// release re-reads the bead, and a bead that has gone back to the pool, been
// reopened or moved to another seat is not this seat's to hand back again: the
// release ends as done with one log line and nothing done to the seat. A read
// that fails stops the release for the next tick (gt-wgim24).
func TestSeatUnreachableReleaseEndsAtABeadThatMoved(t *testing.T) {
	t.Parallel()
	steps := []struct {
		name    string
		moved   bool
		readErr error
		wantLog string
	}{
		{"a bead re-hooked to another seat is not handed back", true, nil,
			"gt-p4aqut is no longer hooked to it"},
		{"a bead read that fails stops the release for the next tick", false, errors.New("bd: connection refused"),
			"re-reading gt-p4aqut before releasing the seat"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, logs := unreachableReleaseTown(t)
			watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))
			f := &releaseFake{beadMoved: step.moved, beadReadErr: step.readErr}

			d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

			require.Empty(t, f.calls, "a bead that is not the seat's must not release the seat")
			require.Equal(t, 1, f.beadReads, "the bead must be re-read once before the release")
			require.Empty(t, f.handBacks, "nothing may be handed back")
			require.Contains(t, logs.String(), step.wantLog)
			if !step.moved {
				return
			}
			d.releaseUnreachableSeats(f.seams(), releaseStart.Add(17*time.Minute), false)
			require.Empty(t, f.calls, "a moved bead ends the release for this outage")
			require.Equal(t, 1, f.beadReads, "a moved bead is not re-read on the next tick")
		})
	}
}

// TestSeatUnreachableReleaseRefusesHalfWiredSeams: a release that cannot pause
// the seat, read whose bead it would hand back, or hand the bead back is no
// release, so the missing seam refuses it and acts on nothing rather than
// running the steps it has (gt-wgim24).
func TestSeatUnreachableReleaseRefusesHalfWiredSeams(t *testing.T) {
	t.Parallel()
	steps := []struct {
		name   string
		unwire func(*seatUnreachableStopSeams)
	}{
		{"no pause seam", func(s *seatUnreachableStopSeams) { s.pause = nil }},
		{"no bead read seam", func(s *seatUnreachableStopSeams) { s.beadStillHeld = nil }},
		{"no hand-back seam", func(s *seatUnreachableStopSeams) { s.handBack = nil }},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, logs := unreachableReleaseTown(t)
			watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))
			f := &releaseFake{}
			seams := f.seams()
			step.unwire(&seams)

			d.releaseUnreachableSeats(seams, releaseStart.Add(16*time.Minute), false)

			require.Empty(t, f.calls, "a half-wired release must act on nothing")
			require.Contains(t, logs.String(), "release seams incomplete", "the missing seam must be logged")
		})
	}
}
