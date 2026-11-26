package daemon

import (
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
)

// The branch the seat landed on and the other bead it was slung to afterwards.
const (
	movedOnLandedBead = "gt-hx8f5i"
	movedOnOtherBead  = "gt-lbyw8q"
	movedOnLanded     = "polecat/" + endPolecat + "/" + movedOnLandedBead + "+x1"
	movedOnOther      = "polecat/" + endPolecat + "/" + movedOnOtherBead + "+x1"
)

// movedOnRec is the kept record the branch guard writes for a seat whose work
// landed, with the agent bead's branch left to the row.
func movedOnRec() keptSandboxSeatRecord {
	return keptSandboxSeatRecord{
		Bead:    movedOnLandedBead,
		Reason:  seatWorksKeepPrefix + movedOnOther + ", not the landed " + movedOnLanded,
		Since:   time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC),
		Landing: &sandboxSeatLanding{BeadID: movedOnLandedBead, Branch: movedOnLanded, Landed: true},
	}
}

// A keep is the branch guard's for work that landed only. Every other record
// the pass may meet — an unlanded landing, a keep of another reason, and a
// record written before the landing facts were kept — is not one, and is left
// alone (gt-i72us8).
func TestMovedOnKeep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rec  func() keptSandboxSeatRecord
		want bool
	}{
		{name: "a landed keep", rec: movedOnRec, want: true},
		{name: "an unlanded landing", want: false, rec: func() keptSandboxSeatRecord {
			rec := movedOnRec()
			rec.Landing.Landed = false
			return rec
		}},
		{name: "a keep for another reason", want: false, rec: func() keptSandboxSeatRecord {
			rec := movedOnRec()
			rec.Reason = keptSeatReason
			return rec
		}},
		{name: "no landing facts", want: false, rec: func() keptSandboxSeatRecord {
			rec := movedOnRec()
			rec.Landing = nil
			return rec
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := movedOnKeep(tt.rec()); got != tt.want {
				t.Errorf("movedOnKeep = %v, want %v", got, tt.want)
			}
		})
	}
}

// movedOnSeat names the bead that landed exactly when the kept record is the
// branch guard's landed keep and the seat's agent bead works another bead: not
// for an absent or other record, a seat taken over onto the landed bead, a
// missing or branchless agent bead, and never for a record or agent bead that
// cannot be read. An unreadable agent bead is an error, not a keep, so a seat
// whose beacon cannot be read is never silently retained (gt-i72us8).
func TestMovedOnSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// branch is the agent bead's branch; the default is another bead.
		branch string
		// noAgentBead leaves the seat's agent bead uncreated.
		noAgentBead bool
		// badRecord writes an unreadable record instead of the landed keep.
		badRecord bool
		// absentRecord writes no record at all.
		absentRecord bool
		// notKeep seeds a record the branch guard did not write.
		notKeep bool
		// failShow makes the agent bead read fail.
		failShow    error
		wantMovedOn bool
		wantLanded  string
		wantErr     string
	}{
		{name: "an absent record", absentRecord: true, branch: movedOnOther},
		{name: "another keep", notKeep: true, branch: movedOnOther},
		{name: "a seat on another bead", branch: movedOnOther, wantMovedOn: true, wantLanded: movedOnLandedBead},
		{name: "a seat on the landed bead", branch: movedOnLanded, wantLanded: movedOnLandedBead},
		{name: "a record that cannot be read", badRecord: true, branch: movedOnOther,
			wantErr: "cannot re-read its kept record"},
		{name: "an agent bead that cannot be read", branch: movedOnOther,
			failShow: errors.New("store unavailable"), wantErr: "cannot read its agent bead"},
		{name: "an absent agent bead", noAgentBead: true, branch: movedOnOther, wantLanded: movedOnLandedBead},
		{name: "an agent bead with an empty branch", branch: "", wantLanded: movedOnLandedBead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newKeptSeatPatrol(t, time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC))
			path, err := keptSandboxSeatPath(p.town, endRig, endPolecat)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.absentRecord {
				rec := movedOnRec()
				if tt.notKeep {
					rec.Reason = keptSeatReason
				}
				if err := writeKeptSandboxSeat(p.town, endRig, endPolecat, rec); err != nil {
					t.Fatal(err)
				}
			}
			if tt.badRecord {
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if !tt.noAgentBead {
				p.seatFields.Branch = tt.branch
				p.seedAgent()
			}
			if tt.failShow != nil {
				p.agentDB.FailWith("show", tt.failShow)
			}

			movedOn, landed, err := p.host.movedOnSeat(endRig, endPolecat, path)

			if movedOn != tt.wantMovedOn {
				t.Errorf("movedOnSeat movedOn = %v, want %v (landed %q, err %v)", movedOn, tt.wantMovedOn, landed, err)
			}
			if landed != tt.wantLanded {
				t.Errorf("movedOnSeat landed = %q, want %q", landed, tt.wantLanded)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("movedOnSeat error = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("movedOnSeat error = %v, want one carrying %q", err, tt.wantErr)
			}
		})
	}
}

// movedOnPatrol seeds the fixture seat's kept record with the branch guard's
// reason and its agent bead with branch, and returns the patrol and the record
// path.
func movedOnPatrol(t *testing.T, branch string, landed bool) (*keptSeatPatrol, string) {
	t.Helper()
	p := newKeptSeatPatrol(t, time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC))
	p.seatFields.Branch = branch
	p.seedAgent()
	seedMovedOnRecord(t, p, seatWorksKeepPrefix+branch+", not the landed "+movedOnLanded, landed)
	path, err := keptSandboxSeatPath(p.town, endRig, endPolecat)
	if err != nil {
		t.Fatal(err)
	}
	return p, path
}

