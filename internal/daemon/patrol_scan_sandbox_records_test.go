package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// recordReap is one reap scenario: a town with one registry record, a fake tmux
// and a scripted gateway.
type recordReap struct {
	t            *testing.T
	town         string
	host         *patrolScanHost
	asked        []string // "gateway|sandbox" questions, in order
	markerAsked  []string // "gateway|sandbox" ended-marker questions, in order
	deleterCalls []string // sandboxes the record deleter was asked to delete
	logs         strings.Builder
}

type recordReapOpts struct {
	age        time.Duration // the record's age at the fixed clock; negative leaves CreatedAt zero
	liveTmux   bool          // a live session answers to the record's name
	gateway    string        // the record's gateway
	exists     bool          // the gateway has the sandbox
	existsErr  error         // the gateway cannot be asked
	marker     bool          // the held sandbox answers that its ended marker is set
	markerErr  error         // the held sandbox cannot be asked for its marker
	deleterErr error         // the record deleter fails
	noDeleter  bool          // neither a seam nor a linked deleter exists
}

const recordReapSession = "gt-capable"

func newRecordReap(t *testing.T, o recordReapOpts) *recordReap {
	t.Helper()
	town := t.TempDir()
	clk := newFixedClock()
	tm := newFakeTmux(clk)
	if o.liveTmux {
		tm.addSession(recordReapSession, "claude", clk.Now())
	}
	r := &recordReap{t: t, town: town}
	rec := session.SandboxRecord{Session: recordReapSession, Rig: "gastown", Polecat: "capable", Gateway: o.gateway}
	if o.age >= 0 {
		rec.CreatedAt = clk.Now().Add(-o.age)
	}
	if err := session.WriteSandboxRecord(town, rec); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&r.logs, "", 0), tmux: tm, clock: clk}
	r.host = &patrolScanHost{d: d, sandboxExists: func(gateway, name string) (bool, error) {
		r.asked = append(r.asked, gateway+"|"+name)
		return o.exists, o.existsErr
	}, sandboxEndedMarker: func(gateway, name string) (bool, error) {
		r.markerAsked = append(r.markerAsked, gateway+"|"+name)
		return o.marker, o.markerErr
	}, noSandboxDeleterLinked: o.noDeleter}
	if !o.noDeleter {
		r.host.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
			r.deleterCalls = append(r.deleterCalls, rec.Session)
			return o.deleterErr
		}
	}
	return r
}

