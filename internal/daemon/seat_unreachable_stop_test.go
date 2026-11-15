package daemon

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/stretchr/testify/require"
)

// releaseSeat is the seat the release tests act on.
var releaseSeat = overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-p4aqut"}

// releaseBranch is the branch releaseSeat's sandbox runs, as its record names it.
const releaseBranch = "polecat/capable/gt-p4aqut+x1"

// releaseSession is the sandbox record name a town with the "gt" prefix gives
// releaseSeat.
const releaseSession = "gt-capable"

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

	// originAbsent makes the origin read answer that origin holds no branch of
	// the seat's (absent is held), originBranch is the branch the read names, and
	// originErr fails the read.
	originAbsent bool
	originBranch string
	originErr    error

	// sandboxAhead makes the sandbox proof answer that the clone holds commits
	// origin's tip does not, and sandboxProofErr makes it fail.
	sandboxAhead    bool
	sandboxProofErr error

	// deleted is the session of each record the sandbox release handed to the
	// deleter, deleteErr fails that delete, and nilDeleter stands in for a
	// binary with no record deleter linked.
	deleted    []string
	deleteErr  error
	nilDeleter bool

	// running is the production step running right now, so a fake read can
	// answer for the step that asks it rather than for a count of calls.
	running string
}

// inStep names the production step the release is running, "" when none is.
func (f *releaseFake) inStep(name string) { f.running = name }

// deleter is the record deleter the production sandbox release runs: the fake
// gateway's delete, which records the record it was given and answers deleteErr.
func (f *releaseFake) deleter(_ string, rec session.SandboxRecord) error {
	f.deleted = append(f.deleted, rec.Session)
	return f.deleteErr
}

func (f *releaseFake) step(name string, seat overrunSeat) error {
	f.calls = append(f.calls, name+" "+seat.Rig+"/"+seat.Polecat)
	return f.failOn[name]
}

func (f *releaseFake) seams() seatUnreachableStopSeams {
	return seatUnreachableStopSeams{
		pause: func(seat overrunSeat) error { return f.step("pause", seat) },
		originHolds: func(seat overrunSeat) (string, bool, error) {
			if err := f.step("origin", seat); err != nil {
				return "", false, err
			}
			branch := f.originBranch
			if branch == "" {
				branch = releaseBranch
			}
			if f.originErr != nil {
				return branch, false, f.originErr
			}
			return branch, !f.originAbsent, nil
		},
		sandboxOnOrigin: func(overrunSeat, string) (bool, error) {
			return !f.sandboxAhead, f.sandboxProofErr
		},
		releaseSandbox: func(seat overrunSeat) error { return f.step("sandbox-release", seat) },
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

// unreachableReleaseTown is a daemon with a town root, a logger and a recording
// notifier, so a release that raises an item records it instead of running gt.
func unreachableReleaseTown(t *testing.T) (*Daemon, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(logs, "", 0),
		notifier: notifyfake.New(),
	}
	return d, logs
}

// raisedItems returns the items a release on d raised.
func raisedItems(d *Daemon) []notifyfake.Call {
	return d.notifier.(*notifyfake.Recorder).Escalations()
}

// releaseSandboxRecord is the record of releaseSeat's sandbox: the session name
// the "gt" prefix gives it, and the branch its clone ran.
func releaseSandboxRecord() session.SandboxRecord {
	return session.SandboxRecord{
		Session: releaseSession,
		Rig:     releaseSeat.Rig,
		Polecat: releaseSeat.Polecat,
		Branch:  releaseBranch,
	}
}

// seedReleaseSandbox gives releaseSeat a sandbox: the town's registry gives the
// rig the "gt" prefix the session name is built from, the record names the branch
// its clone ran, and the seat's host worktree is the stub the origin read resolves.
func seedReleaseSandbox(t *testing.T, d *Daemon) session.SandboxRecord {
	t.Helper()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", releaseSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	rec := releaseSandboxRecord()
	require.NoError(t, session.WriteSandboxRecord(d.config.TownRoot, rec))
	seedHostWorktreeStub(t, d)
	return rec
}

// seedReleaseBranchOnOrigin gives releaseSeat a host worktree whose branch is
// releaseBranch, and an origin that holds it, so a release driven through the
// production seams asks origin and finds the seat's work there. It is for the
// tests that go through the guard's own wiring, where the branch comes from the
// worktree rather than from a fake.
func seedReleaseBranchOnOrigin(t *testing.T, d *Daemon) {
	t.Helper()
	seedHostWorktreeStub(t, d)
	d.openGitFn = func(string) daemonGit {
		return &pushStubGit{branch: releaseBranch,
			originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}}}
	}
}

