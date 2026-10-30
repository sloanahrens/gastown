package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
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
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: boolPtr(false)}}}
	if IsPatrolEnabled(off, "spec_dispatch") {
		t.Error("spec_dispatch enabled:false must be off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: boolPtr(true)}}}
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
	if got := (&Daemon{}).formatSpecDispatchReport(wireOut(`{"candidates": 3, "labeled_failed": 2}`)); !strings.Contains(got[0], "2 held by the failed label") {
		t.Errorf("tick line = %q, want it to carry the 2 beads held by the label", got[0])
	}
	if got := (&Daemon{}).formatSpecDispatchReport(wireOut(`{"notices":["gastown: ignoring stale revert: revert of gt-cul has been building for 31m"]}`)); len(got) != 2 || got[1] != "note: gastown: ignoring stale revert: revert of gt-cul has been building for 31m" {
		t.Errorf("notices = %v", got)
	}
	if got := (&Daemon{}).formatSpecDispatchReport(wireOut(`{"hold":"town ESTOP active"}`)); len(got) != 1 || got[0] != "held: town ESTOP active" {
		t.Errorf("hold = %v", got)
	}
	if got := (&Daemon{}).formatSpecDispatchReport([]byte("garbage")); !strings.Contains(got[0], "unparseable") {
		t.Errorf("garbage = %v", got)
	}
}

// An urgent bead dispatched through an overlap logs its bypass on its own line
// and the tick line counts it, so the dashboard shows the hold the priority
// answered (gt-iydx1).
func TestFormatSpecDispatchReportLogsOverlapBypass(t *testing.T) {
	t.Parallel()
	out := wireOut(`{"candidates": 1,
		"dispatched": [{"bead": "gt-u", "line": "gt-u: slung to gastown/p"}],
		"bypassed": [{"bead": "gt-u", "line": "gt-u: overlap hold bypassed for P0 (would be held by gt-t61ft held by gastown/polecats/capable)"}]}`)
	lines := (&Daemon{}).formatSpecDispatchReport(out)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(lines[0], "1 overlap hold bypassed") {
		t.Errorf("tick line %q does not count the bypass", lines[0])
	}
	if !strings.Contains(joined, "bypassed: gt-u: overlap hold bypassed for P0") {
		t.Errorf("bypass line missing from:\n%s", joined)
	}
	if strings.Contains(joined, "deferred: gt-u") {
		t.Errorf("a bypassed sling was logged as deferred:\n%s", joined)
	}
}

// The tick line names every skipped bead with its reason after the counts, so
// the feed says why a candidate did not dispatch instead of only how many
// stayed ready (gt-gzav5).
func TestFormatSpecDispatchReportNamesSkippedBeads(t *testing.T) {
	t.Parallel()
	out := wireOut(`{"candidates": 4, "skipped": [
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
	out := wireOut(`{"candidates": 8, "skipped": [
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

// A sling-stage deferral past the tick line's cap is still logged with its bead
// and reason: the tick line names six skips and counts the rest as "+N more",
// which said neither which bead was deferred nor why (gt-gi3f9, gt-4wcqd).
func TestFormatSpecDispatchReportLogsDeferralsOutsideTheCap(t *testing.T) {
	t.Parallel()
	out := wireOut(`{"candidates": 8,
		"skipped": [
			{"bead": "gt-a", "line": "gt-a: no seat: full"},
			{"bead": "gt-b", "line": "gt-b: no seat: full"},
			{"bead": "gt-c", "line": "gt-c: no seat: full"},
			{"bead": "gt-d", "line": "gt-d: no seat: full"},
			{"bead": "gt-e", "line": "gt-e: no seat: full"},
			{"bead": "gt-f", "line": "gt-f: no seat: full"},
			{"bead": "gt-g", "line": "gt-g: no seat: full"},
			{"bead": "gt-h", "line": "gt-h: sling deferred, content overlap holds it (work in flight): shares TestX"}
		],
		"deferred": [
			{"bead": "gt-h", "line": "gt-h: sling deferred, content overlap holds it (work in flight): shares TestX"}
		],
		"failed": [
			{"bead": "gt-z", "line": "gt-z: dispatch failed, left unassigned: Error 1213 after 4 attempts; excluded from the queue until label x is cleared"}
		]}`)
	lines := (&Daemon{}).formatSpecDispatchReport(out)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"deferred: gt-h: sling deferred, content overlap holds it (work in flight): shares TestX",
		"failed: gt-z: dispatch failed, left unassigned: Error 1213 after 4 attempts",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// The tick line is unchanged: it still caps its skip list and counts the
	// tail, so the deferral is not named there, only on its own line.
	tick := lines[0]
	if !strings.Contains(tick, " +2 more") {
		t.Errorf("tick line %q lost its cap count", tick)
	}
	if strings.Contains(tick, "gt-h (") {
		t.Errorf("tick line %q names gt-h; the cap and its count must be unchanged", tick)
	}
	if n := strings.Count(joined, "deferred: gt-h:"); n != 1 {
		t.Errorf("deferral logged %d times, want once", n)
	}
}

// A sling that fails after its retries reaches the daemon log as one 'failed:'
// line naming the bead and the reason, and no 'deferred:' line: the tick line
// carries only the count, so that line is how an operator sees what the failed
// label is holding (gt-4wcqd).
func TestSpecDispatchTickLogsAFailedSling(t *testing.T) {
	t.Parallel()
	const report = `{"candidates": 1, "failed": [
		{"bead": "gt-z", "line": "gt-z: dispatch failed, left unassigned: Error 1213 after 4 attempts; excluded from the queue until label x is cleared"}
	]}`
	fake := newFakeCLI(func([]string) cliReply { return cliReply{stdout: string(wireOut(report))} })
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)

	d.runSpecDispatch()

	got := lines()
	if n := countContaining(got, "spec_dispatch: failed: gt-z", "Error 1213 after 4 attempts"); n != 1 {
		t.Errorf("failed lines = %d, want one naming the bead and the reason: %v", n, got)
	}
	if n := countContaining(got, "deferred:"); n != 0 {
		t.Errorf("deferred lines = %d, want none: a failed sling is not a deferral: %v", n, got)
	}
}

// A dry run decides without slinging: the daemon asks the tick for --dry-run,
// reads the beads it would sling as next, and logs neither a 'deferred:' nor a
// 'failed:' line, because it takes those from the report's deferred and failed
// lists and a dry run has none (gt-4wcqd).
func TestSpecDispatchDryRunDecidesWithoutSlinging(t *testing.T) {
	t.Parallel()
	const report = `{"candidates": 2,
		"dispatched": [{"bead": "gt-a", "line": "gt-a: would sling to gastown on claude-sonnet (free seat)"}],
		"skipped": [{"bead": "gt-b", "line": "gt-b: no seat: claude-sonnet 1/1 live"}]}`
	fake := newFakeCLI(func([]string) cliReply { return cliReply{stdout: string(wireOut(report))} })
	logger, _ := lineLogger()
	d := specDispatchFake(t, fake, logger)

	left, err := d.overseerReportLeft()
	if err != nil {
		t.Fatalf("overseerReportLeft: %v", err)
	}
	if got := fake.argvs("spec", "dispatch"); len(got) != 1 || !slices.Contains(got[0], "--dry-run") {
		t.Errorf("dry run argv = %v, want one tick asking for --dry-run", got)
	}
	if len(left.Next) != 2 || left.Next[0].Bead != "gt-a" || len(left.Held) != 0 {
		t.Errorf("LEFT next = %+v held = %+v, want gt-a and the seatless gt-b next, nothing held", left.Next, left.Held)
	}
	joined := strings.Join((&Daemon{}).formatSpecDispatchReport(wireOut(report)), "\n")
	for _, unwanted := range []string{"deferred:", "failed:"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("dry-run report logs %q:\n%s", unwanted, joined)
		}
	}
}

// An unshaped bead stays ready and is held again every tick, so its warning
// lands once — naming the missing headings — and the next tick's tick line
// still names it without a second warning (gt-gzav5).
func TestFormatSpecDispatchReportWarnsOnceAboutUnshaped(t *testing.T) {
	t.Parallel()
	out := wireOut(`{"candidates": 2, "skipped": [
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
		// A seat carrying the dead-hooked breakdown Picture renders still reads
		// as its count (gt-tldj4).
		{"claude-sonnet 1/2, deepseek-flash 3/3 (1 dead-hooked)", []townhealth.DispatchSeat{{Live: 1, Cap: 2}, {Live: 3, Cap: 3}}, false},
		// A seat carrying two parenthetical notes, a dead-hooked count and a
		// closed window, still reads as its count (gt-rredd).
		{"claude-haiku 1/1, deepseek-flash 1/1 (1 dead-hooked) (closed until 08:00)", []townhealth.DispatchSeat{{Live: 1, Cap: 1}, {Live: 1, Cap: 1}}, false},
		// A comma inside a note does not split the seat it belongs to.
		{"deepseek-flash 1/1 (closed 20:00-08:00, 1 dead-hooked)", []townhealth.DispatchSeat{{Live: 1, Cap: 1}}, false},
		{"deepseek-flash 1/1 (closed 20:00-08:00, 1 dead-hooked), claude-sonnet 0/2 (closed until 08:00)", []townhealth.DispatchSeat{{Live: 1, Cap: 1}, {Live: 0, Cap: 2}}, false},
		{"no seats", nil, false},
		// The town-total segment caps the town, not a seat: a full town's
		// roster reads as its seats only, so no free seat is counted (gt-penmy).
		{"claude-sonnet 2/2, total 2/10", []townhealth.DispatchSeat{{Live: 2, Cap: 2}}, false},
		{"claude-sonnet 2/2, deepseek-flash 1/2, total 3/10", []townhealth.DispatchSeat{{Live: 2, Cap: 2}, {Live: 1, Cap: 2}}, false},
		// Picture says "no seats" for none, so a lone "total" segment is a seat
		// named total, as is one anywhere but last (gt-38gsb).
		{"total 2/10", []townhealth.DispatchSeat{{Live: 2, Cap: 10}}, false},
		{"total 1/2, claude-sonnet 2/2, total 3/10", []townhealth.DispatchSeat{{Live: 1, Cap: 2}, {Live: 2, Cap: 2}}, false},
		// A last "total" segment counting fewer live than the seats before it
		// cannot be the town's total, so it is a seat.
		{"claude-sonnet 2/2, total 1/3", []townhealth.DispatchSeat{{Live: 2, Cap: 2}, {Live: 1, Cap: 3}}, false},
		// The total's live count may exceed the seats' (a polecat on an agent no
		// seat names), and an idle town's total is zero against zero.
		{"claude-sonnet 1/2, total 3/10", []townhealth.DispatchSeat{{Live: 1, Cap: 2}}, false},
		{"claude-sonnet 0/2, total 0/10", []townhealth.DispatchSeat{{Live: 0, Cap: 2}}, false},
		// Picture appends a total only under a positive cap, so a last segment
		// with a zero cap is a seat.
		{"claude-sonnet 2/2, total 2/0", []townhealth.DispatchSeat{{Live: 2, Cap: 2}, {Live: 2, Cap: 0}}, false},
		// Known limit: a seat named total that sorts last with live at or above
		// the seats' sum cannot be told from the total, and is read as the total.
		{"claude-sonnet 1/2, total 1/3", []townhealth.DispatchSeat{{Live: 1, Cap: 2}}, false},
		// A last segment shaped like the total but malformed is unreadable.
		{"claude-sonnet 2/2, total", nil, true},
		{"claude-sonnet 2/2, total x/10", nil, true},
		{"claude-sonnet 2/2, total 2/10 (extra)", nil, true},
		// A roster the daemon cannot read in full must not read as a full
		// town, so each unreadable shape is reported rather than dropped
		// (gt-xiw7o).
		{"", nil, true},
		{"claude-sonnet 1/2, junk", nil, true},
		{"claude-sonnet x/2", nil, true},
		// A seat whose only text is notes has no count to read (gt-rredd).
		{"(1 dead-hooked) (closed until 08:00)", nil, true},
		{"deepseek-flash (1 dead-hooked) (closed until 08:00)", nil, true},
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

// gt-cbaoy: while an upgrade restart waits for the tick in flight, no new tick
// starts; one that started would outlast the wait and be cut by the restart.
func TestTriggerSpecDispatchDefersWhileARestartIsPending(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	d.upgradeRestartPending.Store(true)
	if d.triggerSpecDispatch() {
		t.Fatal("a tick started while an upgrade restart is pending")
	}
	if d.specDispatchRunning.Load() {
		t.Fatal("a deferred tick left its single-flight guard set")
	}
	d.upgradeRestartPending.Store(false)
	if !d.triggerSpecDispatch() {
		t.Fatal("tick did not start once the restart was no longer pending")
	}
	d.specDispatchCycles.Wait()
}

// gt-32j1y: a tick's start belongs to that tick alone. A skipped tick leaves the
// running tick's start in place, and a finished tick clears its own.
func TestTriggerSpecDispatchPublishesAndClearsItsClock(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	running := &specDispatchTickRun{started: time.Unix(100, 0)}
	d.specDispatchRunning.Store(true)
	d.specDispatchTick.Store(running)
	if d.triggerSpecDispatch() {
		t.Fatal("a second tick started while one was running")
	}
	if got := d.specDispatchTick.Load(); got != running {
		t.Fatalf("the skipped tick replaced the running tick's start: %+v", got)
	}

	d.specDispatchRunning.Store(false)
	d.specDispatchTick.Store(nil)
	if !d.triggerSpecDispatch() {
		t.Fatal("tick did not start")
	}
	d.specDispatchCycles.Wait()
	if got := d.specDispatchTick.Load(); got != nil {
		t.Fatalf("the tick's start outlived the tick: %+v", got)
	}
}

// gt-32j1y: the deferral follows the flag checkUpgradeRestart sets, not one a
// test poked: while a restart the check marked pending is held by other work,
// no tick starts to outlast it.
func TestTriggerSpecDispatchDefersForTheRestartCheckPending(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.logger = log.New(io.Discard, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})
	// A post-land run holds the restart and lets the check mark it pending.
	d.postLandRuns.Add(1)
	if d.checkUpgradeRestart(time.Now()) {
		t.Fatal("restarted under a post-land run")
	}
	if !d.upgradeRestartPending.Load() {
		t.Fatal("the restart check did not mark the restart pending")
	}
	if d.triggerSpecDispatch() {
		t.Fatal("a tick started while the restart check had a restart pending")
	}
	if d.specDispatchRunning.Load() {
		t.Fatal("a deferred tick left its single-flight guard set")
	}
}

// A dispatcher parked by the operator hold file starts no tick: the tick
// would decide nothing, and its in-flight flag is a daemonWorkHolds entry, so
// a parked tick made the daemon read as busy for the catch-up backup's quiet
// check (gt-y6ovz).
func TestTriggerSpecDispatchParkedTicksNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeOperatorHold(t, townRoot)
	fake := newFakeCLI(nil)
	logger, lines := lineLogger()
	d := &Daemon{
		config:       &Config{TownRoot: townRoot},
		ctx:          context.Background(),
		logger:       logger,
		execCmd:      fake.run,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: boolPtr(true)}}},
	}

	for i := 0; i < 3; i++ {
		if d.triggerSpecDispatch() {
			t.Fatal("a parked dispatcher started a tick")
		}
	}
	if calls := fake.argvs("spec", "dispatch"); len(calls) != 0 {
		t.Errorf("a parked dispatcher shelled out: %v", calls)
	}
	if !d.daemonWorkIdle() {
		t.Error("a parked dispatcher reads as daemon work in flight")
	}
	if n := countContaining(lines(), "spec_dispatch: not ticking:"); n != 1 {
		t.Errorf("park line count = %d, want one for the hold, not one per tick", n)
	}

	// Lifting the hold resumes the ticker, and the next tick leaves no hold
	// behind it.
	if err := os.Remove(dispatch.HoldFilePath(townRoot)); err != nil {
		t.Fatal(err)
	}
	if !d.triggerSpecDispatch() {
		t.Fatal("the ticker did not resume after the hold lifted")
	}
	d.specDispatchCycles.Wait()
	if !d.daemonWorkIdle() {
		t.Error("the daemon is not idle after the resumed tick")
	}
	if n := countContaining(lines(), "operator dispatch hold lifted"); n != 1 {
		t.Errorf("lift line count = %d, want one", n)
	}
}

// gt-icmdq: the idle streak counts consecutive ticks that each observed the
// town. A tick that never ran under the operator hold, one that failed, and one
// whose report cannot be read each observed nothing, so the run ends there and
// the next idle tick counts from one instead of completing a streak the ticker
// never kept.
func TestSpecDispatchEndsTheIdleStreakWhenNoTickIsObserved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		hold      bool
		reply     cliReply
		wantStart bool
	}{
		{"the town hold", true, cliReply{}, false},
		{"a held report", false, cliReply{stdout: `{"hold": "operator dispatch hold present"}`}, true},
		{"a failed tick", false, cliReply{code: 1, stderr: "boom"}, true},
		{"an unreadable report", false, cliReply{stdout: "garbage"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tc.hold {
				writeOperatorHold(t, townRoot)
			}
			fake := newFakeCLI(func([]string) cliReply { return tc.reply })
			d := &Daemon{
				config:       &Config{TownRoot: townRoot},
				ctx:          context.Background(),
				logger:       log.New(io.Discard, "", 0),
				execCmd:      fake.run,
				patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: boolPtr(true)}}},
			}
			idle := idleTick(overlapLine)
			at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
			for i := 0; i < attentionDispatchIdleTicks-1; i++ {
				d.dispatchIdle.Observe(idle, at.Add(time.Duration(i)*time.Minute))
			}
			if started := d.triggerSpecDispatch(); started != tc.wantStart {
				t.Fatalf("tick started = %v, want %v", started, tc.wantStart)
			}
			d.specDispatchCycles.Wait()

			if got := d.dispatchIdle.Status().Streak; got != 0 {
				t.Fatalf("Streak = %d after a tick that observed nothing, want 0", got)
			}
			// The next idle tick counts from one, a minute on and so inside the
			// ticker's own cadence: four idle ticks, the tick that observed
			// nothing and one more must not raise the five-tick item.
			d.dispatchIdle.Observe(idle, at.Add(4*time.Minute))
			if got := d.dispatchIdle.Status().Streak; got != 1 {
				t.Fatalf("Streak = %d after one idle tick on, want 1", got)
			}
		})
	}
}

// gt-icmdq: a seat picture that will not parse starts no tick, so the run of
// idle ticks ends there rather than resuming across the ticks that never ran.
func TestSpecDispatchSeatsUnreadableEndsTheIdleStreak(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 2))
	d, _, _ := specSeatDaemon(t, root)
	writeSpecSeatBody(t, root, `{"type": "town-settings", "version": 1, "polecat_pool": {"seats":`)

	idle := idleTick(overlapLine)
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < attentionDispatchIdleTicks-1; i++ {
		d.dispatchIdle.Observe(idle, at.Add(time.Duration(i)*time.Minute))
	}
	if specSeatTick(d) {
		t.Fatal("the ticker ticked on a seat file that does not parse")
	}
	if got := d.dispatchIdle.Status().Streak; got != 0 {
		t.Fatalf("Streak = %d after a tick the seat file stopped, want 0", got)
	}
}

// The tick line states how many pool beads' design and notes the tick showed
// and how many the daemon's cache answered, and how many the per-tick limit
// left unread; a tick that kept no cache adds nothing (gt-l0vlf).
func TestSpecDispatchTickLineStatesDetailReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"warm", `{"candidates": 2, "details_fetched": 0, "details_cached": 180}`, "; design/notes: 0 fetched, 180 cached"},
		{"cold", `{"candidates": 2, "details_fetched": 40, "details_cached": 0, "details_unread": 140}`, "; design/notes: 40 fetched, 0 cached, 140 unread"},
		{"none", `{"candidates": 2}`, ""},
	} {
		lines := (&Daemon{}).formatSpecDispatchReport(wireOut(tc.out))
		got := lines[0]
		if tc.want == "" {
			if strings.Contains(got, "design/notes") {
				t.Errorf("%s: tick line %q names design/notes reads though the tick kept no cache", tc.name, got)
			}
			continue
		}
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("%s: tick line %q does not end %q", tc.name, got, tc.want)
		}
	}
}

// A candidate the tick skips because a blocking dependency is unresolved
// reaches the tick line the operator reads, naming the blocker and the status
// the read gave it (gt-ndad8).
func TestSpecDispatchTickLineNamesABlockedSkip(t *testing.T) {
	t.Parallel()
	out := `{"roster": "claude-sonnet 1/2", "candidates": 1, "dispatched": [], "refused": [], "planning": [],` +
		`"skipped": [{"bead": "gt-ndad8", "line": "gt-ndad8: blocked by gt-frtad (deferred)"}], "failed": []}`
	lines := (&Daemon{}).formatSpecDispatchReport(wireOut(out))
	if len(lines) == 0 {
		t.Fatal("a tick with one blocked candidate logged nothing")
	}
	if want := "skipped: gt-ndad8 (blocked by gt-frtad (deferred))"; !strings.Contains(lines[0], want) {
		t.Errorf("tick line %q does not name %q", lines[0], want)
	}
}

// The tick the daemon starts names the daemon to the cache it keeps between
// ticks, so a restarted daemon starts it empty (gt-l0vlf).
func TestSpecDispatchCommandNamesTheCacheOwner(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(nil)
	d := &Daemon{ctx: t.Context(), config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0), gtPath: "gt", execCmd: gt.run}
	_, _ = d.runSpecDispatchCommand(time.Minute)
	calls := gt.recorded()
	if len(calls) != 1 {
		t.Fatalf("recorded %d gt calls, want 1", len(calls))
	}
	want := dispatch.SpecDetailOwnerEnv + "=" + specDetailOwner
	for _, kv := range calls[0].env {
		if kv == want {
			return
		}
	}
	t.Errorf("tick env lacks %q", want)
}

// The bound is the one-sling bound plus an allowance for each sling past the
// first, so a tick slinging three beads gets more time than one slinging one,
// and never a zero bound (gt-bbfpr).
func TestSpecDispatchBoundScalesWithMaxPerTick(t *testing.T) {
	t.Parallel()
	const timeout = 5 * time.Minute
	one := specDispatchBound(timeout, 1)
	if one != timeout {
		t.Errorf("bound for one sling = %v, want %v", one, timeout)
	}
	if got := specDispatchBound(timeout, 0); got != one {
		t.Errorf("bound for max_per_tick 0 = %v, want the one-sling bound %v", got, one)
	}
	three := specDispatchBound(timeout, 3)
	if three <= one {
		t.Errorf("bound for 3 slings = %v, want more than the bound for 1 (%v)", three, one)
	}
	if want := timeout + 2*specDispatchSlingAllowance; three != want {
		t.Errorf("bound for 3 slings = %v, want %v", three, want)
	}
}

// The tick's bound reads max_per_tick from the patrol block the tick reads, and
// an absent block counts as one sling (gt-bbfpr).
func TestSpecDispatchMaxPerTickReadsThePatrolBlock(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: townRoot}}
	if got, err := d.specDispatchMaxPerTick(); err != nil || got != 1 {
		t.Errorf("no patrol block: max_per_tick = %d, err %v, want 1", got, err)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{MaxPerTick: 3}}}
	if err := SavePatrolConfig(townRoot, cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := d.specDispatchMaxPerTick(); err != nil || got != 3 {
		t.Errorf("max_per_tick = %d, err %v, want 3 from the patrol block", got, err)
	}
}

// A tick killed after it slinged logs one dispatched line per sling it hooked
// before the report it never printed, with the elapsed time and the bound.
func TestSpecDispatchKilledTickLogsEachSlingItMade(t *testing.T) {
	t.Parallel()
	partial := strings.Repeat("  ✓ "+dispatch.SlingHookedLine("p")+"\n", 2)
	fake := newFakeCLI(func([]string) cliReply { return cliReply{stdout: partial, code: 1, stderr: "signal: killed"} })
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)

	d.runSpecDispatch()

	got := lines()
	if n := countContaining(got, "spec_dispatch: dispatched:", "hooked before the tick failed"); n != 2 {
		t.Errorf("dispatched lines = %d, want one per sling hooked (2): %v", n, got)
	}
	bound := specDispatchBound(specDispatchTimeout, 1).String() + " bound"
	if n := countContaining(got, "spec_dispatch: tick failed after", bound, "signal: killed"); n != 1 {
		t.Errorf("failure line with elapsed time and bound = %d, want one: %v", n, got)
	}
}

// A tick killed before it hooked any sling logs the failure with the elapsed
// time and the bound, and no dispatched line.
func TestSpecDispatchKilledTickBeforeAnySlingLogsNoDispatch(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func([]string) cliReply { return cliReply{code: 1, stderr: "signal: killed"} })
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)

	d.runSpecDispatch()

	got := lines()
	if n := countContaining(got, "dispatched:"); n != 0 {
		t.Errorf("dispatched lines = %d, want none: %v", n, got)
	}
	if n := countContaining(got, "spec_dispatch: tick failed after", specDispatchBound(specDispatchTimeout, 1).String()+" bound"); n != 1 {
		t.Errorf("failure line with elapsed time and bound = %d, want one: %v", n, got)
	}
}

// specDispatchFake is a daemon whose tick runs against fake, in a town with no
// patrol block, so the bound is the one-sling bound.
func specDispatchFake(t *testing.T, fake *fakeCLI, logger *log.Logger) *Daemon {
	t.Helper()
	return &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		ctx:          context.Background(),
		logger:       logger,
		gtPath:       "gt",
		execCmd:      fake.run,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: boolPtr(true)}}},
	}
}

// A tick that decided something logs one 'tick took' line: the tick's length as
// the daemon timed it, then the phases the tick reported. The fake clock moves
// inside the tick, as a real tick takes time (gt-bqch0).
func TestSpecDispatchTickLogsItsDuration(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	const report = `{"candidates": 3, "roster_ns": 1000000000, "board_ns": 2000000000,
		"candidates_ns": 30000000000, "sling_ns": 5000000000}`
	fake := newFakeCLI(func([]string) cliReply {
		clock.Advance(40 * time.Second)
		return cliReply{stdout: string(wireOut(report))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = clock

	d.runSpecDispatch()

	const want = "spec_dispatch: tick took 40s (roster 1s, board 2s, candidates 3 30s, sling 5s)"
	if n := countContaining(lines(), want); n != 1 {
		t.Errorf("tick took lines = %d, want one %q: %v", n, want, lines())
	}
}

// A tick whose roster cannot be read dispatches nothing, but it still ran the
// roster phase: its 'tick took' line names that phase's time, so a slow
// failing roster read is attributed to the roster (gt-co7sr).
func TestSpecDispatchRosterFailureLogsItsDuration(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	const report = `{"candidates": 0, "roster_ns": 3000000000, "errors": ["roster unreadable, not dispatching: no tmux server"]}`
	fake := newFakeCLI(func([]string) cliReply {
		clock.Advance(4 * time.Second)
		return cliReply{stdout: string(wireOut(report))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	d.clock = clock

	d.runSpecDispatch()

	const want = "spec_dispatch: tick took 4s (roster 3s, board 0s, candidates 0 0s, sling 0s)"
	if n := countContaining(lines(), want); n != 1 {
		t.Errorf("tick took lines = %d, want one %q: %v", n, want, lines())
	}
}

// A held tick decided nothing and runs no phase, so it logs no 'tick took' line.
func TestSpecDispatchHeldTickLogsNoDuration(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func([]string) cliReply {
		return cliReply{stdout: `{"hold": "town parked by the operator"}`}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)

	d.runSpecDispatch()

	if n := countContaining(lines(), "tick took"); n != 0 {
		t.Errorf("tick took lines = %d for a held tick, want none: %v", n, lines())
	}
}

// The 'tick took' line ends with the bd calls each phase made, after the times
// it already named, so a phase that is slow is also read by what it cost
// (gt-139dp).
func TestSpecDispatchTickTookLineNamesItsPhaseCalls(t *testing.T) {
	t.Parallel()
	const report = `{"candidates": 3, "roster_ns": 1000000000, "board_ns": 2000000000,
		"candidates_ns": 30000000000, "sling_ns": 5000000000,
		"roster_calls": 41, "board_calls": 20, "candidates_calls": 15}`
	r, err := parseSpecDispatchReport(wireOut(report))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := specDispatchTookLine(r, 40*time.Second)
	const want = "tick took 40s (roster 1s, board 2s, candidates 3 30s, sling 5s); roster 41 calls, board 20 calls, candidates 15 calls"
	if got != want {
		t.Fatalf("tick took line = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, "roster 41 calls, board 20 calls, candidates 15 calls") {
		t.Errorf("tick took line %q does not end with the phase calls", got)
	}
}

// A tick's census file is logged once the tick returns, as the daemon's census
// line, naming the busiest callers of the subprocess's bd reads (gt-139dp).
func TestSpecDispatchLogsItsCensus(t *testing.T) {
	t.Parallel()
	fake := newFakeCLIFor(func(c cliCall) cliReply {
		rows := []beads.CensusRow{
			{Verb: "show", Caller: "cmd.specRoster", Count: 30},
			{Verb: "mol wisp", Caller: "cmd.specBoard", Count: 12},
		}
		if err := beads.WriteCensusSink(dispatch.SpecCensusPath(c.dir), rows); err != nil {
			t.Errorf("writing the tick's census: %v", err)
		}
		return cliReply{stdout: string(wireOut(`{"candidates": 1}`))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)

	d.runSpecDispatch()

	const want = "spec_dispatch: bd census (spec dispatch): total=42 top: show@cmd.specRoster=30 mol wisp@cmd.specBoard=12"
	if n := countContaining(lines(), want); n != 1 {
		t.Errorf("census lines = %d, want one %q: %v", n, want, lines())
	}
}

// The census file is this tick's alone: rows a killed tick left behind are
// cleared before the next one runs, so a tick that wrote nothing logs nothing
// rather than the last one's calls (gt-139dp).
func TestSpecDispatchKilledTickLogsNoStaleCensus(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func([]string) cliReply { return cliReply{code: 1, stderr: "signal: killed"} })
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)
	stale := []beads.CensusRow{{Verb: "show", Caller: "cmd.specRoster", Count: 9}}
	if err := beads.WriteCensusSink(d.specDispatchCensusPath(), stale); err != nil {
		t.Fatal(err)
	}

	d.runSpecDispatch()

	if n := countContaining(lines(), "bd census (spec dispatch)"); n != 0 {
		t.Errorf("census lines = %d after a killed tick, want none: %v", n, lines())
	}
}

// A clean tick that leaves no census file is reported: the tick is told where
// to write, so a missing file is a census lost, not a tick that read nothing
// (gt-139dp).
func TestSpecDispatchReportsACensusItCannotRead(t *testing.T) {
	t.Parallel()
	fake := newFakeCLI(func([]string) cliReply {
		return cliReply{stdout: string(wireOut(`{"candidates": 1}`))}
	})
	logger, lines := lineLogger()
	d := specDispatchFake(t, fake, logger)

	d.runSpecDispatch()

	if n := countContaining(lines(), "bd census unread after the tick"); n != 1 {
		t.Errorf("census error lines = %d, want one: %v", n, lines())
	}
}

// wireOut completes a fragment of a tick's JSON with the keys the dispatcher
// always writes, so a test states only the fields it is about. A fragment that
// is not a JSON object is returned as it is, and the parse reports it.
func wireOut(fragment string) []byte {
	var fields map[string]any
	if err := json.Unmarshal([]byte(fragment), &fields); err != nil {
		return []byte(fragment)
	}
	if _, ok := fields["roster"]; !ok {
		fields["roster"] = ""
	}
	if _, ok := fields["candidates"]; !ok {
		fields["candidates"] = 0
	}
	for _, k := range []string{"dispatched", "refused", "planning", "skipped", "failed"} {
		if _, ok := fields[k]; !ok {
			fields[k] = nil
		}
	}
	out, _ := json.Marshal(fields)
	return out
}

// A report missing one of the keys the dispatcher always writes is an error,
// not a report whose field reads as its zero value. The full report is the
// control: it parses.
func TestParseSpecDispatchReportRequiresTheAlwaysWrittenKeys(t *testing.T) {
	t.Parallel()
	const full = `{"roster": "claude-sonnet 1/2", "candidates": 1, "dispatched": null, "refused": null, "planning": null, "skipped": null, "failed": null}`
	for _, tc := range []struct {
		name    string
		out     string
		wantErr string
	}{
		{"the full report parses", full, ""},
		{"no roster", strings.Replace(full, `"roster": "claude-sonnet 1/2", `, "", 1), "roster"},
		{"no candidates", strings.Replace(full, `"candidates": 1, `, "", 1), "candidates"},
		{"no failed list", strings.Replace(full, `, "failed": null`, "", 1), "failed"},
		{"no skipped list", strings.Replace(full, `"skipped": null, `, "", 1), "skipped"},
		{"a tick line before the report", "sling progress\n" + full, ""},
		{"not an object", `["roster"]`, "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSpecDispatchReport([]byte(tc.out))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("parse: %v, want success", err)
			case tc.wantErr == "parse" && err == nil:
				t.Fatal("parse succeeded, want an error")
			case tc.wantErr != "" && tc.wantErr != "parse" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("parse error = %v, want it to name the missing %q", err, tc.wantErr)
			}
		})
	}
}
