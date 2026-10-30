package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/jev/battery"
	jevrules "github.com/steveyegge/gastown/internal/jev/rules"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

const (
	endRig       = "gastown"
	endPolecat   = "agate"
	endBead      = "gt-n4xq8"
	endBranch    = "polecat/agate/gt-n4xq8+x1"
	endSandboxIn = "deepseek-flash-sandbox"
	endHostAgent = "deepseek-flash"
)

// endSettings is a town with one tmux seat and one openshell seat, the two
// backends a polecat's agent bead can name.
const endSettings = `{"type":"town-settings","version":1,
"agents":{"deepseek-flash":{"command":"claude"},"deepseek-flash-sandbox":{"command":"claude"}},
"sandboxes":{"polecat-default":{"gateway":"gw1","image":"gastown-polecat:dev","policy":"deploy/polecat/policy.yaml"}},
"polecat_pool":{"seats":{
 "deepseek-flash":{"cap":1},
 "deepseek-flash-sandbox":{"cap":1,"backend":"openshell","sandbox":"polecat-default"}}}}`

// seatEndFake is one sandbox-seat end scenario: the town, the seat's agent
// bead, the gateway's answers, and what the step did.
type seatEndFake struct {
	t         *testing.T
	town      string
	agentBead string
	agentDB   *beadsfake.Fake
	host      *patrolScanHost
	removals  []string
	removeErr error
	// removeDeleted is what the seam reports deleted beside removeErr: true
	// is a removal that failed halfway and left the seat partly gone.
	removeDeleted bool
	gwAsked       []string
	seatFields    beads.AgentFields
	// cursorAt and cursorErr answer the daemon's jev_rules cursor seam: a zero
	// cursorAt with no err is a town that has not written a cursor yet.
	cursorAt  time.Time
	cursorErr error
}

func newSeatEndFake(t *testing.T) *seatEndFake {
	t.Helper()
	town := t.TempDir()
	writeTownFile(t, town, "settings/config.json", endSettings)
	f := &seatEndFake{
		t:       t,
		town:    town,
		agentDB: beadsfake.New(),
		seatFields: beads.AgentFields{
			RoleType: "polecat", Rig: endRig, Agent: endSandboxIn,
			Branch: endBranch, CleanupStatus: string(polecat.CleanupClean),
		},
	}
	f.agentBead = beads.AgentBeadIDFor(constants.RolePolecat, endRig, endPolecat, town)
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	d.rulesCursorAt = func(string) (time.Time, bool, error) {
		if f.cursorErr != nil {
			return time.Time{}, false, f.cursorErr
		}
		if f.cursorAt.IsZero() {
			return time.Time{}, false, nil
		}
		return f.cursorAt, true, nil
	}
	f.host = &patrolScanHost{
		d:                 d,
		openRecoveryBeads: func([]string) beads.Client { return f.agentDB },
		sandboxExists: func(gateway, name string) (bool, error) {
			f.gwAsked = append(f.gwAsked, gateway+"|"+name)
			return false, nil
		},
		removeSandboxSeatDirs: func(rigPath, rig, name string) (bool, error) {
			if f.removeErr != nil {
				return f.removeDeleted, f.removeErr
			}
			f.removals = append(f.removals, rig+"|"+name)
			return polecat.RemoveSandboxSeatHostDirs(rigPath, rig, name, func(_, _ string) error { return nil })
		},
	}
	return f
}

// seedAgent writes the seat's agent bead from f.seatFields.
func (f *seatEndFake) seedAgent() {
	f.t.Helper()
	if _, err := f.agentDB.Create(beads.CreateOptions{
		ID: f.agentBead, Title: f.agentBead, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(f.agentBead, &f.seatFields),
	}); err != nil {
		f.t.Fatal(err)
	}
}

// setAgent rewrites the seat's agent bead from f.seatFields, so a test can
// change a fact between two passes of the end.
func (f *seatEndFake) setAgent() {
	f.t.Helper()
	desc := beads.FormatAgentDescription(f.agentBead, &f.seatFields)
	if err := f.agentDB.Update(f.agentBead, beads.UpdateOptions{Description: &desc}); err != nil {
		f.t.Fatal(err)
	}
}

// retire records the seat as submitted for landing and then retired to stop,
// the state gt done and the landing leave behind.
func (f *seatEndFake) retire(desired intent.Desired) {
	f.t.Helper()
	seat := intent.Seat{Rig: endRig, Role: constants.RolePolecat, Name: endPolecat}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	if err := intent.MarkSubmitted(f.town, seat, endBead, "gt done", now); err != nil {
		f.t.Fatal(err)
	}
	if desired == intent.DesiredStop {
		if _, err := intent.ClearLanded(f.town, seat, endBead, "landing worker", now); err != nil {
			f.t.Fatal(err)
		}
	}
}