// seedReleaseRecords writes sandbox records for a rig the town's registry does
// not list, so the seat's sandbox is found by the rig and polecat the records
// name, as the stop's lookup does for an unlisted rig.
func seedReleaseRecords(t *testing.T, d *Daemon, recs ...session.SandboxRecord) {
	t.Helper()
	for _, rec := range recs {
		require.NoError(t, session.WriteSandboxRecord(d.config.TownRoot, rec))
	}
}

// productionSandboxSeams is the release's seams with the origin read and the
// sandbox release left at the production pair, recording both in fake's calls as
// the fake's own seams do. The sandbox release's delete goes to the fake's
// deleter, so no gateway is asked. Each wrapper names the step it is in, so a
// fake read can fail for the step that asks rather than for the position of its
// call in the run.
func productionSandboxSeams(d *Daemon, f *releaseFake) seatUnreachableStopSeams {
	seams := f.seams()
	seams.originHolds = func(seat overrunSeat) (string, bool, error) {
		f.calls = append(f.calls, "origin "+seat.Rig+"/"+seat.Polecat)
		f.inStep("origin")
		defer f.inStep("")
		return d.seatUnreachableOriginHolds(seat)
	}
	seams.releaseSandbox = func(seat overrunSeat) error {
		f.calls = append(f.calls, "sandbox-release "+seat.Rig+"/"+seat.Polecat)
		f.inStep("sandbox-release")
		defer f.inStep("")
		var deleter func(string, session.SandboxRecord) error
		if !f.nilDeleter {
			deleter = f.deleter
		}
		return d.seatUnreachableReleaseSandbox(seat, deleter)
	}
	return seams
}

// releaseProductionSandbox runs the release over the production origin read and
// sandbox release, with the other steps of fake.
func releaseProductionSandbox(t *testing.T, d *Daemon, f *releaseFake) []string {
	t.Helper()
	d.releaseUnreachableSeats(productionSandboxSeams(d, f), releaseStart.Add(16*time.Minute), false)
	return f.calls
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
			seedReleaseBranchOnOrigin(t, d)
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
		"origin gastown/capable",
		"sandbox-release gastown/capable",
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
	require.Equal(t, []string{
		"pause gastown/capable",
		"origin gastown/capable",
		"sandbox-release gastown/capable",
		"hand-back gastown/capable",
	}, f.calls, "a failed hand-back must stop the release there")

	f.failOn = nil
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(17*time.Minute), false)
	require.Equal(t, []string{
		"pause gastown/capable",
		"origin gastown/capable",
		"sandbox-release gastown/capable",
		"hand-back gastown/capable",
		"pause gastown/capable",
		"origin gastown/capable",
		"sandbox-release gastown/capable",
		"hand-back gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}, f.calls, "the next tick must finish the release")
}

// TestSeatUnreachableReleaseRetriesOnTheBranchItRemembered: the release reads the
// seat's branch once and keeps it on the outage. The sandbox release drops the
// record a branch-less seat names its branch with, so a release that finished
// that step and then failed at a later one must still name the branch on the next
// tick, through the production origin read: the seat's worktree answers no branch
// any more and the record is gone, so a release that forgot it would have nothing
// to ask origin about and stay stuck on every tick after (gt-35ihg5).
func TestSeatUnreachableReleaseRetriesOnTheBranchItRemembered(t *testing.T) {
	t.Parallel()
	d, logs := unreachableReleaseTown(t)
	seedReleaseSandbox(t, d)
	origin := &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}}}
	d.openGitFn = func(string) daemonGit { return origin }
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))

	// Tick 1: the seat's worktree answers its branch, the sandbox and its record
	// go, and the hand-back fails.
	f := &releaseFake{branch: releaseBranch, failOn: map[string]error{"hand-back": errors.New("bd: connection refused")}}
	d.releaseUnreachableSeats(productionSandboxSeams(d, f), releaseStart.Add(16*time.Minute), false)
	require.Equal(t, []string{releaseSession}, f.deleted,
		"the sandbox must be deleted on the branch the seat named")
	require.NoFileExists(t, session.SandboxRegistryDir(d.config.TownRoot)+"/"+releaseSession+".json",
		"the record must have gone with its sandbox")
	require.Len(t, f.handBacks, 0, "the failed hand-back must hand nothing back")
	raisedOnFirstTick := len(raisedItems(d))
	require.Equal(t, 1, raisedOnFirstTick, "the failed release must raise its item")

	// The release parked the seat, so the health pass skips it and the tracker
	// carries the outage — the branch with it — to the next tick.
	held := &seatReadSet{}
	held.skip(releaseSeat.Rig, releaseSeat.Polecat)
	d.seatUnreachable.observe(releaseStart.Add(17*time.Minute), held)

	// Tick 2: the seat's worktree answers no branch any more, and no record is
	// left to name one.
	f.branch = ""
	f.failOn = nil
	d.releaseUnreachableSeats(productionSandboxSeams(d, f), releaseStart.Add(18*time.Minute), false)

	require.Equal(t, []string{releaseSeat.Bead + " " + releaseBranch + " 18m0s"}, f.handBacks,
		"the retried release must name the branch it remembered")
	require.Equal(t, []string{"retire gastown/capable"}, f.calls[len(f.calls)-1:],
		"the retried release must finish")
	require.NotContains(t, logs.String(), "names no branch",
		"the retry must not stay stuck on a branch it forgot")
	require.Len(t, raisedItems(d), raisedOnFirstTick, "a release that finishes raises nothing new")
}

