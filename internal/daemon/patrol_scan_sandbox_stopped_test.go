package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// stoppedStatus answers the gateway's phase question with Stopped.
func stoppedStatus(string, string) (sandbox.Status, error) {
	return sandbox.Status{Phase: "Stopped"}, nil
}

// keepSeat writes the kept record the seat's end leaves for a keep with reason,
// carrying landing as the seat's landing facts.
func (p *keptSeatPatrol) keepSeat(t *testing.T, reason string, landing sandboxSeatLanding) {
	t.Helper()
	rec := keptSandboxSeatRecord{Bead: landing.BeadID, Reason: reason, Since: p.clk.Now(), Landing: &landing}
	if err := writeKeptSandboxSeat(p.town, endRig, endPolecat, rec); err != nil {
		t.Fatal(err)
	}
}

// keptSeatOf reads the kept record of the test seat, failing the test when there
// is none.
func (p *keptSeatPatrol) keptSeatOf(t *testing.T) keptSandboxSeatRecord {
	t.Helper()
	path, err := keptSandboxSeatPath(p.town, endRig, endPolecat)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := readKeptSandboxSeatRecord(path)
	if err != nil {
		t.Fatalf("reading the kept record: %v", err)
	}
	return rec
}

// keptSeatItems is the attention collector's kept-seat items at now.
func keptSeatItems(t *testing.T, d *Daemon, now time.Time) []string {
	t.Helper()
	src := &attentionSources{d: d, now: now}
	items, err := src.collectKeptSandboxSeats(context.Background())
	if err != nil {
		t.Fatalf("collecting the kept-seat attention items: %v", err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Summary)
	}
	return out
}

// newStoppedSeatPatrol is a finished seat whose sandbox is up in the Stopped
// phase, whose seat's end recorded the landing facts, and whose registry record
// the patrol finds old enough to judge.
func newStoppedSeatPatrol(t *testing.T, landed bool) *keptSeatPatrol {
	t.Helper()
	now := time.Date(2026, 11, 12, 18, 12, 0, 0, time.UTC)
	p := newKeptSeatPatrol(t, now)
	p.seedAgent()
	p.retire(intent.DesiredStop)
	p.seatDirs()
	p.sandboxUp = true
	p.registryRecord(t)
	p.host.sandboxStatus = stoppedStatus
	landing := keptSeatLanding()
	landing.Landed = landed
	p.keepSeat(t, "cannot ask its sandbox for its ended marker: sandbox is not ready (phase: Stopped)", landing)
	return p
}

// A Stopped sandbox of a seat whose end is recorded, and whose landing proves
// the work landed, is deleted by the record pass on its phase: a Stopped sandbox
// cannot answer the ended-marker question, so the marker is never asked. The
// warning names the seat, and the record goes with the sandbox (gt-7h38pd).
func TestPatrolDeletesAStoppedSandboxOfALandedEndedSeat(t *testing.T) {
	t.Parallel()
	p := newStoppedSeatPatrol(t, true)
	var markerAsked []string
	p.host.sandboxEndedMarker = func(_, name string) (bool, error) {
		markerAsked = append(markerAsked, name)
		return false, nil
	}
	var logs strings.Builder
	p.host.d.logger = log.New(&logs, "", 0)

	p.host.patrolScanSandboxRecords()

	if len(markerAsked) != 0 {
		t.Errorf("ended marker asked of a Stopped sandbox: %v, want no marker read", markerAsked)
	}
	if want := []string{session.PolecatSessionName(session.DefaultPrefix, endPolecat)}; !reflect.DeepEqual(p.deleted, want) {
		t.Fatalf("sandboxes deleted = %v, want %v", p.deleted, want)
	}
	if p.recorded() {
		t.Error("the registry record survived the deleted sandbox")
	}
	if got := logs.String(); !strings.Contains(got, "warning") || !strings.Contains(got, "Stopped") || !strings.Contains(got, endRig+"/"+endPolecat) {
		t.Errorf("log %q does not warn about the Stopped sandbox of %s/%s", got, endRig, endPolecat)
	}
}