// park records the operator's hold on the seat.
func (f *seatEndFake) park() {
	f.t.Helper()
	seat := intent.Seat{Rig: endRig, Role: constants.RolePolecat, Name: endPolecat}
	if _, err := intent.Update(f.town, seat, func(r *intent.Record) error {
		r.Desired = intent.DesiredPark
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

// seatDirs creates the host directories a sandbox seat leaves, at the paths
// the spawn and teardown code give them (the layout the bead's survey lists),
// spelled out here rather than taken from the code under test.
func (f *seatEndFake) seatDirs() polecat.SandboxSeatHostDirs {
	f.t.Helper()
	rigPath := filepath.Join(f.town, endRig)
	dirs := polecat.SandboxSeatHostDirs{
		PolecatDir: filepath.Join(rigPath, "polecats", endPolecat),
		ClonePath:  filepath.Join(rigPath, "polecats", endPolecat, endRig),
		PromptFile: filepath.Join(rigPath, "polecats", ".claude", "sandbox", "system-prompt-"+endPolecat+".md"),
	}
	for _, path := range []string{filepath.Join(dirs.ClonePath, "CLAUDE.md"), dirs.PromptFile} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return dirs
}

// privateLandingsDir is a directory a landings file accepts. Its writer refuses
// one another account could write to, and t.TempDir's numbered directory is made
// 0777 less the umask, so under the gate's permissive umask (000) it is
// world-writable. This one is made 0700 below it, a mode the umask can only
// take bits from; the writer's check is left as it is.
func privateLandingsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "landings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func dirExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

// TestEndFinishedSandboxSeat is the seat's end: the host directories go when
// every fact says the seat is finished, and any one fact that says work may
// still be at risk keeps them all.
func TestEndFinishedSandboxSeat(t *testing.T) {
	t.Parallel()
	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	tests := []struct {
		name        string
		fields      func(*beads.AgentFields)
		exists      bool
		existsErr   error
		desired     intent.Desired
		parked      bool
		landing     sandboxSeatLanding
		removeErr   error
		wantRemoved bool
		wantAsked   bool
	}{
		{
			name: "a finished seat goes", landing: finished, desired: intent.DesiredStop,
			wantRemoved: true, wantAsked: true,
		},
		{
			name:    "unlanded work keeps it",
			landing: sandboxSeatLanding{BeadID: endBead, Branch: endBranch}, desired: intent.DesiredStop,
			wantAsked: true,
		},
		{
			name:    "a branch that never reached origin keeps it",
			fields:  func(f *beads.AgentFields) { f.CleanupStatus = string(polecat.CleanupUnpushed) },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name:    "a missing cleanup status keeps it",
			fields:  func(f *beads.AgentFields) { f.CleanupStatus = "" },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name:    "a recorded push failure keeps it",
			fields:  func(f *beads.AgentFields) { f.PushFailed = true },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name: "a sandbox still there keeps it", exists: true,
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name: "a gateway that cannot answer keeps it", existsErr: errors.New("gateway down"),
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name:    "a hook still set keeps it",
			fields:  func(f *beads.AgentFields) { f.HookBead = endBead },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name:    "another branch keeps it",
			fields:  func(f *beads.AgentFields) { f.Branch = "polecat/agate/gt-other+x1" },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name:    "a seat something still wants keeps it",
			landing: finished, desired: intent.DesiredRun, wantAsked: true,
		},
		{
			name:    "a parked seat keeps it",
			landing: finished, parked: true, wantAsked: true,
		},
		{
			name:    "a tmux seat keeps its worktree",
			fields:  func(f *beads.AgentFields) { f.Agent = endHostAgent },
			landing: finished, desired: intent.DesiredStop,
		},
		{
			name:    "a failed removal keeps it",
			landing: finished, desired: intent.DesiredStop, removeErr: errors.New("disk is read-only"),
			wantAsked: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSeatEndFake(t)
			if tt.fields != nil {
				tt.fields(&f.seatFields)
			}
			f.seedAgent()
			f.host.sandboxExists = func(gateway, name string) (bool, error) {
				f.gwAsked = append(f.gwAsked, gateway+"|"+name)
				return tt.exists, tt.existsErr
			}
			if tt.parked {
				f.park()
			} else if tt.desired != "" {
				f.retire(tt.desired)
			}
			f.removeErr = tt.removeErr
			dirs := f.seatDirs()

			removed := f.host.endFinishedSandboxSeat(endRig, endPolecat, tt.landing)

			if removed != tt.wantRemoved {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			if got := dirExists(t, dirs.PolecatDir); got == tt.wantRemoved {
				t.Errorf("seat directory exists = %v, want %v", got, !tt.wantRemoved)
			}
			if got := dirExists(t, dirs.PromptFile); got == tt.wantRemoved {
				t.Errorf("prompt file exists = %v, want %v", got, !tt.wantRemoved)
			}
			if asked := len(f.gwAsked) > 0; asked != tt.wantAsked {
				t.Errorf("gateway asked = %v, want %v (%v)", asked, tt.wantAsked, f.gwAsked)
			}
		})
	}
}

// An agent bead that cannot be read is not "no bead": the seat is kept and
// nothing is measured or removed.
func TestEndFinishedSandboxSeatKeepsASeatWithAnUnreadableAgentBead(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.agentDB.FailWith("show", errors.New("bd: database is locked"))
	dirs := f.seatDirs()
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}) {
		t.Error("a seat whose agent bead cannot be read was ended")
	}
	if !dirExists(t, dirs.PolecatDir) {
		t.Error("the seat directory was removed")
	}
	if len(f.gwAsked) != 0 {
		t.Errorf("gateway asked %v; want nothing asked", f.gwAsked)
	}
}

// A polecat with no agent bead is not a seat this town tracks: nothing is read
// from the gateway, nothing is removed, and the keep is logged.
func TestEndFinishedSandboxSeatWithoutAnAgentBead(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	dirs := f.seatDirs()
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}) {
		t.Error("a seat with no agent bead was ended")
	}
	if !dirExists(t, dirs.PolecatDir) {
		t.Error("the seat directory was removed")
	}
	if len(f.gwAsked) != 0 {
		t.Errorf("gateway asked %v; want nothing asked", f.gwAsked)
	}
	if !strings.Contains(logs.String(), "kept: no agent bead") {
		t.Errorf("log %q does not say the seat was kept", logs.String())
	}
}

// An unsafe rig or polecat name is refused before anything is read under it:
// no agent bead is read and no gateway is asked.
func TestEndFinishedSandboxSeatRefusesUnsafeNamesBeforeReading(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.host.openRecoveryBeads = func([]string) beads.Client {
		t.Error("an agent bead was read for an unsafe name")
		return f.agentDB
	}
	f.host.sandboxExists = func(gateway, name string) (bool, error) {
		t.Error("the gateway was asked for an unsafe name")
		return false, nil
	}
	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	for _, name := range []string{"../agate", "a/agate", "", "."} {
		if f.host.endFinishedSandboxSeat(endRig, name, finished) {
			t.Errorf("a seat named %q was ended", name)
		}
	}
}

// The gateway's answer is measured under the seat's lock: while the question is
// asked, a sling's lock on the polecat is held, so the answer cannot go stale
// before the removal that acts on it.
func TestEndFinishedSandboxSeatMeasuresTheGatewayUnderTheSeatLock(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	lockPath := filepath.Join(f.town, endRig, ".runtime", "locks", "polecat-"+endPolecat+".lock")
	asked := 0
	f.host.sandboxExists = func(gateway, name string) (bool, error) {
		asked++
		other := flock.New(lockPath)
		locked, err := other.TryLock()
		if err != nil {
			t.Errorf("probing the seat lock: %v", err)
		}
		if locked {
			_ = other.Unlock()
			t.Error("the gateway was asked while the seat lock was free")
		}
		return false, nil
	}
	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
		t.Fatal("a finished seat was kept")
	}
	if asked != 1 {
		t.Fatalf("the gateway was asked %d times, want once", asked)
	}
}