// TestSeatUnreachableReleaseStopsAtAFailedStep runs the release's steps in the
// overrun stop's order: a failed pause, origin read, record clear, hand-back,
// agent-bead reset or retire stops the release where it failed, so the next tick
// repeats it, while a cleanup status that cannot be recorded holds nothing back
// (gt-wgim24, gt-35ihg5).
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
		{"an origin read that fails keeps the record and the bead for the next tick", "origin", []string{
			"pause gastown/capable",
			"origin gastown/capable",
		}},
		{"a sandbox that cannot be released keeps the bead for the next tick", "sandbox-release", []string{
			"pause gastown/capable",
			"origin gastown/capable",
			"sandbox-release gastown/capable",
		}},
		{"a hand-back that fails keeps the rest for the next tick", "hand-back", []string{
			"pause gastown/capable",
			"origin gastown/capable",
			"sandbox-release gastown/capable",
			"hand-back gastown/capable",
		}},
		{"an agent bead that cannot be reset stops the release", "reset-agent-bead", []string{
			"pause gastown/capable",
			"origin gastown/capable",
			"sandbox-release gastown/capable",
			"hand-back gastown/capable",
			"reset-agent-bead gastown/capable",
		}},
		{"a cleanup status that cannot be recorded holds nothing back", "record-cleanup", []string{
			"pause gastown/capable",
			"origin gastown/capable",
			"sandbox-release gastown/capable",
			"hand-back gastown/capable",
			"reset-agent-bead gastown/capable",
			"record-cleanup gastown/capable",
			"retire gastown/capable",
		}},
		{"a seat that cannot be retired leaves the release to the next tick", "retire", []string{
			"pause gastown/capable",
			"origin gastown/capable",
			"sandbox-release gastown/capable",
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

// TestSeatUnreachableReleaseReleasesTheSandboxOfAHeldBranch drives the release
// over a sandboxed seat unreadable for fifteen minutes whose branch origin
// already holds, through the production origin read and sandbox release: the
// gateway's sandbox goes through the record deleter, its record with it, the
// seat's end line is written for the ledger, and the branch is left untouched —
// the release writes nothing to origin, so the only thing it asked was what
// origin holds (gt-35ihg5).
func TestSeatUnreachableReleaseReleasesTheSandboxOfAHeldBranch(t *testing.T) {
	t.Parallel()
	d, logs := unreachableReleaseTown(t)
	seedReleaseSandbox(t, d)
	origin := &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}}}
	d.openGitFn = func(string) daemonGit { return origin }
	gone := errors.New("sandbox pod gastown-capable is gone")
	watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)

	f := &releaseFake{}
	releaseProductionSandbox(t, d, f)

	require.Equal(t, []string{releaseSession}, f.deleted,
		"the sandbox the seat ran in must be deleted through the record deleter")
	if _, ok, err := session.ReadSandboxRecord(d.config.TownRoot, releaseSession); err != nil || ok {
		require.Failf(t, "the seat's sandbox record survived the release", "ok=%v err=%v", ok, err)
	}
	ledger, err := os.ReadFile(session.SandboxSeatLedgerPath(d.config.TownRoot))
	require.NoError(t, err, "the seat-ended writer must write the seat's end line")
	require.Contains(t, string(ledger), `"session":"`+releaseSession+`"`, "the end line must name the seat's record")
	require.Empty(t, origin.pushes, "a branch origin holds must be left untouched")
	require.NotContains(t, logs.String(), "origin holds no branch", "a branch origin holds is no alarm")
	require.Len(t, f.handBacks, 1, "the bead must still be handed back once")
	require.Empty(t, raisedItems(d), "a branch origin holds raises nothing")
}