// A Stopped sandbox whose work is not proven landed stays, and its kept record
// carries the stopped, work not proven landed reason, which the kept-seat item
// raises once the seat has been kept for the attention window (gt-7h38pd).
func TestPatrolKeepsAStoppedSandboxWithUnlandedWork(t *testing.T) {
	t.Parallel()
	p := newStoppedSeatPatrol(t, false)
	var markerAsked []string
	p.host.sandboxEndedMarker = func(_, name string) (bool, error) {
		markerAsked = append(markerAsked, name)
		return false, nil
	}

	p.host.patrolScanSandboxRecords()

	if len(markerAsked) != 0 {
		t.Errorf("ended marker asked of a Stopped sandbox: %v, want no marker read", markerAsked)
	}
	if len(p.deleted) != 0 {
		t.Fatalf("sandboxes deleted = %v, want the unlanded sandbox kept", p.deleted)
	}
	if !p.sandboxUp {
		t.Error("the gateway's sandbox is gone, want the unlanded sandbox still up")
	}
	if !p.recorded() {
		t.Error("the registry record was removed for a sandbox that stays")
	}
	if got := p.keptSeatOf(t).Reason; got != stoppedUnlandedReason {
		t.Errorf("kept reason = %q, want %q", got, stoppedUnlandedReason)
	}
	p.clk.Advance(keptSeatAttentionAfter + time.Minute)
	items := keptSeatItems(t, p.host.d, p.clk.Now())
	if len(items) != 1 {
		t.Fatalf("kept-seat items = %v, want one", items)
	}
	if !strings.Contains(items[0], stoppedUnlandedReason) {
		t.Errorf("kept-seat item %q does not carry the reason %q", items[0], stoppedUnlandedReason)
	}
}

// The record pass refuses a Stopped sandbox it cannot tie to a landed, ended
// seat, and says why. Each row keeps the sandbox and its record. A deleter that
// fails keeps them too, and the deleter is asked only once the seat's end and
// its landing facts are known (gt-7h38pd).
func TestPatrolKeepsAStoppedSandboxItCannotTieToALandedEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		setup       func(t *testing.T, p *keptSeatPatrol)
		deleterErr  error
		noDeleter   bool
		wantDeleter int
		wantLog     string
	}{
		{
			name: "no kept record",
			setup: func(t *testing.T, p *keptSeatPatrol) {
				path, err := keptSandboxSeatPath(p.town, endRig, endPolecat)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: "its seat's end is not recorded",
		},
		{
			name: "a kept record without landing facts",
			setup: func(t *testing.T, p *keptSeatPatrol) {
				rec := keptSandboxSeatRecord{Reason: "cannot ask its sandbox for its ended marker", Since: p.clk.Now()}
				if err := writeKeptSandboxSeat(p.town, endRig, endPolecat, rec); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: "names no landing facts",
		},
		{
			name: "an unreadable kept record",
			setup: func(t *testing.T, p *keptSeatPatrol) {
				path, err := keptSandboxSeatPath(p.town, endRig, endPolecat)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: "its kept record cannot be read",
		},
		{
			name:      "no deleter is linked",
			noDeleter: true,
			setup:     func(t *testing.T, p *keptSeatPatrol) {},
			wantLog:   "no sandbox deleter is linked",
		},
		{
			name:        "the deleter fails",
			deleterErr:  errors.New("disk full"),
			setup:       func(t *testing.T, p *keptSeatPatrol) {},
			wantDeleter: 1,
			wantLog:     "its stopped sandbox could not be deleted: disk full",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newStoppedSeatPatrol(t, true)
			tt.setup(t, p)
			var logs strings.Builder
			p.host.d.logger = log.New(&logs, "", 0)
			if tt.noDeleter {
				p.host.sandboxDeleter = nil
				p.host.noSandboxDeleterLinked = true
			} else {
				p.host.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
					p.deleted = append(p.deleted, rec.Session)
					if tt.deleterErr != nil {
						return tt.deleterErr
					}
					p.sandboxUp = false
					return nil
				}
			}

			p.host.patrolScanSandboxRecords()

			if len(p.deleted) != tt.wantDeleter {
				t.Errorf("deleter calls = %v, want %d", p.deleted, tt.wantDeleter)
			}
			if !p.recorded() {
				t.Error("the registry record was removed for a sandbox that stays")
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log %q does not name %q", logs.String(), tt.wantLog)
			}
		})
	}
}