// runSandboxSeatEnd turns the landing's record into the seat's end: the record
// for the landed head proves the landing, its absence is unlanded work, and a
// landing with no seat or no readable record ends nothing.
func TestRunSandboxSeatEndReadsTheLandingRecord(t *testing.T) {
	t.Parallel()
	const head = "0123456789abcdef"
	tests := []struct {
		name        string
		work        land.Work
		landings    func(t *testing.T) *land.LandingsFile
		wantCalled  bool
		wantLanded  bool
		wantLandedB string
	}{
		{
			name: "a recorded landing ends the seat",
			work: land.Work{BeadID: endBead, Head: head, Branch: endBranch, Worker: endPolecat},
			landings: func(t *testing.T) *land.LandingsFile {
				t.Helper()
				f := &land.LandingsFile{Path: filepath.Join(privateLandingsDir(t), "landings.jsonl")}
				if err := f.Append(land.LandingRecord{BeadID: endBead, Head: head, Branch: endBranch}); err != nil {
					t.Fatal(err)
				}
				return f
			},
			wantCalled: true, wantLanded: true, wantLandedB: endBranch,
		},
		{
			name: "a landing with no record is unlanded",
			work: land.Work{BeadID: endBead, Head: head, Branch: endBranch, Worker: endPolecat},
			landings: func(t *testing.T) *land.LandingsFile {
				t.Helper()
				return &land.LandingsFile{Path: filepath.Join(privateLandingsDir(t), "landings.jsonl")}
			},
			wantCalled: true, wantLanded: false, wantLandedB: endBranch,
		},
		{
			// The landing worker appends while the seat's end reads: a torn
			// line beside w's record must not hide the record.
			name: "a torn line beside the record still finds it",
			work: land.Work{BeadID: endBead, Head: head, Branch: endBranch, Worker: endPolecat},
			landings: func(t *testing.T) *land.LandingsFile {
				t.Helper()
				f := &land.LandingsFile{Path: filepath.Join(privateLandingsDir(t), "landings.jsonl")}
				if err := f.Append(land.LandingRecord{BeadID: endBead, Head: head, Branch: endBranch}); err != nil {
					t.Fatal(err)
				}
				tail, err := os.OpenFile(f.Path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tail.Close() }()
				if _, err := tail.WriteString(`{"bead_id":"gt-other`); err != nil {
					t.Fatal(err)
				}
				return f
			},
			wantCalled: true, wantLanded: true, wantLandedB: endBranch,
		},
		{
			name: "a crew landing has no seat",
			work: land.Work{BeadID: endBead, Head: head, Branch: endBranch},
			landings: func(t *testing.T) *land.LandingsFile {
				t.Helper()
				return &land.LandingsFile{Path: filepath.Join(privateLandingsDir(t), "landings.jsonl")}
			},
		},
		{
			name: "no landings file ends nothing",
			work: land.Work{BeadID: endBead, Head: head, Branch: endBranch, Worker: endPolecat},
		},
		{
			name: "a record that cannot be read ends nothing",
			work: land.Work{BeadID: endBead, Head: head, Branch: endBranch, Worker: endPolecat},
			landings: func(t *testing.T) *land.LandingsFile {
				t.Helper()
				return &land.LandingsFile{Path: privateLandingsDir(t)}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			type ended struct {
				rig, name string
				landing   sandboxSeatLanding
			}
			var got []ended
			d := &Daemon{
				config: &Config{TownRoot: t.TempDir()},
				logger: log.New(io.Discard, "", 0),
				endSandboxSeat: func(rig, name string, landing sandboxSeatLanding) bool {
					got = append(got, ended{rig, name, landing})
					return true
				},
			}
			var landings *land.LandingsFile
			if tt.landings != nil {
				landings = tt.landings(t)
			}
			d.runSandboxSeatEnd(endRig, landings, tt.work)

			if called := len(got) > 0; called != tt.wantCalled {
				t.Fatalf("seat end called = %v, want %v (%v)", called, tt.wantCalled, got)
			}
			if !tt.wantCalled {
				return
			}
			if got[0].rig != endRig || got[0].name != endPolecat {
				t.Errorf("ended %s/%s, want %s/%s", got[0].rig, got[0].name, endRig, endPolecat)
			}
			if got[0].landing.Landed != tt.wantLanded {
				t.Errorf("landed = %v, want %v", got[0].landing.Landed, tt.wantLanded)
			}
			if got[0].landing.Branch != tt.wantLandedB {
				t.Errorf("landing branch = %q, want %q", got[0].landing.Branch, tt.wantLandedB)
			}
			if got[0].landing.BeadID != tt.work.BeadID {
				t.Errorf("landing bead = %q, want %q", got[0].landing.BeadID, tt.work.BeadID)
			}
		})
	}
}