// TestSeatUnreachableReleaseKeepsTheSandboxOfAGoneBranch: origin answered and
// holds no such branch, so the seat's commits may be nowhere. The release keeps
// the sandbox and its record — they may be the only copy of the work — and hands
// the bead back once, while it warns with the seat and the branch and raises the
// item for a person who can look for the work (gt-mp9zrg, gt-35ihg5).
func TestSeatUnreachableReleaseKeepsTheSandboxOfAGoneBranch(t *testing.T) {
	t.Parallel()
	d, logs := unreachableReleaseTown(t)
	seedReleaseSandbox(t, d)
	origin := &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/main", Hash: "bbb222"}}}
	d.openGitFn = func(string) daemonGit { return origin }
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))

	f := &releaseFake{}
	releaseProductionSandbox(t, d, f)

	require.Empty(t, f.deleted, "a sandbox origin cannot vouch for must not be deleted")
	require.FileExists(t, session.SandboxRegistryDir(d.config.TownRoot)+"/"+releaseSession+".json",
		"the record must stay with the sandbox it names")
	require.Len(t, f.handBacks, 1, "a branch origin does not hold still hands the bead back once")
	log := logs.String()
	require.Contains(t, log, "WARNING")
	require.Contains(t, log, "origin holds no branch "+releaseBranch)
	require.Contains(t, log, releaseSeat.Rig+"/"+releaseSeat.Polecat)
	raised := raisedItems(d)
	require.Len(t, raised, 1, "the absence of the branch must be raised once")
	require.Contains(t, raised[0].Escalation.Description, "origin holds no branch "+releaseBranch)
	require.Equal(t, "MEDIUM", raised[0].Escalation.Severity)
	require.Equal(t, "seat-unreachable-sandbox-gone:gastown/capable:gt-p4aqut", raised[0].Escalation.Fingerprint)
}

// TestSeatUnreachableReleaseAsksOriginWithoutASandboxRecord: a seat whose
// sandbox record is already gone — an earlier tick dropped it, or the seat never
// ran in a sandbox — is not a seat with nothing to ask origin about. Origin is
// asked for the seat's own branch, and its answer decides: a branch origin holds
// releases nothing and raises nothing, and one it does not hold warns, raises the
// sandbox-gone item and keeps the bead coming back. A seat that names no branch
// either is not answered with "held": the release stays owed and raises the
// failure item (gt-35ihg5).
func TestSeatUnreachableReleaseAsksOriginWithoutASandboxRecord(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod gastown-capable is gone")
	steps := []struct {
		name       string
		originRefs []git.RemoteRef
		branch     string
		// unlisted leaves the rig out of the registry, so the seat's sandbox is
		// looked for by rig and polecat and names no session at all.
		unlisted  bool
		wantCalls []string
		wantItem  string
		wantBack  int
	}{
		{
			name:       "origin holds the seat's branch",
			originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}},
			branch:     releaseBranch,
			wantCalls: []string{
				"pause gastown/capable",
				"origin gastown/capable",
				"sandbox-release gastown/capable",
				"hand-back gastown/capable",
				"reset-agent-bead gastown/capable",
				"record-cleanup gastown/capable",
				"retire gastown/capable",
			},
			wantBack: 1,
		},
		{
			name:       "origin holds no branch of the seat's",
			originRefs: []git.RemoteRef{{Name: "refs/heads/main", Hash: "bbb222"}},
			branch:     releaseBranch,
			wantCalls: []string{
				"pause gastown/capable",
				"origin gastown/capable",
				"hand-back gastown/capable",
				"reset-agent-bead gastown/capable",
				"record-cleanup gastown/capable",
				"retire gastown/capable",
			},
			wantItem: "seat-unreachable-sandbox-gone:gastown/capable:gt-p4aqut",
			wantBack: 1,
		},
		{
			name:       "a rig with no prefix and no record asks origin too",
			originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}},
			branch:     releaseBranch,
			unlisted:   true,
			wantCalls: []string{
				"pause gastown/capable",
				"origin gastown/capable",
				"sandbox-release gastown/capable",
				"hand-back gastown/capable",
				"reset-agent-bead gastown/capable",
				"record-cleanup gastown/capable",
				"retire gastown/capable",
			},
			wantBack: 1,
		},
		{
			name:     "the seat names no branch to ask about",
			branch:   "",
			unlisted: true,
			wantCalls: []string{
				"pause gastown/capable",
				"origin gastown/capable",
			},
			wantItem: "seat-unreachable-release:gastown/capable:gt-p4aqut",
		},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, _ := unreachableReleaseTown(t)
			if !step.unlisted {
				reg := session.NewPrefixRegistry()
				reg.Register("gt", releaseSeat.Rig)
				d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
			}
			seedHostWorktreeStub(t, d)
			d.openGitFn = func(string) daemonGit { return &pushStubGit{originRefs: step.originRefs} }
			watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)
			f := &releaseFake{branch: step.branch}

			require.Equal(t, step.wantCalls, releaseProductionSandbox(t, d, f))
			require.Len(t, f.handBacks, step.wantBack)
			require.Empty(t, f.deleted, "a seat with no sandbox record has nothing to delete")
			raised := raisedItems(d)
			if step.wantItem == "" {
				require.Empty(t, raised, "a branch origin holds raises nothing")
				return
			}
			require.Len(t, raised, 1, "the release's answer must be raised once")
			require.Equal(t, step.wantItem, raised[0].Escalation.Fingerprint)
		})
	}
}