// A gateway that does not answer is retried on every tick. The kept record counts
// each silent tick and names the time of the last try, and the answer on the
// fourth tick deletes the Stopped sandbox. Six silent ticks raise the seat's
// kept-seat item once, whatever its age (gt-7h38pd).
func TestGatewayThatDoesNotAnswerIsRetriedEveryTick(t *testing.T) {
	t.Parallel()
	t.Run("three silent ticks then an answer delete the sandbox on the fourth", func(t *testing.T) {
		t.Parallel()
		p := newStoppedSeatPatrol(t, true)
		silent := 3
		p.host.sandboxExists = func(string, string) (bool, error) {
			if silent > 0 {
				silent--
				return false, errors.New("signal: killed")
			}
			return p.sandboxUp, nil
		}
		for tick := 1; tick <= 3; tick++ {
			p.clk.Advance(time.Minute)
			p.host.patrolScanSandboxRecords()
			if len(p.deleted) != 0 {
				t.Fatalf("tick %d: sandboxes deleted = %v, want none while the gateway is silent", tick, p.deleted)
			}
			kept := p.keptSeatOf(t)
			if kept.Tries != tick {
				t.Errorf("tick %d: tries = %d, want %d", tick, kept.Tries, tick)
			}
			if want := gatewayNoAnswerKeepReason(p.clk.Now()); kept.Reason != want {
				t.Errorf("tick %d: reason = %q, want %q", tick, kept.Reason, want)
			}
		}
		p.clk.Advance(time.Minute)
		p.host.patrolScanSandboxRecords()
		if want := []string{session.PolecatSessionName(session.DefaultPrefix, endPolecat)}; !reflect.DeepEqual(p.deleted, want) {
			t.Fatalf("sandboxes deleted after the answer = %v, want %v", p.deleted, want)
		}
		if p.recorded() {
			t.Error("the registry record survived the deleted sandbox")
		}
	})

	t.Run("six silent ticks raise the kept-seat item once", func(t *testing.T) {
		t.Parallel()
		p := newStoppedSeatPatrol(t, true)
		p.host.sandboxExists = func(string, string) (bool, error) {
			return false, errors.New("signal: killed")
		}
		for tick := 1; tick <= 5; tick++ {
			p.clk.Advance(time.Minute)
			p.host.patrolScanSandboxRecords()
		}
		if items := keptSeatItems(t, p.host.d, p.clk.Now()); len(items) != 0 {
			t.Fatalf("kept-seat items after five silent ticks = %v, want none", items)
		}
		p.clk.Advance(time.Minute)
		p.host.patrolScanSandboxRecords()
		items := keptSeatItems(t, p.host.d, p.clk.Now())
		if len(items) != 1 {
			t.Fatalf("kept-seat items after six silent ticks = %v, want one", items)
		}
		if !strings.Contains(items[0], gatewayNoAnswerKeepReason(p.clk.Now())) {
			t.Errorf("kept-seat item %q does not name the last try", items[0])
		}
		if n := len(keptSeatItems(t, p.host.d, p.clk.Now())); n != 1 {
			t.Errorf("kept-seat items on a second read = %d, want the same one", n)
		}
		if len(p.deleted) != 0 {
			t.Errorf("sandboxes deleted = %v, want none while the gateway is silent", p.deleted)
		}
	})

	t.Run("the retry limit is the daemon's to set", func(t *testing.T) {
		t.Parallel()
		p := newStoppedSeatPatrol(t, true)
		p.host.d.sandboxGatewayRetryLimit = 2
		p.host.sandboxExists = func(string, string) (bool, error) {
			return false, errors.New("signal: killed")
		}
		for tick := 1; tick <= 2; tick++ {
			p.clk.Advance(time.Minute)
			p.host.patrolScanSandboxRecords()
		}
		if items := keptSeatItems(t, p.host.d, p.clk.Now()); len(items) != 1 {
			t.Fatalf("kept-seat items after two silent ticks at limit 2 = %v, want one", items)
		}
	})

	t.Run("a seat whose end is not recorded counts no tick", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 11, 12, 18, 12, 0, 0, time.UTC)
		p := newKeptSeatPatrol(t, now)
		p.seedAgent()
		p.retire(intent.DesiredStop)
		p.seatDirs()
		p.sandboxUp = true
		p.registryRecord(t)
		p.host.sandboxExists = func(string, string) (bool, error) {
			return false, errors.New("signal: killed")
		}
		for tick := 1; tick <= 6; tick++ {
			p.clk.Advance(time.Minute)
			p.host.patrolScanSandboxRecords()
		}
		if seats, _, err := KeptSandboxSeats(p.town); err != nil || len(seats) != 0 {
			t.Errorf("kept seats = %+v, %v; want none for a seat whose end is not recorded", seats, err)
		}
		if !p.recorded() {
			t.Error("the registry record was removed while the gateway was silent")
		}
	})
}

