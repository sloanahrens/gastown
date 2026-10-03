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
	lines := (&Daemon{}).formatSpecDispatchReport(out)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"tick: 3 candidate(s), roster claude-sonnet 1/2, deepseek-flash 2/2, 1 dispatched, 1 refused, 0 planning, 1 skipped, 0 failed, 0 held by the failed label",
		"dispatched: gt-a: slung to gastown/p",
		"refused: gt-b: spec lint refused: ## Gate",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// The tick line carries the beads the spec-dispatch-failed label is
	// holding out of the queue: a tick that dispatched nothing but left two
	// beads stuck must say so in the daemon log, not only in the health field
	// (om, gt-q6zoo attempt 1).
	if got := (&Daemon{}).formatSpecDispatchReport([]byte(`{"candidates": 3, "labeled_failed": 2}`)); !strings.Contains(got[0], "2 held by the failed label") {
		t.Errorf("tick line = %q, want it to carry the 2 beads held by the label", got[0])
	}
	if got := (&Daemon{}).formatSpecDispatchReport([]byte(`{"notices":["gastown: ignoring stale revert: revert of gt-cul has been building for 31m"]}`)); len(got) != 2 || got[1] != "note: gastown: ignoring stale revert: revert of gt-cul has been building for 31m" {
		t.Errorf("notices = %v", got)
	}
	if got := (&Daemon{}).formatSpecDispatchReport([]byte(`{"hold":"town ESTOP active"}`)); len(got) != 1 || got[0] != "held: town ESTOP active" {
		t.Errorf("hold = %v", got)
	}
	if got := (&Daemon{}).formatSpecDispatchReport([]byte("garbage")); !strings.Contains(got[0], "unparseable") {
		t.Errorf("garbage = %v", got)
	}
}

// The tick line names every skipped bead with its reason after the counts, so
// the feed says why a candidate did not dispatch instead of only how many
// stayed ready (gt-gzav5).
func TestFormatSpecDispatchReportNamesSkippedBeads(t *testing.T) {
	t.Parallel()
	out := []byte(`{"candidates": 4, "skipped": [
		{"bead": "gt-a", "line": "gt-a: unshaped: ## Goal, acceptance"},
		{"bead": "gt-b", "line": "gt-b: no seat: claude-sonnet 2/2 live"},
		{"bead": "gt-c", "line": "gt-c: clean, per-tick limit 1 reached"},
		{"bead": "gt-d", "line": "gt-d: held: open child gt-e"}
	]}`)
	lines := (&Daemon{}).formatSpecDispatchReport(out)
	tick := lines[0]
	for _, want := range []string{
		"4 skipped",
		"gt-a (unshaped: ## Goal, acceptance)",
		"gt-b (no seat: claude-sonnet 2/2 live)",
		"gt-c (clean, per-tick limit 1 reached)",
		"gt-d (held: open child gt-e)",
	} {
		if !strings.Contains(tick, want) {
			t.Errorf("tick line %q missing %q", tick, want)
		}
	}
}

// The skipped list is capped: at most specDispatchSkipCap beads are named and
// the rest are counted, so one tick with a long board still logs one line
// (gt-gzav5).
func TestFormatSpecDispatchReportCapsSkippedBeads(t *testing.T) {
	t.Parallel()
	out := []byte(`{"candidates": 8, "skipped": [
		{"bead": "gt-a", "line": "gt-a: no seat: full"},
		{"bead": "gt-b", "line": "gt-b: no seat: full"},
		{"bead": "gt-c", "line": "gt-c: no seat: full"},
		{"bead": "gt-d", "line": "gt-d: no seat: full"},
		{"bead": "gt-e", "line": "gt-e: no seat: full"},
		{"bead": "gt-f", "line": "gt-f: no seat: full"},
		{"bead": "gt-g", "line": "gt-g: no seat: full"},
		{"bead": "gt-h", "line": "gt-h: no seat: full"}
	]}`)
	tick := (&Daemon{}).formatSpecDispatchReport(out)[0]
	for _, want := range []string{"gt-f (no seat: full)", " +2 more"} {
		if !strings.Contains(tick, want) {
			t.Errorf("tick line %q missing %q", tick, want)
		}
	}
	for _, beyond := range []string{"gt-g (", "gt-h ("} {
		if strings.Contains(tick, beyond) {
			t.Errorf("tick line %q names %q past the cap of %d", tick, beyond, specDispatchSkipCap)
		}
	}
}

// An unshaped bead stays ready and is held again every tick, so its warning
// lands once — naming the missing headings — and the next tick's tick line
// still names it without a second warning (gt-gzav5).
func TestFormatSpecDispatchReportWarnsOnceAboutUnshaped(t *testing.T) {
	t.Parallel()
	out := []byte(`{"candidates": 2, "skipped": [
		{"bead": "gt-a", "line": "gt-a: unshaped: ## Gate, ## Size"},
		{"bead": "gt-b", "line": "gt-b: no seat: claude-sonnet 2/2 live"}
	]}`)
	d := &Daemon{}

	first := d.formatSpecDispatchReport(out)
	if !warnsAbout(first, "gt-a", "unshaped: ## Gate, ## Size") {
		t.Errorf("first tick = %v, want one warning naming gt-a's missing headings", first)
	}
	for _, line := range first {
		if strings.Contains(line, "gt-b") && strings.HasPrefix(line, "warning:") {
			t.Errorf("a bead skipped for a reason other than shape warned: %q", line)
		}
	}

	second := d.formatSpecDispatchReport(out)
	if !strings.Contains(second[0], "gt-a (unshaped: ## Gate, ## Size)") {
		t.Errorf("second tick = %q, want the skip still named on the tick line", second[0])
	}
	for _, line := range second {
		if strings.HasPrefix(line, "warning:") {
			t.Errorf("second tick warned again: %v", second)
		}
	}
}

// warnsAbout reports whether lines carry one warning for bead that names
// reason.
func warnsAbout(lines []string, bead, reason string) bool {
	want := "warning: " + bead + " " + reason
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
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

// The recorded tick carries the count of ready beads the spec-dispatch-failed
// label is holding out of the queue, which the health field puts in front of
// the operator (gt-q6zoo).
func TestRecordDispatchTickRecordsTheLabeledFailures(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d := &Daemon{}
	d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet 1/2", "candidates": 4, "labeled_failed": 3}`), at)

	got := d.dispatchTickRecords()
	if len(got) != 1 || got[0].LabeledFailed != 3 {
		t.Errorf("recorded %+v, want one tick with 3 ready beads held by the label", got)
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
