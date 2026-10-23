package daemon

import (
	"errors"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

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

// One tick asks the gateway about at most its budget of records, and the walk
// rotates with the daemon clock, so records the gateway still has cannot starve
// the ones behind them: here the first four (sorted) are present and the last
// three are gone, the budget is three, and every gone record is removed within
// ceil(7/3) = 3 ticks while no tick asks more than three questions.
func TestPatrolScanSandboxRecordsRotateSoKeptRecordsDoNotStarveTheRest(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	names := []string{"gt-p0", "gt-p1", "gt-p2", "gt-p3", "gt-p4", "gt-p5", "gt-p6"}
	writeOldRecords(t, town, clk, names)
	present := map[string]bool{"gt-p0": true, "gt-p1": true, "gt-p2": true, "gt-p3": true}
	var asked []string
	h := &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: newFakeTmux(clk), clock: clk},
		sandboxExists: func(_, name string) (bool, error) {
			asked = append(asked, name)
			return present[name], nil
		},
		sandboxRecordBudget: 3,
	}

	const budget, ticks = 3, 3 // ceil(7/3)
	for tick := 0; tick < ticks; tick++ {
		asked = nil
		h.patrolScanSandboxRecords()
		if len(asked) > budget {
			t.Fatalf("tick %d asked %d questions %v, want at most %d", tick, len(asked), asked, budget)
		}
		clk.Advance(time.Minute)
	}

	left := session.ListSandboxSessions(town)
	if want := []string{"gt-p0", "gt-p1", "gt-p2", "gt-p3"}; !reflect.DeepEqual(left, want) {
		t.Errorf("records left after %d ticks = %v, want only the ones the gateway has %v", ticks, left, want)
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