// The landing's own question to a silent gateway keeps the seat with its landing
// facts, so the patrol can retry the gateway and finish the end. The landing is
// the first try, not a tick, so the record starts with no tries (gt-7h38pd).
func TestLandingKeepsASeatWhoseGatewayDoesNotAnswer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 18, 12, 0, 0, time.UTC)
	p := newKeptSeatPatrol(t, now)
	p.seedAgent()
	p.retire(intent.DesiredStop)
	p.seatDirs()
	p.sandboxUp = true
	p.host.sandboxExists = func(string, string) (bool, error) {
		return false, errors.New("signal: killed")
	}

	if p.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a seat whose gateway did not answer was removed")
	}

	kept := p.keptSeatOf(t)
	if want := gatewayNoAnswerKeepReason(now); kept.Reason != want {
		t.Errorf("kept reason = %q, want %q", kept.Reason, want)
	}
	if kept.Tries != 0 {
		t.Errorf("tries = %d, want 0 for the landing's own question", kept.Tries)
	}
	if kept.Landing == nil || kept.Landing.BeadID != endBead || !kept.Landing.Landed {
		t.Errorf("landing facts = %+v, want the seat's landed facts", kept.Landing)
	}
	if !dirExists(t, filepath.Join(p.town, endRig, "polecats", endPolecat)) {
		t.Error("the seat's host directories were removed while the gateway was silent")
	}
}

// Once the gateway answers, a gateway keep is replaced by the reason the answer
// gives: a seat whose gateway was silent and whose sandbox answers, running, with
// no ended marker, is kept for the marker, not for the silent gateway (gt-7h38pd).
func TestGatewayKeepIsReplacedByTheReasonTheAnswerGives(t *testing.T) {
	t.Parallel()
	p := newStoppedSeatPatrol(t, true)
	p.host.sandboxStatus = func(string, string) (sandbox.Status, error) {
		return sandbox.Status{Phase: "Ready"}, nil
	}
	p.host.sandboxEndedMarker = func(string, string) (bool, error) { return false, nil }
	silent := true
	p.host.sandboxExists = func(string, string) (bool, error) {
		if silent {
			return false, errors.New("signal: killed")
		}
		return p.sandboxUp, nil
	}
	p.clk.Advance(time.Minute)
	p.host.patrolScanSandboxRecords()
	if got := p.keptSeatOf(t).Reason; !isGatewayNoAnswerKeep(got) {
		t.Fatalf("kept reason after a silent tick = %q, want a gateway keep", got)
	}

	silent = false
	p.clk.Advance(time.Minute)
	p.host.patrolScanSandboxRecords()

	kept := p.keptSeatOf(t)
	if want := "its ended marker is not set"; kept.Reason != want {
		t.Errorf("kept reason after the answer = %q, want %q", kept.Reason, want)
	}
	if kept.Tries != 0 {
		t.Errorf("tries after the answer = %d, want 0", kept.Tries)
	}
	if len(p.deleted) != 0 {
		t.Errorf("sandboxes deleted = %v, want the running sandbox kept", p.deleted)
	}
}