func (r *recordReap) recorded() bool {
	r.t.Helper()
	ok, err := session.IsSandboxed(r.town, recordReapSession)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// A registry record is removed only for a sandbox the gateway reports gone,
// and only once it is past the grace and no live session answers to its name.
// An unreachable gateway keeps the record, as does a young one: a sandbox that
// deleted itself is told from one still coming up by the gateway's answer and
// the record's age together. A sandbox the gateway still has is ended only when
// its ended marker is set, through the record deleter; every other answer, and a
// binary with no deleter linked, keeps the record and the sandbox.
func TestPatrolScanSandboxRecords(t *testing.T) {
	t.Parallel()
	old := sandboxRecordGrace + time.Minute
	tests := []struct {
		name             string
		opts             recordReapOpts
		wantGone         bool
		wantAsked        bool
		wantMarkerAsked  bool
		wantDeleterCalls bool
		wantLog          string // a fragment the pass logs, "" for no assertion
	}{
		{"old_record_gateway_has_no_sandbox", recordReapOpts{age: old, gateway: "gw1"}, true, true, false, false, ""},
		{"old_record_with_no_gateway_named", recordReapOpts{age: old}, true, true, false, false, ""},
		{"record_without_a_date_counts_as_old", recordReapOpts{age: -1, gateway: "gw1"}, true, true, false, false, ""},
		{"young_record_is_kept", recordReapOpts{age: sandboxRecordGrace - time.Second, gateway: "gw1"}, false, false, false, false, ""},
		{"gateway_unreachable_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", existsErr: errors.New("connection refused")}, false, true, false, false, ""},
		{"live_session_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", liveTmux: true}, false, false, false, false, ""},
		{"held_sandbox_with_marker_is_deleted_through_deleter", recordReapOpts{age: old, gateway: "gw1", exists: true, marker: true}, true, true, true, true, ""},
		{"held_sandbox_without_marker_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", exists: true}, false, true, true, false, ""},
		{"held_sandbox_marker_exec_error_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", exists: true, markerErr: errors.New("gateway unreachable")}, false, true, true, false, ""},
		{"held_sandbox_deleter_error_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", exists: true, marker: true, deleterErr: errors.New("disk full")}, false, true, true, true, ""},
		{"held_sandbox_with_no_deleter_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", exists: true, marker: true, noDeleter: true}, false, true, true, false, "no sandbox deleter is linked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRecordReap(t, tt.opts)
			r.host.patrolScanSandboxRecords()
			if gone := !r.recorded(); gone != tt.wantGone {
				t.Fatalf("record removed = %v, want %v", gone, tt.wantGone)
			}
			if asked := len(r.asked) > 0; asked != tt.wantAsked {
				t.Errorf("gateway asked = %v (%v), want %v", asked, r.asked, tt.wantAsked)
			}
			if asked := len(r.markerAsked) > 0; asked != tt.wantMarkerAsked {
				t.Errorf("ended marker asked = %v (%v), want %v", asked, r.markerAsked, tt.wantMarkerAsked)
			}
			if called := len(r.deleterCalls) > 0; called != tt.wantDeleterCalls {
				t.Errorf("record deleter called = %v (%v), want %v", called, r.deleterCalls, tt.wantDeleterCalls)
			}
			if tt.wantAsked && len(r.asked) == 1 && r.asked[0] != tt.opts.gateway+"|"+recordReapSession {
				t.Errorf("gateway question = %q, want the record's gateway and its sandbox name", r.asked[0])
			}
			if tt.wantLog != "" && !strings.Contains(r.logs.String(), tt.wantLog) {
				t.Errorf("log = %q, want it to contain %q", r.logs.String(), tt.wantLog)
			}
		})
	}
}

// writeOldRecords writes one old record per name under town.
func writeOldRecords(t *testing.T, town string, clk interface{ Now() time.Time }, names []string) {
	t.Helper()
	for _, n := range names {
		rec := session.SandboxRecord{Session: n, CreatedAt: clk.Now().Add(-time.Hour)}
		if err := session.WriteSandboxRecord(town, rec); err != nil {
			t.Fatal(err)
		}
	}
}

// One tick checks at most its budget of records, and each tick's walk starts
// after the name the last one ended on, so records the gateway still has cannot
// starve the ones behind them: every record is asked within ceil(n/budget)
// ticks, and the ones the gateway has are the only ones left. The two-minute
// case runs at the default cadence.
func TestPatrolScanSandboxRecordsRotateSoKeptRecordsDoNotStarveTheRest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		records int
		budget  int
		step    time.Duration // between ticks
		kept    int           // the first kept records (sorted) are the gateway's
	}{
		{"minute_ticks_seven_records", 7, 3, time.Minute, 4},
		{"two_minute_ticks_forty_records", 40, 20, 2 * time.Minute, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			names := make([]string, tt.records)
			for i := range names {
				names[i] = fmt.Sprintf("gt-p%02d", i)
			}
			writeOldRecords(t, town, clk, names)
			present := make(map[string]bool, tt.kept)
			for _, n := range names[:tt.kept] {
				present[n] = true
			}
			asked := make(map[string]bool)
			var tickAsked []string
			h := &patrolScanHost{
				d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: newFakeTmux(clk), clock: clk},
				sandboxExists: func(_, name string) (bool, error) {
					tickAsked = append(tickAsked, name)
					asked[name] = true
					return present[name], nil
				},
				sandboxEndedMarker:  func(string, string) (bool, error) { return false, nil },
				sandboxRecordBudget: tt.budget,
			}

			ticks := (tt.records + tt.budget - 1) / tt.budget
			for tick := 0; tick < ticks; tick++ {
				tickAsked = nil
				h.patrolScanSandboxRecords()
				if len(tickAsked) > tt.budget {
					t.Fatalf("tick %d asked %d questions %v, want at most %d", tick, len(tickAsked), tickAsked, tt.budget)
				}
				clk.Advance(tt.step)
			}

			for _, n := range names {
				if !asked[n] {
					t.Errorf("record %s not asked within %d ticks", n, ticks)
				}
			}
			if left, want := session.ListSandboxSessions(town), names[:tt.kept]; !reflect.DeepEqual(left, want) {
				t.Errorf("records left after %d ticks = %v, want only the ones the gateway has %v", ticks, left, want)
			}
		})
	}
}

