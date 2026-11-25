package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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
