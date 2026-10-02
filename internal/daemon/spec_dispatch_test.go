package daemon

import (
	"io"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townhealth"
)

func TestSpecDispatchDefaultsOn(t *testing.T) {
	t.Parallel()
	if !IsPatrolEnabled(nil, "spec_dispatch") {
		t.Error("spec_dispatch must be on with no config")
	}
	if !IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "spec_dispatch") {
		t.Error("spec_dispatch must be on with no spec_dispatch entry")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "spec_dispatch") {
		t.Error("spec_dispatch enabled:false must be off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "spec_dispatch") {
		t.Error("spec_dispatch enabled:true must be on")
	}
}

func TestSpecDispatchInterval(t *testing.T) {
	t.Parallel()
	if got := specDispatchInterval(nil); got != 60*time.Second {
		t.Errorf("default = %v", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{IntervalStr: "90s"}}}
	if got := specDispatchInterval(cfg); got != 90*time.Second {
		t.Errorf("configured = %v", got)
	}
	cfg.Patrols.SpecDispatch.IntervalStr = "bogus"
	if got := specDispatchInterval(cfg); got != 60*time.Second {
		t.Errorf("bad interval = %v", got)
	}
}

func TestFormatSpecDispatchReport(t *testing.T) {
	t.Parallel()
	out := []byte("  ✓ Work attached to p\n{\n  \"template\": \"built-in\",\n  \"roster\": \"claude-sonnet 1/2, deepseek-flash 2/2\",\n  \"candidates\": 3,\n" +
		"  \"dispatched\": [{\"bead\": \"gt-a\", \"line\": \"gt-a: slung to gastown/p on claude-sonnet (hooked seat 2/2)\"}],\n" +
		"  \"refused\": [{\"bead\": \"gt-b\", \"line\": \"gt-b: spec lint refused: ## Gate: section missing\"}],\n" +
		"  \"planning\": null, \"skipped\": [{\"bead\": \"gt-c\", \"line\": \"x\"}], \"failed\": null\n}\n")
	lines := formatSpecDispatchReport(out)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"tick: 3 candidate(s), roster claude-sonnet 1/2, deepseek-flash 2/2, 1 dispatched, 1 refused, 0 planning, 1 skipped, 0 failed",
		"dispatched: gt-a: slung to gastown/p",
		"refused: gt-b: spec lint refused: ## Gate",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if got := formatSpecDispatchReport([]byte(`{"hold":"town ESTOP active"}`)); len(got) != 1 || got[0] != "held: town ESTOP active" {
		t.Errorf("hold = %v", got)
	}
	if got := formatSpecDispatchReport([]byte("garbage")); !strings.Contains(got[0], "unparseable") {
		t.Errorf("garbage = %v", got)
	}
}

func TestDispatchRosterSeats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		roster         string
		want           []townhealth.DispatchSeat
		wantUnreadable bool
	}{
		{"claude-sonnet 1/2, deepseek-flash 2/2", []townhealth.DispatchSeat{{Live: 1, Cap: 2}, {Live: 2, Cap: 2}}, false},
		{"claude-sonnet 0/1", []townhealth.DispatchSeat{{Live: 0, Cap: 1}}, false},
		{"no seats", nil, false},
		// A roster the daemon cannot read in full must not read as a full
		// town, so each unreadable shape is reported rather than dropped
		// (gt-xiw7o).
		{"", nil, true},
		{"claude-sonnet 1/2, junk", nil, true},
		{"claude-sonnet x/2", nil, true},
	} {
		got, unreadable := dispatchRosterSeats(tc.roster)
		if !reflect.DeepEqual(got, tc.want) || unreadable != tc.wantUnreadable {
			t.Errorf("dispatchRosterSeats(%q) = %+v, %v, want %+v, %v", tc.roster, got, unreadable, tc.want, tc.wantUnreadable)
		}
	}
}

// The daemon keeps the ticker's recent decisions for townhealth's dispatch
// field: what each tick saw, the roster it decided against, and what it slung
// (gt-xiw7o).
func TestRecordDispatchTickRecordsTheDecision(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d := &Daemon{}
	d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet 1/2, deepseek-flash 2/2", "candidates": 4, "dispatched": [{"line": "gt-a"}]}`), at)

	got := d.dispatchTickRecords()
	want := []townhealth.DispatchTick{{
		At:         at,
		Candidates: 4,
		Seats:      []townhealth.DispatchSeat{{Live: 1, Cap: 2}, {Live: 2, Cap: 2}},
		Dispatched: 1,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded %+v, want %+v", got, want)
	}
}

// The recorded tick carries the decisions that tell a stalled dispatcher from
// one that turned its candidates away on purpose, and an unreadable roster
// from a full town (gt-xiw7o).
func TestRecordDispatchTickRecordsTheDeclines(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d := &Daemon{}
	d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet 1/2", "candidates": 4, "dispatched": null,
		"refused": [{"line": "gt-a"}], "planning": [{"line": "gt-b"}],
		"skipped": [{"line": "gt-c"}], "failed": [{"line": "gt-d"}]}`), at)

	got := d.dispatchTickRecords()
	want := []townhealth.DispatchTick{{
		At:         at,
		Candidates: 4,
		Seats:      []townhealth.DispatchSeat{{Live: 1, Cap: 2}},
		Refused:    1, Planning: 1, Skipped: 1, Failed: 1,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded %+v, want %+v", got, want)
	}
}

// A tick whose roster the daemon cannot read is recorded unreadable, not as a
// town with every seat taken (gt-xiw7o).
func TestRecordDispatchTickRecordsAnUnreadableRoster(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d := &Daemon{}
	d.recordDispatchTick(tickReport(t, `{"roster": "", "candidates": 0, "dispatched": null}`), at)

	got := d.dispatchTickRecords()
	if len(got) != 1 || !got[0].RosterUnreadable {
		t.Errorf("recorded %+v, want one tick with an unreadable roster", got)
	}
}

// The history is bounded: the daemon keeps the newest dispatchTickHistory
// ticks, which is far more than the health window can reach (gt-xiw7o).
func TestRecordDispatchTickKeepsTheNewestHistory(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d := &Daemon{}
	report := tickReport(t, `{"roster": "claude-sonnet 1/2", "candidates": 1}`)
	for i := 0; i < dispatchTickHistory+5; i++ {
		d.recordDispatchTick(report, at.Add(time.Duration(i)*time.Second))
	}

	got := d.dispatchTickRecords()
	if len(got) != dispatchTickHistory {
		t.Fatalf("kept %d ticks, want %d", len(got), dispatchTickHistory)
	}
	if want := at.Add(5 * time.Second); !got[0].At.Equal(want) {
		t.Errorf("oldest kept = %v, want %v (the records before it dropped)", got[0].At, want)
	}
}

func TestTriggerSpecDispatchSingleFlight(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	d.specDispatchRunning.Store(true)
	if d.triggerSpecDispatch() {
		t.Fatal("a second tick started while one was running")
	}
	d.specDispatchRunning.Store(false)
	// No config: the tick returns without shelling out.
	if !d.triggerSpecDispatch() {
		t.Fatal("tick did not start")
	}
	d.specDispatchCycles.Wait()
	if d.specDispatchRunning.Load() {
		t.Fatal("guard not released")
	}
}