// churnHost is a record pass over town whose gateway has every sandbox, so the
// pass removes nothing and asked holds the names it judged, in order.
func churnHost(town string, clk *clockwork.FakeClock, budget int, asked *[]string) *patrolScanHost {
	return &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: newFakeTmux(clk), clock: clk},
		sandboxExists: func(_, name string) (bool, error) {
			*asked = append(*asked, name)
			return true, nil
		},
		sandboxEndedMarker:  func(string, string) (bool, error) { return false, nil },
		sandboxRecordBudget: budget,
	}
}

// The walk keeps its place when records come and go between ticks: a record
// removed under the cursor does not restart the walk, a record still inside the
// grace uses none of the budget, and a record added behind the cursor is judged
// once the walk wraps.
func TestPatrolScanSandboxRecordsCursorSurvivesChurn(t *testing.T) {
	t.Parallel()
	t.Run("removed_cursor_record_does_not_restart_the_walk", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		clk := newFixedClock()
		writeOldRecords(t, town, clk, []string{"gt-p00", "gt-p01", "gt-p02", "gt-p03"})
		var asked []string
		h := churnHost(town, clk, 2, &asked)
		h.patrolScanSandboxRecords()
		if want := []string{"gt-p00", "gt-p01"}; !reflect.DeepEqual(asked, want) {
			t.Fatalf("first tick asked %v, want %v", asked, want)
		}
		if err := session.RemoveSandboxRecord(town, "gt-p01"); err != nil {
			t.Fatal(err)
		}

		asked = nil
		h.patrolScanSandboxRecords()
		if want := []string{"gt-p02", "gt-p03"}; !reflect.DeepEqual(asked, want) {
			t.Errorf("second tick asked %v, want %v: the walk resumes after the removed cursor name", asked, want)
		}
	})
	t.Run("young_records_use_no_budget", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		clk := newFixedClock()
		for _, n := range []string{"gt-a", "gt-b"} {
			if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: n, CreatedAt: clk.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
		}
		writeOldRecords(t, town, clk, []string{"gt-c", "gt-d", "gt-e"})
		var asked []string
		h := churnHost(town, clk, 2, &asked)
		h.patrolScanSandboxRecords()
		if want := []string{"gt-c", "gt-d"}; !reflect.DeepEqual(asked, want) {
			t.Errorf("tick asked %v, want %v: the young records are skipped without costing the budget", asked, want)
		}
	})
	t.Run("record_added_behind_the_cursor_is_judged_after_the_wrap", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		clk := newFixedClock()
		writeOldRecords(t, town, clk, []string{"gt-p02", "gt-p03", "gt-p04"})
		var asked []string
		h := churnHost(town, clk, 2, &asked)
		h.patrolScanSandboxRecords()
		if want := []string{"gt-p02", "gt-p03"}; !reflect.DeepEqual(asked, want) {
			t.Fatalf("first tick asked %v, want %v", asked, want)
		}
		writeOldRecords(t, town, clk, []string{"gt-p00"})

		asked = nil
		h.patrolScanSandboxRecords()
		if want := []string{"gt-p04", "gt-p00"}; !reflect.DeepEqual(asked, want) {
			t.Errorf("second tick asked %v, want %v: the new record sorts behind the cursor and is reached on the wrap", asked, want)
		}
	})
}

// probeCounter counts the tmux probes a record pass makes.
type probeCounter struct {
	sessionTmux
	probes int
}