// The reasons below are pinned: each one names the fact that kept the seat, so
// an operator reading the daemon log knows which measurement to look at.
func TestSandboxSeatEndKeepReasons(t *testing.T) {
	t.Parallel()
	finished := sandboxSeatEnd{
		SandboxSeat: true, SandboxGone: true, Landed: true, Retired: true,
		Branch: endBranch, LandedBranch: endBranch, CleanupStatus: polecat.CleanupClean,
	}
	if why := finished.keepReason(); why != "" {
		t.Fatalf("a finished seat was kept: %s", why)
	}
	tests := []struct {
		name  string
		mut   func(*sandboxSeatEnd)
		want  string
		empty bool
	}{
		{"host seat", func(e *sandboxSeatEnd) { e.SandboxSeat = false }, "not a sandbox seat", false},
		{"sandbox live", func(e *sandboxSeatEnd) { e.SandboxGone = false }, "the gateway has not confirmed", false},
		{"unlanded", func(e *sandboxSeatEnd) { e.Landed = false }, "unlanded", false},
		{"unpushed", func(e *sandboxSeatEnd) { e.CleanupStatus = polecat.CleanupUnpushed }, "has_unpushed", false},
		{"uncommitted", func(e *sandboxSeatEnd) { e.CleanupStatus = polecat.CleanupUncommitted }, "has_uncommitted", false},
		{"missing status", func(e *sandboxSeatEnd) { e.CleanupStatus = "" }, "<missing>", false},
		{"push failed", func(e *sandboxSeatEnd) { e.PushFailed = true }, "push_failed=true", false},
		{"rules ask pending", func(e *sandboxSeatEnd) { e.RulesPending = true }, "jev_rules has not yet asked about this submission", false},
		// The may-go baseline beside it: the seat is landed, clean and retired,
		// and only an outstanding rules ask holds it.
		{"the rules ask answered, so the seat may go", func(e *sandboxSeatEnd) { e.RulesPending = false }, "", true},
		{"unlanded with the rules ask pending", func(e *sandboxSeatEnd) { e.Landed = false; e.RulesPending = true }, "unlanded", false},
		{"unpushed with the rules ask pending", func(e *sandboxSeatEnd) { e.CleanupStatus = polecat.CleanupUnpushed; e.RulesPending = true }, "has_unpushed", false},
		{"hook set", func(e *sandboxSeatEnd) { e.HookBead = endBead }, "hook_bead=" + endBead, false},
		{"another branch", func(e *sandboxSeatEnd) { e.Branch = "polecat/agate/gt-other+x1" }, "not the landed", false},
		{"seat branch unrecorded", func(e *sandboxSeatEnd) { e.Branch = "" }, "branch=<missing>", false},
		{"landed branch unrecorded", func(e *sandboxSeatEnd) { e.LandedBranch = "  " }, "landed_branch=<missing>", false},
		{"still wanted", func(e *sandboxSeatEnd) { e.Retired = false }, "does not say stop", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := finished
			tt.mut(&e)
			why := e.keepReason()
			if tt.empty {
				if why != "" {
					t.Errorf("keepReason = %q, want the seat to go", why)
				}
				return
			}
			if !strings.Contains(why, tt.want) {
				t.Errorf("keepReason = %q, want it to name %q", why, tt.want)
			}
		})
	}
}

// The rules ask is measured: the seat keeps its host directories while the
// jev_rules patrol is on, the submission is inside the one-hour window, and the
// battery's cursor has not reached that submission. Every way of not knowing —
// the patrol off, no cursor, an unreadable cursor, no submission stamp —
// leaves the seat to the other facts (gt-381oj). The cursor is read through
// the daemon's seam, so the unreadable path is one a test can drive, and it
// must say so: an unreadable cursor never looks like a clean one.
func TestSandboxSeatEndRulesPending(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	submitted := now.Add(-10 * time.Minute)
	at := func(t time.Time) (time.Time, bool, error) { return t, true, nil }
	tests := []struct {
		name        string
		patrol      bool
		submitted   time.Time
		cursor      func() (time.Time, bool, error)
		wantPending bool
		// wantLog is the one line an unreadable cursor must leave; empty means
		// the measurement must say nothing at all.
		wantLog string
	}{
		{
			name: "a cursor before the submission is pending", patrol: true, submitted: submitted,
			cursor: func() (time.Time, bool, error) { return at(submitted.Add(-5 * time.Minute)) }, wantPending: true,
		},
		{
			name: "a cursor past the submission is not pending", patrol: true, submitted: submitted,
			cursor: func() (time.Time, bool, error) { return at(submitted.Add(time.Minute)) },
		},
		{
			name: "a submission past the window is not pending", patrol: true,
			submitted: now.Add(-rulesPendingWindow - time.Minute),
			cursor:    func() (time.Time, bool, error) { return at(now.Add(-rulesPendingWindow - 5*time.Minute)) },
		},
		{
			name: "a future submission is clamped to now and is pending", patrol: true,
			submitted:   now.Add(2 * time.Hour),
			cursor:      func() (time.Time, bool, error) { return at(now.Add(-5 * time.Minute)) },
			wantPending: true,
		},
		{
			name: "a town with the patrol off is not pending", submitted: submitted,
			cursor: func() (time.Time, bool, error) { return at(submitted.Add(-5 * time.Minute)) },
		},
		{
			name: "a cursor that cannot be read is not pending and says so", patrol: true, submitted: submitted,
			cursor: func() (time.Time, bool, error) {
				return time.Time{}, false, errors.New("jev_rules cursor: bad checksum")
			},
			wantLog: "jev_rules cursor unreadable: jev_rules cursor: bad checksum; the seat is not held for rules",
		},
		{
			name: "a cursor never written is not pending and says nothing", patrol: true, submitted: submitted,
			cursor: func() (time.Time, bool, error) { return time.Time{}, false, nil },
		},
		{
			name: "a submission with no stamp is not pending", patrol: true,
			cursor: func() (time.Time, bool, error) { return at(submitted.Add(-5 * time.Minute)) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSeatEndFake(t)
			f.seedAgent()
			f.retire(intent.DesiredStop)
			var logs strings.Builder
			d := f.host.d
			d.logger = log.New(&logs, "", 0)
			d.clock = clockwork.NewFakeClockAt(now)
			if tt.patrol {
				d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
					JevRules: &JevRulesConfig{Enabled: true},
				}}
			}
			d.rulesCursorAt = func(path string) (time.Time, bool, error) {
				if want := jevrules.CursorPath(f.town); path != want {
					t.Errorf("cursor read at %q, want the rules battery's %q", path, want)
				}
				return tt.cursor()
			}
			landing := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Submitted: tt.submitted, Landed: true}

			end, unmeasured := f.host.measureSandboxSeatEnd(endRig, endPolecat, landing)

			if unmeasured != "" {
				t.Fatalf("the seat was left unmeasured: %s", unmeasured)
			}
			if end.RulesPending != tt.wantPending {
				t.Errorf("RulesPending = %v, want %v", end.RulesPending, tt.wantPending)
			}
			if tt.wantLog != "" {
				if got := logs.String(); !strings.Contains(got, endRig+"/"+endPolecat) || !strings.Contains(got, tt.wantLog) {
					t.Errorf("log %q does not carry the seat and %q", got, tt.wantLog)
				}
				return
			}
			if got := logs.String(); got != "" {
				t.Errorf("log = %q, want nothing said", got)
			}
		})
	}
}