// TestSeatUnreachableReleaseFailureStaysOwedAndRaisesTheItem: a release whose
// origin read fails is not finished, and nothing may mark the outage released.
// The seat stays owed, so the next tick runs the release again from the pause,
// and the failure is raised with the seat, the bead and a bounded reason, so a
// seat no tick can release reaches a person instead of sitting out the outage
// (gt-35ihg5).
func TestSeatUnreachableReleaseFailureStaysOwedAndRaisesTheItem(t *testing.T) {
	t.Parallel()
	d, logs := unreachableReleaseTown(t)
	gone := errors.New("sandbox pod gastown-capable is gone")
	readErr := errors.New("could not resolve host: origin\ngit ls-remote: exit status 128")
	watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)
	f := &releaseFake{originErr: readErr}

	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(16*time.Minute), false)

	require.Equal(t, []string{"pause gastown/capable", "origin gastown/capable"}, f.calls,
		"a failed origin read must stop the release before the record is cleared")
	require.Empty(t, f.handBacks, "nothing may be handed back")

	raised := raisedItems(d)
	require.Len(t, raised, 1, "a failed release must raise one item")
	require.Equal(t, "seat-unreachable-release:gastown/capable:gt-p4aqut", raised[0].Escalation.Fingerprint)
	require.Equal(t, "MEDIUM", raised[0].Escalation.Severity)
	require.Contains(t, raised[0].Escalation.Reason, "gt-p4aqut")
	require.Contains(t, raised[0].Escalation.Reason, "16m")
	require.NotContains(t, raised[0].Escalation.Reason, "\n", "the reason carries one bounded line")
	require.Contains(t, logs.String(), "stays owed to the next tick")

	// The seat is still owed: the same outage is released again next tick, from
	// the pause, which is every step the failed release ran repeating safely.
	f.originErr = nil
	d.releaseUnreachableSeats(f.seams(), releaseStart.Add(17*time.Minute), false)
	require.Equal(t, []string{
		"pause gastown/capable",
		"origin gastown/capable",
		"pause gastown/capable",
		"origin gastown/capable",
		"sandbox-release gastown/capable",
		"hand-back gastown/capable",
		"reset-agent-bead gastown/capable",
		"record-cleanup gastown/capable",
		"retire gastown/capable",
	}, f.calls, "a failed release must stay owed to the next tick")
	require.Len(t, f.handBacks, 1, "the retried release hands the bead back once")
}

// TestSeatUnreachableReleaseStopsWhenTheRecordCannotBeAsked drives the production
// origin read through every answer that is not an answer about the branch: a
// lookup that cannot tell which record is the seat's, a record and a seat that
// together name no branch, a seat with no worktree, and an origin that cannot be
// read. Each fails the release before the sandbox is released, so the sandbox and
// its record stay for the next tick, and each is raised rather than passing as a
// release with nothing left to do (gt-35ihg5).
func TestSeatUnreachableReleaseStopsWhenTheRecordCannotBeAsked(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod gastown-capable is gone")
	readErr := errors.New("could not resolve host: origin")
	steps := []struct {
		name string
		// seed writes the seat's sandbox records for a rig the registry does not
		// list; worktree brings the stub the origin read resolves, and originErr
		// makes the git handle answer that origin cannot be read.
		seed      func(d *Daemon)
		worktree  bool
		originErr error
		want      string
	}{
		{"the lookup cannot tell which record is the seat's", func(d *Daemon) {
			seedReleaseRecords(t, d, releaseSandboxRecord(),
				session.SandboxRecord{Session: releaseSeat.Polecat, Rig: releaseSeat.Rig, Polecat: releaseSeat.Polecat, Branch: releaseBranch})
		}, true, nil, "2 records name it"},
		{"the record and the seat name no branch", func(d *Daemon) {
			rec := releaseSandboxRecord()
			rec.Branch = ""
			seedReleaseRecords(t, d, rec)
		}, true, nil, "names no branch to ask origin about"},
		{"the seat has no worktree to ask from", func(d *Daemon) {
			seedReleaseRecords(t, d, releaseSandboxRecord())
		}, false, nil, "has no worktree to read origin from"},
		{"origin cannot be read", func(d *Daemon) {
			seedReleaseRecords(t, d, releaseSandboxRecord())
		}, true, readErr, "could not resolve host: origin"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, logs := unreachableReleaseTown(t)
			step.seed(d)
			if step.worktree {
				seedHostWorktreeStub(t, d)
			}
			d.openGitFn = func(string) daemonGit {
				return &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}}, originErr: step.originErr}
			}
			watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)
			f := &releaseFake{}

			require.Equal(t, []string{"pause gastown/capable", "origin gastown/capable"},
				releaseProductionSandbox(t, d, f), "an unanswerable origin read must stop the release before the sandbox is released")
			require.Empty(t, f.handBacks, "nothing may be handed back")
			require.FileExists(t, session.SandboxRegistryDir(d.config.TownRoot)+"/"+releaseSession+".json",
				"the record must stay for the next tick")
			raised := raisedItems(d)
			require.Len(t, raised, 1, "a failed release must raise one item")
			require.Equal(t, "seat-unreachable-release:gastown/capable:gt-p4aqut", raised[0].Escalation.Fingerprint)
			require.Contains(t, logs.String(), step.want)
		})
	}
}