func (p *probeCounter) HasSession(name string) (bool, error) {
	p.probes++
	return p.sessionTmux.HasSession(name)
}

// Every record a pass checks costs a tmux probe, live ones included, so one
// tick probes at most its budget of records: here all twelve records are live
// and the budget is four, so no tick probes more than four.
func TestPatrolScanSandboxRecordsBoundsTmuxProbes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	tm := newFakeTmux(clk)
	names := make([]string, 12)
	for i := range names {
		names[i] = fmt.Sprintf("gt-l%02d", i)
		tm.addSession(names[i], "claude", clk.Now())
	}
	writeOldRecords(t, town, clk, names)
	probes := &probeCounter{sessionTmux: tm}
	h := &patrolScanHost{
		d:                   &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: probes, clock: clk},
		sandboxRecordBudget: 4,
	}

	for tick := 0; tick < 3; tick++ {
		probes.probes = 0
		h.patrolScanSandboxRecords()
		if probes.probes > 4 {
			t.Fatalf("tick %d probed tmux %d times, want at most 4", tick, probes.probes)
		}
	}
}

// A tmux error keeps every record untouched: the probe cannot say the session
// is gone, so the pass neither asks the gateway nor deletes anything, and it
// logs why for each record. The next tick judges them again (gt-0vb2fy).
func TestPatrolScanSandboxRecordsKeepsEverythingWhenTmuxErrors(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	writeOldRecords(t, town, clk, []string{"gt-a", "gt-b"})
	tm := newFakeTmux(clk)
	tm.hasErr = errors.New("tmux: no server")
	var logs strings.Builder
	gatewayAsked, removeCalls := 0, 0
	h := &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), tmux: tm, clock: clk},
		sandboxExists: func(_, _ string) (bool, error) {
			gatewayAsked++
			return false, nil // the answer that would delete if it were asked
		},
		removeSandboxRecord: func(_, _ string) error {
			removeCalls++
			return nil
		},
		sandboxRecordBudget: 10,
	}

	h.patrolScanSandboxRecords()

	if gatewayAsked != 0 || removeCalls != 0 {
		t.Errorf("gateway asked %d times, records removed %d times, want 0 and 0 on a tmux error", gatewayAsked, removeCalls)
	}
	out := logs.String()
	for _, name := range []string{"gt-a", "gt-b"} {
		if want := "sandbox record " + name + " kept: cannot ask tmux whether the session is live"; !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// The failure branches keep what they cannot judge and never stop the pass: a
// gateway error keeps its record, and a removal that fails is logged while the
// next record is still judged and removed.
func TestPatrolScanSandboxRecordsFailureBranches(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	writeOldRecords(t, town, clk, []string{"gt-a", "gt-b", "gt-c", "gt-d"})
	tm := newFakeTmux(clk)
	var logs strings.Builder
	var removed []string
	h := &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), tmux: tm, clock: clk},
		sandboxExists: func(_, name string) (bool, error) {
			if name == "gt-a" {
				return false, errors.New("connection refused")
			}
			return false, nil
		},
		removeSandboxRecord: func(_, name string) error {
			if name == "gt-b" {
				return errors.New("disk full")
			}
			removed = append(removed, name)
			return nil
		},
		sandboxRecordBudget: 10,
	}

	h.patrolScanSandboxRecords()

	if want := []string{"gt-c", "gt-d"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("records removed = %v, want %v (gt-a: gateway error; gt-b: removal failed, pass went on)", removed, want)
	}
	out := logs.String()
	for _, want := range []string{
		"gt-a kept: cannot ask gateway",
		"gt-b: removing it failed: disk full",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// keptSeatPatrol is one finished sandbox seat whose keep the end recorded and
// whose sandbox registry record the patrol finds: the record pass over the
// seat-end fake, with the record pass's own gateway and deleter seams, so the
// patrol's completion of the seat's end is the real end.
type keptSeatPatrol struct {
	*seatEndFake
	clk *clockwork.FakeClock
	// sandboxUp is the gateway's answer for the seat's sandbox: true until the
	// record deleter has deleted it.
	sandboxUp bool
}

func newKeptSeatPatrol(t *testing.T, now time.Time) *keptSeatPatrol {
	t.Helper()
	f := newSeatEndFake(t)
	clk := clockwork.NewFakeClockAt(now)
	f.host.d.clock = clk
	f.host.d.tmux = newFakeTmux(clk)
	p := &keptSeatPatrol{seatEndFake: f, clk: clk}
	f.host.sandboxExists = func(string, string) (bool, error) { return p.sandboxUp, nil }
	f.host.sandboxEndedMarker = func(string, string) (bool, error) { return true, nil }
	f.host.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
		f.deleted = append(f.deleted, rec.Session)
		p.sandboxUp = false
		return nil
	}
	f.host.d.endSandboxSeat = f.host.endFinishedSandboxSeat
	return p
}