// A kept record the patrol cannot read is never rewritten by a gateway tick or an
// answer, and a landing that cannot read it dates the keep from now. A record
// that is readable but names no landing facts is not counted (gt-7h38pd).
func TestGatewayKeepLeavesAnUnusableKeptRecordAlone(t *testing.T) {
	t.Parallel()
	silentGateway := func(p *keptSeatPatrol) {
		p.host.sandboxExists = func(string, string) (bool, error) {
			return false, errors.New("signal: killed")
		}
	}
	answeringMarkerUnset := func(p *keptSeatPatrol) {
		p.sandboxUp = true
		p.host.sandboxStatus = func(string, string) (sandbox.Status, error) {
			return sandbox.Status{Phase: "Ready"}, nil
		}
		p.host.sandboxEndedMarker = func(string, string) (bool, error) { return false, nil }
	}
	corrupt := func(t *testing.T, p *keptSeatPatrol) {
		path, err := keptSandboxSeatPath(p.town, endRig, endPolecat)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		// setup makes the kept record and the gateway answer, then run is one pass.
		setup   func(t *testing.T, p *keptSeatPatrol)
		run     func(p *keptSeatPatrol)
		wantLog string
		// wantUnread is whether the kept record is still unreadable afterwards.
		wantUnread bool
	}{
		{
			name:       "a silent tick does not count an unreadable record",
			setup:      func(t *testing.T, p *keptSeatPatrol) { corrupt(t, p); silentGateway(p) },
			run:        func(p *keptSeatPatrol) { p.host.patrolScanSandboxRecords() },
			wantLog:    "its kept record cannot be read, so the gateway try is not counted",
			wantUnread: true,
		},
		{
			name: "an answer does not settle an unreadable record",
			setup: func(t *testing.T, p *keptSeatPatrol) {
				corrupt(t, p)
				answeringMarkerUnset(p)
			},
			run:        func(p *keptSeatPatrol) { p.host.patrolScanSandboxRecords() },
			wantLog:    "its gateway keep is not settled",
			wantUnread: true,
		},
		{
			name: "a landing dates a keep it cannot read from now",
			setup: func(t *testing.T, p *keptSeatPatrol) {
				p.seedAgent()
				p.retire(intent.DesiredStop)
				p.seatDirs()
				p.sandboxUp = true
				corrupt(t, p)
				silentGateway(p)
			},
			run: func(p *keptSeatPatrol) {
				p.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())
			},
			wantLog: "its kept record is unreadable, so the gateway keep is dated from now",
		},
		{
			name: "a readable record without landing facts is not counted",
			setup: func(t *testing.T, p *keptSeatPatrol) {
				rec := keptSandboxSeatRecord{Reason: "cannot ask its sandbox for its ended marker", Since: p.clk.Now()}
				if err := writeKeptSandboxSeat(p.town, endRig, endPolecat, rec); err != nil {
					t.Fatal(err)
				}
				silentGateway(p)
			},
			run: func(p *keptSeatPatrol) { p.host.patrolScanSandboxRecords() },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 12, 18, 12, 0, 0, time.UTC)
			p := newKeptSeatPatrol(t, now)
			p.registryRecord(t)
			tt.setup(t, p)
			var logs strings.Builder
			p.host.d.logger = log.New(&logs, "", 0)

			p.clk.Advance(time.Minute)
			tt.run(p)

			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log %q does not say %q", logs.String(), tt.wantLog)
			}
			path, err := keptSandboxSeatPath(p.town, endRig, endPolecat)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := readKeptSandboxSeatRecord(path)
			if unread := readErr != nil; unread != tt.wantUnread {
				t.Errorf("kept record unreadable after the pass = %v (%v), want %v", unread, readErr, tt.wantUnread)
			}
			if !tt.wantUnread && tt.wantLog == "" {
				if got := p.keptSeatOf(t); got.Tries != 0 {
					t.Errorf("tries = %d, want the record without landing facts left uncounted", got.Tries)
				}
			}
		})
	}
}