// TestSeatUnreachableReleaseStopsWhenTheSandboxCannotBeReleased: origin answered
// and holds the branch, and the sandbox release then fails — its record read
// cannot be answered, or the gateway refuses the delete. The sandbox and its
// record stay together, the bead is not handed back, and the failure is raised,
// so the next tick retries a release that is owed rather than one recorded as
// done (gt-35ihg5).
func TestSeatUnreachableReleaseStopsWhenTheSandboxCannotBeReleased(t *testing.T) {
	t.Parallel()
	gone := errors.New("sandbox pod gastown-capable is gone")
	steps := []struct {
		name string
		// readErr fails the record read the sandbox release asks for, keyed on
		// the step asking rather than on the position of the call; deleteErr is
		// what the gateway answers to the delete, and nilDeleter stands in for a
		// binary with no record deleter linked.
		readErr    error
		deleteErr  error
		nilDeleter bool
		wantLog    string
	}{
		{name: "the record cannot be read when the sandbox is released", readErr: errors.New("registry unreadable"), wantLog: "registry unreadable"},
		{name: "the gateway refuses the delete", deleteErr: errors.New("gateway: dial tcp 10.0.0.5:443: connection refused"), wantLog: "connection refused"},
		{name: "no sandbox deleter is linked", nilDeleter: true, wantLog: "no sandbox deleter is linked"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			d, logs := unreachableReleaseTown(t)
			rec := releaseSandboxRecord()
			reg := session.NewPrefixRegistry()
			reg.Register("gt", releaseSeat.Rig)
			d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
			require.NoError(t, session.WriteSandboxRecord(d.config.TownRoot, rec))
			seedHostWorktreeStub(t, d)
			f := &releaseFake{deleteErr: step.deleteErr, nilDeleter: step.nilDeleter}
			d.readSandboxRecord = func(string, string) (session.SandboxRecord, bool, error) {
				if f.running == "sandbox-release" && step.readErr != nil {
					return session.SandboxRecord{}, false, step.readErr
				}
				return rec, true, nil
			}
			d.openGitFn = func(string) daemonGit {
				return &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}}}
			}
			watchUnreachable(d, releaseStart, releaseSeat.Bead, gone)

			require.Equal(t, []string{"pause gastown/capable", "origin gastown/capable", "sandbox-release gastown/capable"},
				releaseProductionSandbox(t, d, f), "a sandbox that cannot be released must stop the release there")
			require.Empty(t, f.handBacks, "nothing may be handed back")
			require.FileExists(t, session.SandboxRegistryDir(d.config.TownRoot)+"/"+releaseSession+".json",
				"the record must stay with its sandbox for the next tick")
			raised := raisedItems(d)
			require.Len(t, raised, 1, "a failed release must raise one item")
			require.Equal(t, "seat-unreachable-release:gastown/capable:gt-p4aqut", raised[0].Escalation.Fingerprint)
			require.Contains(t, logs.String(), "sandbox of gastown/capable not released")
			require.Contains(t, logs.String(), step.wantLog)
		})
	}
}