// registryRecord writes the seat's sandbox registry record, as the sling does
// when it launches the seat, old enough for the record pass to judge.
func (p *keptSeatPatrol) registryRecord(t *testing.T) {
	t.Helper()
	rec := session.SandboxRecord{
		Session:   session.PolecatSessionName(session.DefaultPrefix, endPolecat),
		Rig:       endRig,
		Polecat:   endPolecat,
		Gateway:   "gw1",
		CreatedAt: p.clk.Now().Add(-time.Hour),
	}
	if err := session.WriteSandboxRecord(p.town, rec); err != nil {
		t.Fatal(err)
	}
}

// recorded reports whether the seat's sandbox registry record is still there.
func (p *keptSeatPatrol) recorded() bool {
	p.t.Helper()
	ok, err := session.IsSandboxed(p.town, session.PolecatSessionName(session.DefaultPrefix, endPolecat))
	if err != nil {
		p.t.Fatal(err)
	}
	return ok
}

// keptSeatAttentionSeats is the attention collector's view of the town's kept
// records at now.
func keptSeatAttentionSeats(t *testing.T, d *Daemon, now time.Time) int {
	t.Helper()
	src := &attentionSources{d: d, now: now}
	items, err := src.collectKeptSandboxSeats(context.Background())
	if err != nil {
		t.Fatalf("collecting the kept-seat attention items: %v", err)
	}
	return len(items)
}

// The patrol that deletes a held sandbox completes the kept seat's end in the
// same pass: the end runs again with the landing facts the kept record holds,
// every guard agrees, the host directories and the record go, and the queue
// raises no false sandbox-seat-kept item (gt-cz5tdz).
func TestPatrolScanCompletesTheEndOfADeletedKeptSeat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC)
	p := newKeptSeatPatrol(t, now)
	p.seedAgent()
	p.retire(intent.DesiredStop)
	p.seatDirs()
	p.sandboxUp = true
	p.registryRecord(t)
	// The agent's session is still ending: its ended marker is not set yet, so
	// the landing has no sandbox it may delete (gt-0vb2fy).
	markerSet := false
	p.host.sandboxEndedMarker = func(string, string) (bool, error) { return markerSet, nil }

	// The landing's own pass keeps the seat: the sandbox is still up and its
	// marker is not set.
	if p.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a seat whose sandbox is still up was removed")
	}
	// The session has ended, so the marker is set and the patrol deletes the
	// sandbox the landing left.
	markerSet = true
	// Half an hour on, the queue would raise the false item.
	p.clk.Advance(32 * time.Minute)
	if got := keptSeatAttentionSeats(t, p.host.d, p.clk.Now()); got != 1 {
		t.Fatalf("attention items before the patrol = %d, want the false kept item", got)
	}

	p.host.patrolScanSandboxRecords()

	if want := []string{session.PolecatSessionName(session.DefaultPrefix, endPolecat)}; !reflect.DeepEqual(p.deleted, want) {
		t.Fatalf("sandboxes deleted = %v, want %v", p.deleted, want)
	}
	if p.recorded() {
		t.Error("the registry record survived the deleted sandbox")
	}
	if seats, bad, err := KeptSandboxSeats(p.town); err != nil || len(bad) != 0 || len(seats) != 0 {
		t.Errorf("kept seats after the patrol = %+v, bad %v, %v; want none", seats, bad, err)
	}
	if dirExists(t, filepath.Join(p.town, endRig, "polecats", endPolecat)) {
		t.Error("the released seat kept its host directories")
	}
	if got := keptSeatAttentionSeats(t, p.host.d, p.clk.Now()); got != 0 {
		t.Errorf("attention items after the patrol = %d, want none", got)
	}
}