// A removal that deleted some of the seat's directories and then failed is not
// a kept seat: the log says the removal was partial, so an operator is not sent
// looking for a seat that is half gone.
func TestEndFinishedSandboxSeatLogsAPartialRemovalAsSuch(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	f.removeErr = errors.New("removing /town/gastown/polecats/agate: permission denied")
	f.removeDeleted = true

	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
		t.Error("a removal that failed was reported as a finished seat")
	}
	got := logs.String()
	if !strings.Contains(got, "partly removed") {
		t.Errorf("log %q does not say the removal was partial", got)
	}
	if strings.Contains(got, "kept:") {
		t.Errorf("log %q calls a seat whose directories were partly deleted kept", got)
	}
}

// The seat's end the daemon builds for itself, with no injected end, is the
// production path: the facts come from the seat's agent bead and the gateway
// seam, and the removal is polecat's own. Nothing here replaces the step.
func TestSandboxSeatEnderDefaultPathEndsTheSeat(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	const head = "0123456789abcdef"
	landings := &land.LandingsFile{Path: filepath.Join(privateLandingsDir(t), "landings.jsonl")}
	if err := landings.Append(land.LandingRecord{BeadID: endBead, Head: head, Branch: endBranch}); err != nil {
		t.Fatal(err)
	}

	d := f.host.d
	d.sandboxExists = f.host.sandboxExists
	d.openRecoveryBeads = func([]string) beads.Client { return f.agentDB }
	d.endSandboxSeat = nil
	d.runSandboxSeatEnd(endRig, landings, land.Work{BeadID: endBead, Head: head, Branch: endBranch, Worker: endPolecat})

	if dirExists(t, dirs.PolecatDir) {
		t.Error("the seat directory the daemon's own end removes is still there")
	}
	if dirExists(t, dirs.PromptFile) {
		t.Error("the prompt file the daemon's own end removes is still there")
	}
	if len(f.gwAsked) != 1 {
		t.Errorf("gateway asked %v, want one question", f.gwAsked)
	}
}

// A seat the daemon ends through its own path keeps its host directories while
// the rules battery has not asked about its submission, and the same seat goes
// once the cursor has passed that submission (gt-381oj).
func TestSandboxSeatEnderDefaultPathKeepsASeatUntilTheRulesAsk(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	const head = "0123456789abcdef"
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	submitted := now.Add(-10 * time.Minute)

	d := f.host.d
	d.clock = clockwork.NewFakeClockAt(now)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JevRules: &JevRulesConfig{Enabled: true},
	}}
	d.sandboxExists = f.host.sandboxExists
	d.openRecoveryBeads = func([]string) beads.Client { return f.agentDB }
	// The production cursor reader, reading the battery's file as it does in
	// the town; the other rules tests drive the daemon's seam instead.
	d.rulesCursorAt = nil
	d.endSandboxSeat = nil
	landings := &land.LandingsFile{Path: filepath.Join(privateLandingsDir(t), "landings.jsonl")}
	if err := landings.Append(land.LandingRecord{BeadID: endBead, Head: head, Branch: endBranch}); err != nil {
		t.Fatal(err)
	}
	cursorPath := jevrules.CursorPath(f.town)
	if err := battery.WriteCursor(cursorPath, battery.Cursor{Time: submitted.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	work := land.Work{BeadID: endBead, Head: head, Branch: endBranch, Worker: endPolecat, Submitted: submitted}

	d.runSandboxSeatEnd(endRig, landings, work)

	if !dirExists(t, dirs.PolecatDir) || !dirExists(t, dirs.PromptFile) {
		t.Fatal("the seat went before the rules battery asked about its submission")
	}
	if got := logs.String(); !strings.Contains(got, "kept: jev_rules has not yet asked about this submission") {
		t.Errorf("log %q does not name the rules ask as the reason the seat stayed", got)
	}

	if err := battery.WriteCursor(cursorPath, battery.Cursor{Time: submitted.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	d.runSandboxSeatEnd(endRig, landings, work)

	if dirExists(t, dirs.PolecatDir) || dirExists(t, dirs.PromptFile) {
		t.Error("the seat stayed after the cursor passed its submission")
	}
}

// jevRulesOnly is a patrol config with the rules battery enabled and nothing
// else said.
func jevRulesOnly() *DaemonPatrolConfig {
	return &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevRules: &JevRulesConfig{Enabled: true}}}
}

// seatEndWaitingForRules prepares a finished sandbox seat whose rules ask is
// outstanding and runs the landing's own pass: the seat is kept and recorded,
// and the test drives what happens on later passes.
func seatEndWaitingForRules(t *testing.T, now time.Time) (*seatEndFake, *clockwork.FakeClock, sandboxSeatLanding) {
	t.Helper()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	clk := clockwork.NewFakeClockAt(now)
	f.host.d.clock = clk
	f.host.d.patrolConfig = jevRulesOnly()
	// A later pass builds its own end over the daemon's seams, not the host's.
	f.host.d.sandboxExists = f.host.sandboxExists
	f.host.d.openRecoveryBeads = f.host.openRecoveryBeads
	submitted := now.Add(-10 * time.Minute)
	f.cursorAt = submitted.Add(-time.Minute)
	landing := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Submitted: submitted, Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landing) {
		t.Fatal("the landing's pass removed a seat whose rules ask was outstanding")
	}
	return f, clk, landing
}