// TestSeatUnreachableReleaseStopsWhenTheSandboxLookupFails: the sandbox release
// looks the seat's sandbox up again, and a lookup that cannot read the seat's
// record stops the release with the bead still the seat's, so the next tick
// retries it rather than the record going on a guess (gt-35ihg5).
func TestSeatUnreachableReleaseStopsWhenTheSandboxLookupFails(t *testing.T) {
	t.Parallel()
	d, logs := unreachableReleaseTown(t)
	rec := releaseSandboxRecord()
	require.NoError(t, session.WriteSandboxRecord(d.config.TownRoot, rec))
	seedHostWorktreeStub(t, d)
	// The origin read's lookup answers; the sandbox release's lookup cannot be
	// read, keyed on the step asking rather than on the position of the call.
	f := &releaseFake{}
	d.readSandboxRecord = func(string, string) (session.SandboxRecord, bool, error) {
		if f.running == "sandbox-release" {
			return session.SandboxRecord{}, false, errors.New("records listing unreadable")
		}
		return rec, true, nil
	}
	d.openGitFn = func(string) daemonGit {
		return &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}}}
	}
	watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod gastown-capable is gone"))

	require.Equal(t, []string{"pause gastown/capable", "origin gastown/capable", "sandbox-release gastown/capable"},
		releaseProductionSandbox(t, d, f), "a lookup that cannot read the record must stop the release there")
	require.Empty(t, f.handBacks, "nothing may be handed back")
	require.Empty(t, f.deleted, "no delete may run on a record the lookup could not read")
	require.FileExists(t, session.SandboxRegistryDir(d.config.TownRoot)+"/"+releaseSession+".json",
		"the record must stay for the next tick")
	raised := raisedItems(d)
	require.Len(t, raised, 1, "a failed release must raise one item")
	require.Equal(t, "seat-unreachable-release:gastown/capable:gt-p4aqut", raised[0].Escalation.Fingerprint)
	require.Contains(t, logs.String(), "records listing unreadable")
}

// TestSeatUnreachableReleaseRunsWhenTheSeatListFails: the unreachable seats come
// from the tracker, not from the working-seat list, so a tick whose list read
// fails must still release a seat due for it and log why the rest of the tick
// stopped (gt-wgim24).
func TestSeatUnreachableReleaseRunsWhenTheSeatListFails(t *testing.T) {
	t.Parallel()
	d, logs := newOverrunDaemon(t, false)
	seedReleaseBranchOnOrigin(t, d)
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
		"origin gastown/capable",
		"sandbox-release gastown/capable",
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
		{"no origin read seam", func(s *seatUnreachableStopSeams) { s.originHolds = nil }},
		{"no sandbox release seam", func(s *seatUnreachableStopSeams) { s.releaseSandbox = nil }},
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

// sandboxGitScript is an in-sandbox git runner that answers by subcommand, so a
// proof test says what the clone does without counting calls: fetchErr fails the
// fetch, aheadErr makes merge-base report the clone ahead of what was fetched.
type sandboxGitScript struct {
	fetchErr error
	aheadErr error
	calls    []string
}

func (g *sandboxGitScript) run(_ string, args ...string) ([]byte, error) {
	g.calls = append(g.calls, strings.Join(args, " "))
	switch args[0] {
	case "fetch":
		return nil, g.fetchErr
	case "merge-base":
		return nil, g.aheadErr
	}
	return nil, fmt.Errorf("unexpected in-sandbox git call %v", args)
}

// proofTown is a release town whose sandbox record names a clone, whose origin
// holds the seat's branch, and whose in-sandbox runner is script.
func proofTown(t *testing.T, script *sandboxGitScript) *Daemon {
	t.Helper()
	d, _ := unreachableReleaseTown(t)
	rec := releaseSandboxRecord()
	rec.WorkDir = "/sandbox/gt/gastown/polecats/capable/gastown"
	reg := session.NewPrefixRegistry()
	reg.Register("gt", releaseSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	require.NoError(t, session.WriteSandboxRecord(d.config.TownRoot, rec))
	seedHostWorktreeStub(t, d)
	d.sandboxGitRunnerFn = func(string, session.SandboxRecord) (func(string, ...string) ([]byte, error), error) {
		return script.run, nil
	}
	origin := &pushStubGit{originRefs: []git.RemoteRef{{Name: "refs/heads/" + releaseBranch, Hash: "aaa111"}}}
	d.openGitFn = func(string) daemonGit { return origin }
	return d
}

// TestSeatUnreachableSandboxOnOriginNeedsProofFromTheClone: origin holding a
// branch of the seat's name says nothing about the commits beyond its tip, so
// the proof is the clone's own: HEAD an ancestor of what origin's tip fetches.
// A clone that is ahead, and a fetch that fails, are both unproven, and so is a
// record that names no clone; only an ancestor HEAD, or no sandbox at all, is
// proof (gt-35ihg5).
func TestSeatUnreachableSandboxOnOriginNeedsProofFromTheClone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		script  sandboxGitScript
		want    bool
		wantErr bool
	}{
		{name: "the clone's head is on origin's tip", want: true},
		{name: "the clone is ahead of origin's tip", script: sandboxGitScript{aheadErr: errors.New("exit status 1")}, wantErr: true},
		{name: "the fetch fails", script: sandboxGitScript{fetchErr: errors.New("sandbox unreachable")}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := tc.script
			d := proofTown(t, &script)

			got, err := d.seatUnreachableSandboxOnOrigin(releaseSeat, releaseBranch)

			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantErr, err != nil, "err = %v", err)
			require.Equal(t, "fetch --quiet origin "+releaseBranch, script.calls[0],
				"the clone fetches the seat's branch before it compares")
		})
	}
	t.Run("a record naming no clone is unproven", func(t *testing.T) {
		t.Parallel()
		d, _ := unreachableReleaseTown(t)
		seedReleaseSandbox(t, d)
		got, err := d.seatUnreachableSandboxOnOrigin(releaseSeat, releaseBranch)
		require.False(t, got)
		require.Error(t, err)
	})
}