// The record pass deletes a held sandbox under the seat lock the landing's own
// end takes, and re-reads the record under it. A landing that removed the record
// between the pass's read and its delete leaves the patrol nothing to do: it
// makes no deleter call and does not fail the pass. Every other answer keeps the
// record with that condition's own reason (gt-0vb2fy).
func TestPatrolScanKeepsAHeldSandboxItCannotDeleteUnderTheSeatLock(t *testing.T) {
	t.Parallel()
	name := session.PolecatSessionName(session.DefaultPrefix, endPolecat)
	tests := []struct {
		name string
		// between runs in the gateway seam, after the pass read the record and
		// before it takes the seat lock.
		between     func(t *testing.T, p *keptSeatPatrol)
		lock        func(rigPath, name string, fn func() error) error
		removeErr   error
		rig         string
		wantDeleted int
		wantReason  string // "" for a quiet skip
	}{
		{
			name: "a landing removed the record first",
			between: func(t *testing.T, p *keptSeatPatrol) {
				if err := session.RemoveSandboxRecord(p.town, name); err != nil {
					t.Fatalf("removing the record as the landing would: %v", err)
				}
			},
		},
		{
			name: "an unreadable record keeps it",
			between: func(t *testing.T, p *keptSeatPatrol) {
				path := filepath.Join(session.SandboxRegistryDir(p.town), name+".json")
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "kept: cannot re-read its sandbox record under its seat lock:",
		},
		{
			name: "a busy seat lock keeps it",
			lock: func(string, string, func() error) error {
				return polecat.ErrSandboxSeatLockBusy
			},
			wantReason: "kept: its seat lock could not be taken: polecat directory lock is busy",
		},
		{
			name:        "a record that cannot be removed keeps the seat end",
			removeErr:   errors.New("registry is read-only"),
			wantDeleted: 1,
			wantReason:  "kept: its sandbox is deleted but its record could not be removed: registry is read-only",
		},
		{
			name:       "an unsafe rig keeps it",
			rig:        "../gastown",
			wantReason: "kept: sandbox seat directories: rig \"../gastown\" is not a single path segment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC)
			p := newKeptSeatPatrol(t, now)
			p.seedAgent()
			p.retire(intent.DesiredStop)
			p.seatDirs()
			p.sandboxUp = true
			if tt.rig != "" {
				if err := session.WriteSandboxRecord(p.town, session.SandboxRecord{
					Session: name, Rig: tt.rig, Polecat: endPolecat, Gateway: "gw1",
					CreatedAt: p.clk.Now().Add(-time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				p.registryRecord(t)
			}
			var logs strings.Builder
			p.host.d.logger = log.New(&logs, "", 0)
			p.host.sandboxExists = func(string, string) (bool, error) {
				if tt.between != nil {
					tt.between(t, p)
				}
				return true, nil
			}
			p.host.sandboxSeatLock = tt.lock
			p.host.removeSandboxRecord = func(string, string) error { return tt.removeErr }

			p.host.patrolScanSandboxRecords()

			if len(p.deleted) != tt.wantDeleted {
				t.Errorf("record deleter calls = %v, want %d", p.deleted, tt.wantDeleted)
			}
			if tt.wantReason == "" {
				return
			}
			if !strings.Contains(logs.String(), tt.wantReason) {
				t.Errorf("log %q does not name the reason %q", logs.String(), tt.wantReason)
			}
		})
	}
}

// A seat kept for a reason that stays true is still kept after the patrol
// deletes its sandbox, and its record names that reason: the completed end
// measured the sandbox gone and the next guard kept the seat (gt-cz5tdz).
func TestPatrolScanLeavesASeatKeptForAnotherReasonKept(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC)
	p := newKeptSeatPatrol(t, now)
	p.seatFields.PushFailed = true
	p.seedAgent()
	p.retire(intent.DesiredStop)
	p.seatDirs()
	p.sandboxUp = true
	p.registryRecord(t)

	if p.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a seat whose sandbox is still up was removed")
	}

	p.host.patrolScanSandboxRecords()

	if len(p.deleted) != 1 {
		t.Fatalf("sandboxes deleted = %v, want the held one", p.deleted)
	}
	seats, bad, err := KeptSandboxSeats(p.town)
	if err != nil || len(bad) != 0 {
		t.Fatalf("reading the kept seats: bad %v, %v", bad, err)
	}
	if len(seats) != 1 {
		t.Fatalf("kept seats after the patrol = %+v, want the still-kept seat", seats)
	}
	if seats[0].Reason != keptSeatReason {
		t.Errorf("keep reason = %q, want %q: the completed end names the fact that still keeps it", seats[0].Reason, keptSeatReason)
	}
	if !dirExists(t, filepath.Join(p.town, endRig, "polecats", endPolecat)) {
		t.Error("a seat kept for unpushed work lost its host directories")
	}
}

// A kept record written before it held landing facts is left as it is and the
// patrol logs why: with no facts to offer the end, the keep stands as today
// (gt-cz5tdz).
func TestPatrolScanLeavesAKeptRecordWithoutLandingFactsAlone(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true})
	var logs strings.Builder
	r.host.d.logger = log.New(&logs, "", 0)
	since := time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC)
	old := keptSandboxSeatRecord{Bead: endBead, Reason: keptSeatReason, Since: since}
	if err := writeKeptSandboxSeat(r.town, "gastown", "capable", old); err != nil {
		t.Fatal(err)
	}
	var calls []string
	r.host.d.endSandboxSeat = func(rig, name string, _ sandboxSeatLanding) bool {
		calls = append(calls, rig+"/"+name)
		return true
	}

	r.host.patrolScanSandboxRecords()

	if len(r.deleterCalls) != 1 {
		t.Fatalf("the held sandbox was not deleted: %v", r.deleterCalls)
	}
	if r.recorded() {
		t.Error("the registry record survived the deleted sandbox")
	}
	if len(calls) != 0 {
		t.Errorf("the seat end was offered for a record with no landing facts: %v", calls)
	}
	seats, bad, err := KeptSandboxSeats(r.town)
	if err != nil || len(bad) != 0 || len(seats) != 1 {
		t.Fatalf("kept seats = %+v, bad %v, %v; want the untouched record", seats, bad, err)
	}
	if seats[0].Reason != old.Reason || !seats[0].Since.Equal(since) || seats[0].Bead != old.Bead {
		t.Errorf("kept record = %+v, want it left as it was", seats[0])
	}
	if want := "sandbox seat end gastown/capable: its kept record names no landing facts"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}