// A seat kept for the rules ask is recorded with its landing facts: the
// landing's own run of the end is spent, and the record is what offers the seat
// to a later pass (gt-381oj).
func TestEndFinishedSandboxSeatRecordsAPendingSeatEnd(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f, _, landing := seatEndWaitingForRules(t, now)

	if !dirExists(t, filepath.Join(f.town, endRig, "polecats", endPolecat)) {
		t.Fatal("a kept seat lost its host directory")
	}
	records, bad, err := readPendingSandboxSeatEnds(f.town)
	if err != nil {
		t.Fatalf("reading the pending ends: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("unreadable pending ends = %v", bad)
	}
	want := pendingSandboxSeatEnd{Rig: endRig, Name: endPolecat, Landing: landing}
	if len(records) != 1 || records[0] != want {
		t.Fatalf("pending ends = %+v, want just %+v", records, want)
	}
}

// A record that cannot be written is a seat nothing will offer again, so the
// failure is said once and the seat is kept: it must not look like a recorded
// seat, and a lost record must never cost the seat its directories (gt-381oj).
func TestEndFinishedSandboxSeatKeepsASeatWhoseRecordCannotBeWritten(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := newSeatEndFake(t)
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	f.host.d.clock = clockwork.NewFakeClockAt(now)
	f.host.d.patrolConfig = jevRulesOnly()
	f.cursorAt = now.Add(-11 * time.Minute)
	f.host.writePendingSeatEnd = func(string, string, string, sandboxSeatLanding) error {
		return errors.New("writing the pending seat end: no space left on device")
	}

	landing := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Submitted: now.Add(-10 * time.Minute), Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landing) {
		t.Fatal("a seat whose pending record could not be written was removed")
	}
	if !dirExists(t, dirs.PolecatDir) {
		t.Error("a seat kept for the rules ask lost its host directory")
	}
	want := "cannot record the seat for a later rules pass: writing the pending seat end: no space left on device"
	if got := strings.Count(logs.String(), want); got != 1 {
		t.Errorf("log says %q %d times, want once: %q", want, got, logs.String())
	}
}

// The pending record is written under the seat lock the decision is made
// under, so a heartbeat never lists a record this end has not finished
// deciding on (gt-381oj).
func TestEndFinishedSandboxSeatRecordsThePendingSeatUnderTheSeatLock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	f.host.d.clock = clockwork.NewFakeClockAt(now)
	f.host.d.patrolConfig = jevRulesOnly()
	f.cursorAt = now.Add(-11 * time.Minute)
	// The lock file the seat's end takes, spelled out here rather than taken
	// from the code under test.
	lockPath := filepath.Join(f.town, endRig, ".runtime", "locks", "polecat-"+endPolecat+".lock")
	var wrote, lockedWhileWriting bool
	f.host.writePendingSeatEnd = func(townRoot, rig, name string, landing sandboxSeatLanding) error {
		probe := flock.New(lockPath)
		held, err := probe.TryLock()
		if err != nil {
			t.Fatalf("probing the seat lock: %v", err)
		}
		if held {
			_ = probe.Unlock()
		}
		wrote, lockedWhileWriting = true, !held
		return writePendingSandboxSeatEnd(townRoot, rig, name, landing)
	}

	landing := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Submitted: now.Add(-10 * time.Minute), Landed: true}
	f.host.endFinishedSandboxSeat(endRig, endPolecat, landing)

	if !wrote {
		t.Fatal("the seat kept for the rules ask was not recorded")
	}
	if !lockedWhileWriting {
		t.Error("the pending record was written with the seat lock not held")
	}
	if records, _, err := readPendingSandboxSeatEnds(f.town); err != nil || len(records) != 1 {
		t.Fatalf("pending ends = %+v, %v; want the one record", records, err)
	}
}

// An end that measured nothing keeps the seat but does not leave its record for
// ever: past the one-hour window no ask can be pending, so the record goes,
// while one still inside its window stays for the next heartbeat to retry
// (gt-381oj).
func TestEndFinishedSandboxSeatExpiresAStalePendingRecord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	// unreadableBead, unreadableGateway and lockedOut are the three ways a pass
	// measures nothing: before the lock, inside it, and unable to take it.
	unreadableBead := func(f *seatEndFake) {
		f.agentDB.FailWith("show", errors.New("dolt: connection refused"))
	}
	unreadableGateway := func(f *seatEndFake) {
		f.host.sandboxExists = func(string, string) (bool, error) {
			return false, errors.New("gateway unreachable")
		}
	}
	lockedOut := func(f *seatEndFake) {
		// The lock's directory is a file, so the lock cannot be taken at all.
		lockDir := filepath.Join(f.town, endRig, ".runtime", "locks")
		if err := os.RemoveAll(lockDir); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(lockDir, []byte("x"), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	tests := []struct {
		name       string
		advance    time.Duration
		change     func(f *seatEndFake)
		wantRecord bool
	}{
		// The recorded submission is ten minutes before now, so the window
		// closes fifty minutes after now.
		{name: "inside the window the record stays", advance: 30 * time.Minute, change: unreadableBead, wantRecord: true},
		{name: "past the window the record goes", advance: 50 * time.Minute, change: unreadableBead},
		{name: "an unreadable gateway inside the lock expires too", advance: 50 * time.Minute, change: unreadableGateway},
		{name: "a lock that cannot be taken expires too", advance: 50 * time.Minute, change: lockedOut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, clk, _ := seatEndWaitingForRules(t, now)
			clk.Advance(tt.advance)
			tt.change(f)

			landing := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Submitted: now.Add(-10 * time.Minute), Landed: true}
			if f.host.endFinishedSandboxSeat(endRig, endPolecat, landing) {
				t.Fatal("a seat whose facts could not be read was removed")
			}
			records, _, err := readPendingSandboxSeatEnds(f.town)
			if err != nil {
				t.Fatalf("reading the pending ends: %v", err)
			}
			if got := len(records) > 0; got != tt.wantRecord {
				t.Errorf("a pending end is recorded = %v, want %v (%+v)", got, tt.wantRecord, records)
			}
		})
	}
}

