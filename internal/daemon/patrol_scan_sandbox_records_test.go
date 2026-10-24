package daemon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/session"
)

// recordReap is one reap scenario: a town with one registry record, a fake tmux
// and a scripted gateway.
type recordReap struct {
	t     *testing.T
	town  string
	host  *patrolScanHost
	asked []string // "gateway|sandbox" questions, in order
}

type recordReapOpts struct {
	age       time.Duration // the record's age at the fixed clock; negative leaves CreatedAt zero
	liveTmux  bool          // a live session answers to the record's name
	gateway   string        // the record's gateway
	exists    bool          // the gateway has the sandbox
	existsErr error         // the gateway cannot be asked
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
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: tm, clock: clk}
	r.host = &patrolScanHost{d: d, sandboxExists: func(gateway, name string) (bool, error) {
		r.asked = append(r.asked, gateway+"|"+name)
		return o.exists, o.existsErr
	}}
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
// the record's age together.
func TestPatrolScanSandboxRecords(t *testing.T) {
	t.Parallel()
	old := sandboxRecordGrace + time.Minute
	tests := []struct {
		name      string
		opts      recordReapOpts
		wantGone  bool
		wantAsked bool
	}{
		{"old_record_gateway_has_no_sandbox", recordReapOpts{age: old, gateway: "gw1"}, true, true},
		{"old_record_with_no_gateway_named", recordReapOpts{age: old}, true, true},
		{"record_without_a_date_counts_as_old", recordReapOpts{age: -1, gateway: "gw1"}, true, true},
		{"young_record_is_kept", recordReapOpts{age: sandboxRecordGrace - time.Second, gateway: "gw1"}, false, false},
		{"gateway_still_has_the_sandbox", recordReapOpts{age: old, gateway: "gw1", exists: true}, false, true},
		{"gateway_unreachable_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", existsErr: errors.New("connection refused")}, false, true},
		{"live_session_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", liveTmux: true}, false, false},
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
			if tt.wantAsked && len(r.asked) == 1 && r.asked[0] != tt.opts.gateway+"|"+recordReapSession {
				t.Errorf("gateway question = %q, want the record's gateway and its sandbox name", r.asked[0])
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

// The failure branches keep what they cannot judge and never stop the pass: a
// gateway error keeps its record, a tmux error is logged and the gateway still
// decides, and a removal that fails is logged while the next record is still
// judged and removed.
func TestPatrolScanSandboxRecordsFailureBranches(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	writeOldRecords(t, town, clk, []string{"gt-a", "gt-b", "gt-c", "gt-d"})
	tm := newFakeTmux(clk)
	tm.hasErr = errors.New("tmux: no server")
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
		"cannot ask tmux, treating it as not live",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}