// The patrol offers a seat's end only once it has removed the sandbox's record
// and can read the landing facts from the kept record: a removal that fails and
// a kept record that cannot be read each leave the keep as it is (gt-cz5tdz).
func TestPatrolScanOffersNoSeatEndItCannotReadTheFactsFor(t *testing.T) {
	t.Parallel()
	landing := keptSeatLanding()
	tests := []struct {
		name string
		// noSeat is a registry record that names no rig or polecat: no kept
		// record can exist for it, so nothing is offered and nothing is logged.
		noSeat    bool
		removeErr error
		corrupt   bool
		wantLog   string
	}{
		{
			name:      "a removal that fails",
			removeErr: errors.New("disk full"),
			wantLog:   "sandbox record gt-agate: removing it failed: disk full",
		},
		{
			name:    "a kept record that cannot be read",
			corrupt: true,
			wantLog: "sandbox seat end gastown/agate: its kept record is unreadable, so its seat end is not completed",
		},
		{
			name:   "a record that names no seat",
			noSeat: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			reg := session.SandboxRecord{
				Session: "gt-agate", Rig: endRig, Polecat: "agate", Gateway: "gw1",
				CreatedAt: clk.Now().Add(-time.Hour),
			}
			if tt.noSeat {
				reg.Rig, reg.Polecat = "", ""
			}
			if err := session.WriteSandboxRecord(town, reg); err != nil {
				t.Fatal(err)
			}
			if !tt.noSeat {
				path, err := keptSandboxSeatPath(town, endRig, "agate")
				if err != nil {
					t.Fatal(err)
				}
				if tt.corrupt {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else if err := writeKeptSandboxSeat(town, endRig, "agate", keptSandboxSeatRecord{
					Bead: endBead, Reason: "the gateway has not confirmed the sandbox gone", Since: clk.Now(), Landing: &landing,
				}); err != nil {
					t.Fatal(err)
				}
			}
			var logs strings.Builder
			var calls []string
			d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), tmux: newFakeTmux(clk), clock: clk}
			d.endSandboxSeat = func(rig, name string, _ sandboxSeatLanding) bool {
				calls = append(calls, rig+"/"+name)
				return true
			}
			h := &patrolScanHost{
				d:             d,
				sandboxExists: func(string, string) (bool, error) { return false, nil },
			}
			if tt.removeErr != nil {
				h.removeSandboxRecord = func(string, string) error { return tt.removeErr }
			}

			h.patrolScanSandboxRecords()

			if len(calls) != 0 {
				t.Errorf("seat ends offered = %v, want none", calls)
			}
			if tt.removeErr == nil {
				if left := session.ListSandboxSessions(town); len(left) != 0 {
					t.Errorf("registry records left = %v, want none", left)
				}
			}
			if tt.wantLog == "" {
				if strings.Contains(logs.String(), "sandbox seat end") {
					t.Errorf("log names a seat end for a record that names no seat:\n%s", logs.String())
				}
			} else if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log lacks %q:\n%s", tt.wantLog, logs.String())
			}
		})
	}
}