// A Stopped sandbox whose seat was slung again after its landing is kept: the
// landed facts no longer describe the seat, whose hook names another bead or
// whose branch is not the landed one. The kept record names both pairs, the
// sandbox and its record stay, and the seat's item is raised once the window
// passes (gt-7h38pd).
func TestPatrolKeepsAStoppedSandboxOfAReusedSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		reslung    func(p *keptSeatPatrol)
		holdBead   string
		holdBranch string
	}{
		{
			name: "the seat's hook names another bead",
			reslung: func(p *keptSeatPatrol) {
				p.seatFields.HookBead = "gt-slung2"
				p.seatFields.Branch = "polecat/agate/gt-slung2+x2"
			},
			holdBead:   "gt-slung2",
			holdBranch: "polecat/agate/gt-slung2+x2",
		},
		{
			name: "the seat works another branch of the same bead",
			reslung: func(p *keptSeatPatrol) {
				p.seatFields.Branch = "polecat/agate/gt-n4xq8+x2"
			},
			holdBead:   "<missing>",
			holdBranch: "polecat/agate/gt-n4xq8+x2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newStoppedSeatPatrol(t, true)
			tt.reslung(p)
			p.setAgent()
			var logs strings.Builder
			p.host.d.logger = log.New(&logs, "", 0)

			p.host.patrolScanSandboxRecords()

			want := fmt.Sprintf(sandboxKeptRecordStaleReason, endBead, endBranch, tt.holdBead, tt.holdBranch)
			if len(p.deleted) != 0 {
				t.Fatalf("sandboxes deleted = %v, want the reused seat's sandbox kept", p.deleted)
			}
			if !p.sandboxUp {
				t.Error("the sandbox is gone, want the reused seat's sandbox still up")
			}
			if !p.recorded() {
				t.Error("the registry record was removed for a sandbox that stays")
			}
			if got := p.keptSeatOf(t).Reason; got != want {
				t.Errorf("kept reason = %q, want %q", got, want)
			}
			if !strings.Contains(logs.String(), want) {
				t.Errorf("log %q does not carry the stale reason %q", logs.String(), want)
			}
			p.clk.Advance(keptSeatAttentionAfter + time.Minute)
			items := keptSeatItems(t, p.host.d, p.clk.Now())
			if len(items) != 1 || !strings.Contains(items[0], want) {
				t.Errorf("kept-seat items = %v, want one carrying %q", items, want)
			}
		})
	}
}

// A gateway that answers the existence question but not the phase question is
// silent for the tick too: the record is kept and counts the try, the error is
// logged, and the seat's item is raised at the retry limit (gt-7h38pd).
func TestGatewayPhaseQuestionThatErrorsIsRetriedLikeTheExistenceQuestion(t *testing.T) {
	t.Parallel()
	p := newStoppedSeatPatrol(t, true)
	p.host.sandboxStatus = func(string, string) (sandbox.Status, error) {
		return sandbox.Status{}, errors.New("signal: killed")
	}
	var logs strings.Builder
	p.host.d.logger = log.New(&logs, "", 0)

	for tick := 1; tick <= 5; tick++ {
		p.clk.Advance(time.Minute)
		p.host.patrolScanSandboxRecords()
		if len(p.deleted) != 0 {
			t.Fatalf("tick %d: sandboxes deleted = %v, want none while the phase is unknown", tick, p.deleted)
		}
		if !p.sandboxUp {
			t.Fatalf("tick %d: the sandbox is gone, want it kept while the phase is unknown", tick)
		}
		if got := p.keptSeatOf(t).Tries; got != tick {
			t.Errorf("tick %d: tries = %d, want %d", tick, got, tick)
		}
	}
	if items := keptSeatItems(t, p.host.d, p.clk.Now()); len(items) != 0 {
		t.Fatalf("kept-seat items after five silent ticks = %v, want none", items)
	}
	p.clk.Advance(time.Minute)
	p.host.patrolScanSandboxRecords()

	items := keptSeatItems(t, p.host.d, p.clk.Now())
	if len(items) != 1 {
		t.Fatalf("kept-seat items after six silent ticks = %v, want one", items)
	}
	if want := gatewayNoAnswerKeepReason(p.clk.Now()); !strings.Contains(items[0], want) {
		t.Errorf("kept-seat item %q does not name the last try %q", items[0], want)
	}
	if got := logs.String(); !strings.Contains(got, "cannot ask gateway") || !strings.Contains(got, "for its phase") {
		t.Errorf("log %q does not name the phase question that failed", got)
	}
}

