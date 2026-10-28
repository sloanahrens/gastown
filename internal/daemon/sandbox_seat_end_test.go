package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
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
	t          *testing.T
	town       string
	agentBead  string
	agentDB    *beadsfake.Fake
	host       *patrolScanHost
	removals   []string
	removeErr  error
	gwAsked    []string
	seatFields beads.AgentFields
}

func newSeatEndFake(t *testing.T) *seatEndFake {
	t.Helper()
	town := t.TempDir()
	writeTownFile(t, town, "settings/config.json", endSettings)
	f := &seatEndFake{
		t:         t,
		town:      town,
		agentDB:   beadsfake.New(),
		seatFields: beads.AgentFields{
			RoleType: "polecat", Rig: endRig, Agent: endSandboxIn,
			Branch: endBranch, CleanupStatus: string(polecat.CleanupClean),
		},
	}
	f.agentBead = beads.AgentBeadIDFor(constants.RolePolecat, endRig, endPolecat, town)
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	f.host = &patrolScanHost{
		d:                 d,
		openRecoveryBeads: func([]string) beads.Client { return f.agentDB },
		sandboxExists: func(gateway, name string) (bool, error) {
			f.gwAsked = append(f.gwAsked, gateway+"|"+name)
			return false, nil
		},
		removeSandboxSeatDirs: func(rigPath, rig, name string) error {
			if f.removeErr != nil {
				return f.removeErr
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

// seatDirs creates the host directories a sandbox seat leaves.
func (f *seatEndFake) seatDirs() polecat.SandboxSeatHostDirs {
	f.t.Helper()
	dirs := polecat.SandboxSeatHostDirsFor(filepath.Join(f.town, endRig), endRig, endPolecat)
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
			name: "unlanded work keeps it",
			landing: sandboxSeatLanding{BeadID: endBead, Branch: endBranch}, desired: intent.DesiredStop,
			wantAsked: true,
		},
		{
			name: "a branch that never reached origin keeps it",
			fields: func(f *beads.AgentFields) { f.CleanupStatus = string(polecat.CleanupUnpushed) },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name: "a missing cleanup status keeps it",
			fields: func(f *beads.AgentFields) { f.CleanupStatus = "" },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name: "a recorded push failure keeps it",
			fields: func(f *beads.AgentFields) { f.PushFailed = true },
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
			name: "a hook still set keeps it",
			fields: func(f *beads.AgentFields) { f.HookBead = endBead },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name: "another branch keeps it",
			fields: func(f *beads.AgentFields) { f.Branch = "polecat/agate/gt-other+x1" },
			landing: finished, desired: intent.DesiredStop, wantAsked: true,
		},
		{
			name: "a seat something still wants keeps it",
			landing: finished, desired: intent.DesiredRun, wantAsked: true,
		},
		{
			name: "a parked seat keeps it",
			landing: finished, parked: true, wantAsked: true,
		},
		{
			name: "a tmux seat keeps its worktree",
			fields: func(f *beads.AgentFields) { f.Agent = endHostAgent },
			landing: finished, desired: intent.DesiredStop,
		},
		{
			name: "a failed removal keeps it",
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

// A polecat with no agent bead is not a seat this town tracks: nothing is read
// from the gateway and nothing is removed.
func TestEndFinishedSandboxSeatWithoutAnAgentBead(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
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
				f := &land.LandingsFile{Path: filepath.Join(t.TempDir(), "landings.jsonl")}
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
				return &land.LandingsFile{Path: filepath.Join(t.TempDir(), "landings.jsonl")}
			},
			wantCalled: true, wantLanded: false, wantLandedB: endBranch,
		},
		{
			name: "a crew landing has no seat",
			work: land.Work{BeadID: endBead, Head: head, Branch: endBranch},
			landings: func(t *testing.T) *land.LandingsFile {
				t.Helper()
				return &land.LandingsFile{Path: filepath.Join(t.TempDir(), "landings.jsonl")}
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
				return &land.LandingsFile{Path: t.TempDir()}
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
		{"hook set", func(e *sandboxSeatEnd) { e.HookBead = endBead }, "hook_bead=" + endBead, false},
		{"another branch", func(e *sandboxSeatEnd) { e.Branch = "polecat/agate/gt-other+x1" }, "not the landed", false},
		{"still wanted", func(e *sandboxSeatEnd) { e.Retired = false }, "does not say stop", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := finished
			tt.mut(&e)
			if why := e.keepReason(); !strings.Contains(why, tt.want) {
				t.Errorf("keepReason = %q, want it to name %q", why, tt.want)
			}
		})
	}
}