// A seat end that does not complete is logged and does not stop the pass: every
// other record is still removed and its seat's end offered, with the landing
// facts its kept record holds (gt-cz5tdz).
func TestPatrolScanSeatEndFailureDoesNotStopThePass(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	seats := []struct{ session, polecat string }{
		{"gt-agate", "agate"},
		{"gt-bismuth", "bismuth"},
	}
	landing := keptSeatLanding()
	for _, s := range seats {
		if err := session.WriteSandboxRecord(town, session.SandboxRecord{
			Session: s.session, Rig: endRig, Polecat: s.polecat, Gateway: "gw1",
			CreatedAt: clk.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeKeptSandboxSeat(town, endRig, s.polecat, keptSandboxSeatRecord{
			Bead: endBead, Reason: "the gateway has not confirmed the sandbox gone", Since: clk.Now(), Landing: &landing,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var logs strings.Builder
	var ended []string
	var landed []sandboxSeatLanding
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), tmux: newFakeTmux(clk), clock: clk}
	d.endSandboxSeat = func(rig, name string, l sandboxSeatLanding) bool {
		ended = append(ended, rig+"/"+name)
		landed = append(landed, l)
		return name != "agate" // the first seat's end fails here
	}
	h := &patrolScanHost{d: d, sandboxExists: func(string, string) (bool, error) { return false, nil }}

	h.patrolScanSandboxRecords()

	if want := []string{endRig + "/agate", endRig + "/bismuth"}; !reflect.DeepEqual(ended, want) {
		t.Errorf("seat ends offered = %v, want %v (the failed one does not stop the pass)", ended, want)
	}
	for _, got := range landed {
		if got != landing {
			t.Errorf("landing offered = %+v, want the recorded %+v", got, landing)
		}
	}
	if left := session.ListSandboxSessions(town); len(left) != 0 {
		t.Errorf("registry records left = %v, want none", left)
	}
	if want := "sandbox seat end gastown/agate: its seat end did not complete"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}