func seedMovedOnRecord(t *testing.T, p *keptSeatPatrol, reason string, landed bool) {
	t.Helper()
	rec := keptSandboxSeatRecord{
		Bead: movedOnLandedBead, Reason: reason, Since: p.clk.Now().Add(-time.Hour),
		Landing: &sandboxSeatLanding{BeadID: movedOnLandedBead, Branch: movedOnLanded, Landed: landed},
	}
	if err := writeKeptSandboxSeat(p.town, endRig, endPolecat, rec); err != nil {
		t.Fatal(err)
	}
}

// The pass drops a kept record whose seat has moved on to another bead, with
// the line naming the bead that landed. Every other answer keeps the record or
// leaves it for a later pass: a takeover onto the landed bead, a keep of
// another kind, an unlanded landing, a record or agent bead that cannot be
// read, a busy seat lock, a failed removal, and the same facts re-judged under
// the lock (gt-i72us8).
func TestPatrolScanDropsMovedOnKeptSeat(t *testing.T) {
	t.Parallel()
	dropLine := "patrol_scan: kept record of " + endRig + "/" + endPolecat +
		" dropped: the seat works another bead than the landed " + movedOnLandedBead
	tests := []struct {
		name string
		// branch is the agent bead's branch; "" is another bead than the landed.
		branch string
		// unlanded seeds the record with a landing that did not land.
		unlanded bool
		// passes is how many times the pass runs; 1 when zero.
		passes     int
		setup      func(t *testing.T, p *keptSeatPatrol)
		lock       func(t *testing.T, p *keptSeatPatrol, path string, fn func() error) error
		wantRecord bool
		wantLog    string
	}{
		{name: "moved on to another bead", wantRecord: false, wantLog: dropLine},
		{name: "taken over onto the landed bead", branch: movedOnLanded, wantRecord: true},
		{name: "a keep that is not the branch guard's", wantRecord: true,
			setup: func(t *testing.T, p *keptSeatPatrol) { seedMovedOnRecord(t, p, keptSeatReason, true) }},
		{name: "an unlanded landing", unlanded: true, wantRecord: true},
		{name: "an agent bead that cannot be read", passes: 2, wantRecord: true,
			setup:   func(_ *testing.T, p *keptSeatPatrol) { p.agentDB.FailWith("show", errors.New("store unavailable")) },
			wantLog: "kept record of " + endRig + "/" + endPolecat + " kept: cannot read its agent bead: store unavailable"},
		{name: "a removal that fails", wantRecord: true,
			setup: func(_ *testing.T, p *keptSeatPatrol) {
				p.host.removeKeptSeat = func(string, string, string) error { return errors.New("registry is read-only") }
			},
			wantLog: "kept: its record cannot be removed: registry is read-only"},
		{name: "a busy seat lock", wantRecord: true,
			lock: func(*testing.T, *keptSeatPatrol, string, func() error) error {
				return polecat.ErrSandboxSeatLockBusy
			},
			wantLog: "kept: polecat directory lock is busy"},
		{name: "a takeover under the lock", wantRecord: true,
			lock: func(t *testing.T, p *keptSeatPatrol, _ string, fn func() error) error {
				p.seatFields.Branch = movedOnLanded
				p.setAgent()
				return fn()
			}},
		{name: "a record that cannot be re-read under the lock", wantRecord: true,
			lock: func(_ *testing.T, _ *keptSeatPatrol, path string, fn func() error) error {
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					return err
				}
				return fn()
			},
			wantLog: "kept: cannot re-read its kept record"},
		{name: "an agent bead that cannot be read under the lock", wantRecord: true,
			lock: func(_ *testing.T, p *keptSeatPatrol, _ string, fn func() error) error {
				p.agentDB.FailWith("show", errors.New("store unavailable"))
				defer p.agentDB.FailWith("show", nil)
				return fn()
			},
			wantLog: "kept: cannot read its agent bead: store unavailable"},
		{name: "a kept-seat store that cannot be listed", wantRecord: false,
			setup: func(t *testing.T, p *keptSeatPatrol) {
				dir := keptSandboxSeatsDir(p.town)
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: "the kept-seat records cannot be read, so none is dropped"},
		{name: "a record dropped under the lock first", wantRecord: false,
			lock: func(t *testing.T, p *keptSeatPatrol, _ string, fn func() error) error {
				if err := removeKeptSandboxSeat(p.town, endRig, endPolecat); err != nil {
					t.Fatal(err)
				}
				return fn()
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			branch := tt.branch
			if branch == "" {
				branch = movedOnOther
			}
			p, path := movedOnPatrol(t, branch, !tt.unlanded)
			if tt.setup != nil {
				tt.setup(t, p)
			}
			if tt.lock != nil {
				p.host.sandboxSeatLock = func(_, _ string, fn func() error) error { return tt.lock(t, p, path, fn) }
			}
			var logs strings.Builder
			p.host.d.logger = log.New(&logs, "", 0)
			for i := 0; i < max(tt.passes, 1); i++ {
				p.host.dropMovedOnKeptSeats()
			}

			if _, err := os.Stat(path); (err == nil) != tt.wantRecord {
				t.Errorf("record present = %v, want %v (stat: %v)", err == nil, tt.wantRecord, err)
			}
			if tt.wantLog == "" && strings.Contains(logs.String(), "patrol_scan: kept record of") {
				t.Errorf("the pass logged a line the row does not expect:\n%s", logs.String())
			}
			if tt.wantLog != "" {
				if got := strings.Count(logs.String(), tt.wantLog); got != 1 {
					t.Errorf("log carries %q %d times, want once:\n%s", tt.wantLog, got, logs.String())
				}
			}
		})
	}
}