// A recorded seat is offered to the end again on every pass: once the rules ask
// has happened, or the window has closed, the seat goes; while the ask is
// outstanding it stays; and a seat another fact now decides is dropped from the
// store instead of offered for ever (gt-381oj).
func TestRetryPendingSandboxSeatEnds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	submitted := now.Add(-10 * time.Minute)
	tests := []struct {
		name string
		// landingStamp is what the landing's own pass hands the end; zero is
		// submitted.
		landingStamp time.Time
		// change is the world between the two passes.
		change      func(f *seatEndFake, clk *clockwork.FakeClock)
		wantRemoved bool
		wantRecord  bool
		wantAsked   bool
	}{
		{
			name: "the cursor reached the submission",
			change: func(f *seatEndFake, _ *clockwork.FakeClock) {
				f.cursorAt = submitted.Add(time.Minute)
			},
			wantRemoved: true, wantAsked: true,
		},
		{
			name: "the window closed with the cursor behind",
			change: func(_ *seatEndFake, clk *clockwork.FakeClock) {
				clk.Advance(rulesPendingWindow + time.Minute)
			},
			wantRemoved: true, wantAsked: true,
		},
		{
			name:         "a stamp in the future waits only the window from now",
			landingStamp: now.Add(2 * time.Hour),
			change: func(_ *seatEndFake, clk *clockwork.FakeClock) {
				clk.Advance(rulesPendingWindow + time.Minute)
			},
			wantRemoved: true, wantAsked: true,
		},
		{
			name:       "the ask is still outstanding, so the seat waits",
			change:     func(*seatEndFake, *clockwork.FakeClock) {},
			wantRecord: true, wantAsked: true,
		},
		{
			name: "another fact now keeps the seat",
			change: func(f *seatEndFake, _ *clockwork.FakeClock) {
				f.cursorAt = submitted.Add(time.Minute)
				f.seatFields.CleanupStatus = string(polecat.CleanupUnpushed)
				f.setAgent()
			},
			wantAsked: true,
		},
		{
			name: "the agent bead is gone",
			change: func(f *seatEndFake, _ *clockwork.FakeClock) {
				if err := f.agentDB.DeleteIssues(f.agentBead); err != nil {
					f.t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, clk, _ := seatEndWaitingForRules(t, now)
			if !tt.landingStamp.IsZero() {
				// The landing's own pass again with the future stamp: the
				// record it leaves counts from the clamp, not the stamp.
				if f.host.endFinishedSandboxSeat(endRig, endPolecat, sandboxSeatLanding{
					BeadID: endBead, Branch: endBranch, Submitted: tt.landingStamp, Landed: true,
				}) {
					t.Fatal("a seat with a future stamp was removed")
				}
			}
			tt.change(f, clk)
			dirs := f.seatDirs()
			f.gwAsked = nil // the landing's own pass asked; count the retry's

			f.host.d.retryPendingSandboxSeatEnds()
			waitSeatEndCycles(t, f.host.d)

			if got := dirExists(t, dirs.PolecatDir); got == tt.wantRemoved {
				t.Errorf("seat directory exists = %v, want removed = %v", got, tt.wantRemoved)
			}
			records, bad, err := readPendingSandboxSeatEnds(f.town)
			if err != nil || len(bad) != 0 {
				t.Fatalf("reading the pending ends: %v, %v", err, bad)
			}
			if got := len(records) > 0; got != tt.wantRecord {
				t.Errorf("a pending end is recorded = %v, want %v (%+v)", got, tt.wantRecord, records)
			}
			if asked := len(f.gwAsked) > 0; asked != tt.wantAsked {
				t.Errorf("gateway asked = %v, want %v (%v)", asked, tt.wantAsked, f.gwAsked)
			}
		})
	}
}

// The heartbeat's step is what re-runs a kept seat (gt-381oj): the step the
// daemon schedules is the retry, and a seat whose cursor has passed is gone by
// the time the step returns.
func TestHeartbeatRetriesPendingSandboxSeatEnds(t *testing.T) {
	t.Parallel()
	var step *heartbeatStep
	for i := range heartbeatSteps {
		if heartbeatSteps[i].name == "sandbox-seat-ends" {
			step = &heartbeatSteps[i]
		}
	}
	if step == nil {
		t.Fatal("the heartbeat has no sandbox-seat-ends step")
	}
	if step.lifecycle {
		t.Error("sandbox-seat-ends must not be a lifecycle step: an E-stopped town still wants the seat gone")
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f, _, _ := seatEndWaitingForRules(t, now)
	f.cursorAt = now
	dirs := f.seatDirs()

	step.run(f.host.d)
	waitSeatEndCycles(t, f.host.d)

	if dirExists(t, dirs.PolecatDir) {
		t.Error("the heartbeat's seat-end step left a seat whose rules ask had happened")
	}
}

// The production cursor reader hands an unreadable cursor file back as an
// error, which the end logs and reads as not pending; the daemon's seam is
// only for tests (gt-381oj).
func TestReadRulesCursorReportsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	path := jevrules.CursorPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not a cursor"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.readRulesCursor(); err == nil || ok {
		t.Errorf("readRulesCursor = ok %v, err %v; want the parse error", ok, err)
	}
}

// A cursor never written is no ask yet, not an unreadable cursor: the
// production reader says so with no error, and the end reads it as not pending
// without a line (gt-381oj).
func TestReadRulesCursorReportsAMissingCursor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	if at, ok, err := d.readRulesCursor(); ok || err != nil {
		t.Errorf("readRulesCursor = (%v, %v, %v); want no cursor and no error", at, ok, err)
	}
}

// The pending store refuses a rig or polecat name a path cannot hold, on every
// one of its writes, and a store that cannot be listed is an error rather than
// an empty set of records (gt-381oj).
func TestPendingSandboxSeatEndStoreReportsFailures(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, bad := range []struct{ rig, name string }{
		{"../gastown", endPolecat},
		{endRig, "a/agate"},
	} {
		if _, err := pendingSandboxSeatEndPath(town, bad.rig, bad.name); err == nil {
			t.Errorf("a path was made for rig %q name %q", bad.rig, bad.name)
		}
		if err := writePendingSandboxSeatEnd(town, bad.rig, bad.name, sandboxSeatLanding{}); err == nil {
			t.Errorf("a record was written for rig %q name %q", bad.rig, bad.name)
		}
		if err := removePendingSandboxSeatEnd(town, bad.rig, bad.name); err == nil {
			t.Errorf("a record was removed for rig %q name %q", bad.rig, bad.name)
		}
	}
	// A store path that is a file cannot be listed: the pass must fail, not
	// read it as a store with no records.
	dir := pendingSandboxSeatEndsDir(town)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPendingSandboxSeatEnds(town); err == nil {
		t.Error("a store that is a file was read as an empty store")
	}
}