// TestSeatUnreachableReleaseKeepsASandboxAheadOfOrigin: origin holds a branch of
// the seat's name, but the clone holds commits beyond its tip. The release keeps
// the sandbox and its record, hands the bead back once and raises the
// sandbox-ahead item; and a clone origin does cover is released as before
// (gt-35ihg5).
func TestSeatUnreachableReleaseKeepsASandboxAheadOfOrigin(t *testing.T) {
	t.Parallel()
	t.Run("the clone is ahead", func(t *testing.T) {
		t.Parallel()
		script := &sandboxGitScript{aheadErr: errors.New("exit status 1")}
		d := proofTown(t, script)
		watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod is gone"))
		f := &releaseFake{}
		seams := f.seams()
		seams.originHolds = d.seatUnreachableOriginHolds
		seams.sandboxOnOrigin = d.seatUnreachableSandboxOnOrigin
		seams.releaseSandbox = func(seat overrunSeat) error {
			return d.seatUnreachableReleaseSandbox(seat, f.deleter)
		}

		d.releaseUnreachableSeats(seams, releaseStart.Add(16*time.Minute), false)

		require.Empty(t, f.deleted, "a sandbox ahead of origin must not be deleted")
		require.FileExists(t, session.SandboxRegistryDir(d.config.TownRoot)+"/"+releaseSession+".json")
		require.Len(t, f.handBacks, 1, "the bead still goes back once")
		raised := raisedItems(d)
		require.Len(t, raised, 1)
		require.Equal(t, "seat-unreachable-sandbox-ahead:gastown/capable:gt-p4aqut", raised[0].Escalation.Fingerprint)
		require.Contains(t, raised[0].Escalation.Description, "holds commits origin's tip does not")
	})
	t.Run("the clone cannot be read", func(t *testing.T) {
		t.Parallel()
		script := &sandboxGitScript{fetchErr: errors.New("sandbox unreachable")}
		d := proofTown(t, script)
		watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod is gone"))
		f := &releaseFake{}
		seams := f.seams()
		seams.originHolds = d.seatUnreachableOriginHolds
		seams.sandboxOnOrigin = d.seatUnreachableSandboxOnOrigin
		seams.releaseSandbox = func(seat overrunSeat) error {
			return d.seatUnreachableReleaseSandbox(seat, f.deleter)
		}

		d.releaseUnreachableSeats(seams, releaseStart.Add(16*time.Minute), false)

		require.Empty(t, f.deleted)
		raised := raisedItems(d)
		require.Len(t, raised, 1)
		require.Contains(t, raised[0].Escalation.Description, "could not be compared with origin")
	})
	t.Run("the clone is on origin", func(t *testing.T) {
		t.Parallel()
		d := proofTown(t, &sandboxGitScript{})
		watchUnreachable(d, releaseStart, releaseSeat.Bead, errors.New("sandbox pod is gone"))
		f := &releaseFake{}
		seams := f.seams()
		seams.originHolds = d.seatUnreachableOriginHolds
		seams.sandboxOnOrigin = d.seatUnreachableSandboxOnOrigin
		seams.releaseSandbox = func(seat overrunSeat) error {
			return d.seatUnreachableReleaseSandbox(seat, f.deleter)
		}

		d.releaseUnreachableSeats(seams, releaseStart.Add(16*time.Minute), false)

		require.Equal(t, []string{releaseSession}, f.deleted)
		require.Empty(t, raisedItems(d))
	})
}