// corruptKeptSeat overwrites the test seat's kept record with bytes that do not
// decode.
func corruptKeptSeat(t *testing.T, p *keptSeatPatrol) {
	t.Helper()
	path, err := keptSandboxSeatPath(p.town, endRig, endPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A silent gateway whose tick count cannot be kept in the seat's record raises
// the seat's item at once, naming why the count cannot be kept, without waiting
// for the retry limit. An unreadable record is no collect failure once its seat
// is raised, and the alarm clears when the gateway answers (gt-7h38pd).
func TestGatewayTickCounterThatCannotBeKeptRaisesTheAlarm(t *testing.T) {
	t.Parallel()
	silentGateway := func(p *keptSeatPatrol) {
		p.host.sandboxExists = func(string, string) (bool, error) {
			return false, errors.New("signal: killed")
		}
	}
	t.Run("an unreadable kept record raises the counter reason on the first silent tick", func(t *testing.T) {
		t.Parallel()
		p := newStoppedSeatPatrol(t, true)
		corruptKeptSeat(t, p)
		silentGateway(p)

		p.clk.Advance(time.Minute)
		p.host.patrolScanSandboxRecords()

		items := keptSeatItems(t, p.host.d, p.clk.Now())
		want := "cannot count gateway tries for " + endRig + "/" + endPolecat + ":"
		if len(items) != 1 || !strings.Contains(items[0], want) {
			t.Fatalf("kept-seat items = %v, want one carrying %q", items, want)
		}
		if len(p.deleted) != 0 {
			t.Errorf("sandboxes deleted = %v, want none while the gateway is silent", p.deleted)
		}
	})

	t.Run("a write that fails raises the write's error and keeps counting in memory", func(t *testing.T) {
		t.Parallel()
		p := newStoppedSeatPatrol(t, true)
		p.host.keptSeatWrite = func(string, string, string, keptSandboxSeatRecord) error {
			return errors.New("disk full")
		}
		silent := true
		p.host.sandboxExists = func(string, string) (bool, error) {
			if silent {
				return false, errors.New("signal: killed")
			}
			return p.sandboxUp, nil
		}

		p.clk.Advance(time.Minute)
		p.host.patrolScanSandboxRecords()
		items := keptSeatItems(t, p.host.d, p.clk.Now())
		if len(items) != 1 || !strings.Contains(items[0], "disk full") {
			t.Fatalf("kept-seat items after the first silent tick = %v, want one naming the write error", items)
		}
		for tick := 2; tick <= 6; tick++ {
			p.clk.Advance(time.Minute)
			p.host.patrolScanSandboxRecords()
		}
		key := gatewayTallyKey(endRig, endPolecat)
		if got := p.host.d.gatewayTallySnapshot()[key].tries; got != 6 {
			t.Errorf("tries held in memory after six silent ticks = %d, want 6", got)
		}
		if n := len(keptSeatItems(t, p.host.d, p.clk.Now())); n != 1 {
			t.Errorf("kept-seat items after six silent ticks = %d, want the same one", n)
		}

		silent = false
		p.clk.Advance(time.Minute)
		p.host.patrolScanSandboxRecords()

		if want := []string{session.PolecatSessionName(session.DefaultPrefix, endPolecat)}; !reflect.DeepEqual(p.deleted, want) {
			t.Fatalf("sandboxes deleted after the answer = %v, want %v", p.deleted, want)
		}
		if items := keptSeatItems(t, p.host.d, p.clk.Now()); len(items) != 0 {
			t.Errorf("kept-seat items after the answer = %v, want none", items)
		}
	})
}