// A record that cannot be removed is reported rather than read as already
// gone: the next pass retries the end either way, so a stale record costs one
// extra run, never a wrong one (gt-381oj).
func TestRemovePendingSandboxSeatEndReportsAFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// The rig's directory position is a file, so the record under it cannot be
	// removed and the error is not the "no such record" that means success.
	rigFile := filepath.Join(pendingSandboxSeatEndsDir(town), endRig)
	if err := os.MkdirAll(filepath.Dir(rigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removePendingSandboxSeatEnd(town, endRig, endPolecat); err == nil {
		t.Error("removing a record under a file was reported as success")
	}
}

// A record that cannot be read is reported, and the records beside it are still
// offered: one corrupt file does not stop the pass or pass silently.
func TestRetryPendingSandboxSeatEndsReportsUnreadableRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f, _, _ := seatEndWaitingForRules(t, now)
	f.cursorAt = now
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	broken := filepath.Join(pendingSandboxSeatEndsDir(f.town), endRig, "broken.json")
	if err := os.WriteFile(broken, []byte("{not a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirs := f.seatDirs()

	f.host.d.retryPendingSandboxSeatEnds()
	waitSeatEndCycles(t, f.host.d)

	if dirExists(t, dirs.PolecatDir) {
		t.Error("the readable record beside the broken one was not offered")
	}
	if got := logs.String(); !strings.Contains(got, "cannot read the record "+broken) {
		t.Errorf("log %q does not report the unreadable record", got)
	}
}

// The landing worker's after-landing hook is the production path to a seat's
// end: the hook the rig's worker is built with reads the rig's landing record,
// so a landing the record does not yet prove keeps the seat and the one it
// proves ends it.
func TestLandingWorkerAfterLandedEndsTheSeat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "promote-"+endRig+".key")
	promoteRigConfig(t, townRoot, endRig, keyFile)
	writeRigsJSON(t, townRoot, []string{endRig})
	d := newForgejoPromoteDaemon(t, townRoot, endRig)
	var ended []sandboxSeatLanding
	d.endSandboxSeat = func(rig, name string, landing sandboxSeatLanding) bool {
		ended = append(ended, landing)
		return true
	}
	w, err := d.newRigLandingWorker(endRig)
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	if w.AfterLanded == nil {
		t.Fatal("the rig's landing worker has no after-landing hook")
	}
	const head = "0123456789abcdef"
	work := land.Work{BeadID: endBead, Head: head, Branch: endBranch, Worker: endPolecat}

	w.AfterLanded(work)
	waitSeatEndCycles(t, d)
	if len(ended) != 1 || ended[0].Landed {
		t.Fatalf("with no landing record: ended %+v; want one end that is unlanded", ended)
	}

	landings, err := land.RigLandingsFile(townRoot, endRig)
	if err != nil {
		t.Fatal(err)
	}
	if err := landings.Append(land.LandingRecord{BeadID: endBead, Head: head, Branch: endBranch}); err != nil {
		t.Fatal(err)
	}
	w.AfterLanded(work)
	waitSeatEndCycles(t, d)
	if len(ended) != 2 || !ended[1].Landed || ended[1].BeadID != endBead {
		t.Fatalf("with the landing record: ended %+v; want a second end that is landed", ended)
	}
}

// A daemon that is stopping starts no seat end, so the seat keeps its
// directories and no goroutine is left behind to race the shutdown.
func TestStartSandboxSeatEndDoesNotStartWhileStopping(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.ctx = ctx
	asked := false
	d.endSandboxSeat = func(string, string, sandboxSeatLanding) bool {
		asked = true
		return true
	}
	d.startSandboxSeatEnd(endRig, nil, land.Work{BeadID: endBead, Head: "h", Worker: endPolecat})
	waitSeatEndCycles(t, d)
	if asked {
		t.Error("a seat end started while the daemon was stopping")
	}
}

// A stop closes the door before it waits: a seat end started after the stop is
// refused, and the wait does not return until the ends already in flight have.
func TestStopSandboxSeatEndsRefusesNewEndsAndWaitsForTheOnesInFlight(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	d.endSandboxSeat = func(string, string, sandboxSeatLanding) bool {
		close(started)
		<-release
		close(finished)
		return true
	}
	landings := &land.LandingsFile{Path: filepath.Join(privateLandingsDir(t), "landings.jsonl")}
	if err := landings.Append(land.LandingRecord{BeadID: endBead, Head: "h"}); err != nil {
		t.Fatal(err)
	}
	work := land.Work{BeadID: endBead, Head: "h", Worker: endPolecat}
	d.startSandboxSeatEnd(endRig, landings, work)
	waitClosed(t, started, "the seat end to start")

	d.beginSandboxSeatEndStop()
	asked := make(chan struct{}, 1)
	d.endSandboxSeat = func(string, string, sandboxSeatLanding) bool {
		asked <- struct{}{}
		return false
	}
	d.startSandboxSeatEnd(endRig, landings, work)

	waited := make(chan struct{})
	go func() {
		d.waitForSandboxSeatEnds()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("the wait returned while a seat end was still running")
	default:
	}
	close(release)
	waitClosed(t, waited, "the stop wait to return")
	select {
	case <-finished:
	default:
		t.Error("the wait returned before the seat end in flight finished")
	}
	select {
	case <-asked:
		t.Error("a seat end started after the stop had closed the door")
	default:
	}
}

// seatEndTestBound is how long a test waits on a seat end before it fails. It is
// a backstop that turns a hang into a failure with a message, never a delay a
// test relies on: every wait below returns the moment the thing it waits for
// happens.
const seatEndTestBound = 30 * time.Second

// waitClosed fails the test, instead of hanging it, when ch is not closed
// within seatEndTestBound.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), seatEndTestBound)
	defer cancel()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatalf("waiting for %s: still blocked after %s", what, seatEndTestBound)
	}
}

// waitSeatEndCycles waits for the seat ends d started, bounded the same way.
func waitSeatEndCycles(t *testing.T, d *Daemon) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		d.sandboxSeatEndCycles.Wait()
		close(done)
	}()
	waitClosed(t, done, "the seat-end cycles")
}
