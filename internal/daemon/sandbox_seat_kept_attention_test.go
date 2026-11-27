package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// keptSeatAttentionNow is the tick every collection in these tests judges
// against, so no test reads the wall clock.
var keptSeatAttentionNow = time.Date(2026, 11, 12, 22, 0, 0, 0, time.UTC)

// keptSeatSince writes a kept record for gastown/name whose keep began age ago.
func keptSeatSince(t *testing.T, town, name, bead string, age time.Duration) {
	t.Helper()
	keptSeatRecord(t, town, name, bead, keptSeatReason, age)
}

// keptSeatRetiredSince writes a kept record for gastown/name, kept age ago, for
// the reason a record that does not say stop gives: what a slung polecat's
// record says.
func keptSeatRetiredSince(t *testing.T, town, name, bead string, age time.Duration) {
	t.Helper()
	keptSeatRecord(t, town, name, bead, retiredRecordKeepReason, age)
}

func keptSeatRecord(t *testing.T, town, name, bead, reason string, age time.Duration) {
	t.Helper()
	rec := keptSandboxSeatRecord{Bead: bead, Reason: reason, Since: keptSeatAttentionNow.Add(-age)}
	if err := writeKeptSandboxSeat(town, attentionRig, name, rec); err != nil {
		t.Fatalf("writing the kept record for %s: %v", name, err)
	}
}

// keptSeatAgentID is the agent bead the daemon reads for gastown/name.
func keptSeatAgentID(f *attentionFixture, name string) string {
	town := f.d.config.TownRoot
	return beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, attentionRig), attentionRig, name)
}

// keptSeatHooked gives gastown/name the agent bead the daemon reads, hooking
// hook (empty for no hook), and answers the hooked bead's own read with status.
// A seat whose keep reason is not the retired record never asks it.
func keptSeatHooked(t *testing.T, f *attentionFixture, name, hook, status string) {
	t.Helper()
	id := keptSeatAgentID(f, name)
	r := keptSeatHookReader{issues: map[string]*beads.Issue{
		id: {ID: id, Description: "hook_bead: " + hook},
	}}
	if hook != "" {
		r.issues[hook] = &beads.Issue{ID: hook, Status: status}
	}
	f.d.openWorkBeads = r.open
}

// keptSeatHookReader answers the daemon's hook reads from memory: the issue by
// id, a routing error for the ids in fail, no bead and no error for the ids in
// noBead, and beads.ErrNotFound otherwise.
type keptSeatHookReader struct {
	issues map[string]*beads.Issue
	fail   map[string]error
	noBead map[string]bool
}

func (r keptSeatHookReader) open([]string, time.Duration) workBeadReader { return r }

func (r keptSeatHookReader) Show(id string) (*beads.Issue, error) {
	if err := r.fail[id]; err != nil {
		return nil, err
	}
	if r.noBead[id] {
		return nil, nil
	}
	if is, ok := r.issues[id]; ok {
		return is, nil
	}
	return nil, beads.ErrNotFound
}

func (r keptSeatHookReader) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, nil }

// A seat kept more than thirty minutes raises one high item that names the seat,
// its bead and the age, and carries the daemon's reason verbatim. A seat kept
// for just under thirty minutes raises nothing.
func TestKeptSeatAttentionRaisesAfterTheWindow(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	keptSeatSince(t, f.d.config.TownRoot, "capable", "gt-abc12", 31*time.Minute)
	keptSeatSince(t, f.d.config.TownRoot, "nux", "gt-abc13", 29*time.Minute)
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one (the 29m seat raises none)", items)
	}
	it := items[0]
	if it.Key != "sandbox-seat-kept:gastown/capable" || it.Kind != attention.KindSandboxSeatKept ||
		it.Severity != attention.SeverityHigh || it.Rig != attentionRig || it.Bead != "gt-abc12" {
		t.Errorf("item = %+v, want a high sandbox-seat-kept item on gastown/capable and gt-abc12", it)
	}
	want := "gastown/capable kept 31m on gt-abc12: " + keptSeatReason
	if it.Summary != want {
		t.Errorf("summary = %q, want %q", it.Summary, want)
	}
}

// The item's key is the seat, so two ticks over the same record give one item
// with the same key, and removing the record clears it.
func TestKeptSeatAttentionKeyIsTheSeat(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	town := f.d.config.TownRoot
	keptSeatSince(t, town, "capable", "gt-abc12", 3*time.Hour)
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	first, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Key != second[0].Key {
		t.Fatalf("ticks = %+v then %+v, want one item with the same key", first, second)
	}

	if err := removeKeptSandboxSeat(town, attentionRig, "capable"); err != nil {
		t.Fatalf("removing the kept record: %v", err)
	}
	after, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("items after the record is gone = %+v, want none", after)
	}
}

// A record the reader cannot decode fails the collection: it returns the error
// and no items, so the previous tick's items are kept rather than cleared.
func TestKeptSeatAttentionFailsOnAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	keptSeatSince(t, f.d.config.TownRoot, "nux", "gt-abc13", 3*time.Hour)
	path, err := keptSandboxSeatPath(f.d.config.TownRoot, attentionRig, "capable")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err == nil {
		t.Fatalf("collector read a malformed record as clean: items = %+v", items)
	}
	if items != nil {
		t.Errorf("items = %+v beside an unreadable record, want none", items)
	}
	if !strings.Contains(err.Error(), "capable.json") {
		t.Errorf("error %q does not name the unreadable record", err)
	}
}

// A store that cannot be listed fails the collection with the listing error and
// no items, so the previous tick's items stand rather than clearing as an empty
// queue.
func TestKeptSeatAttentionFailsOnAStoreThatCannotBeListed(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	dir := keptSandboxSeatsDir(f.d.config.TownRoot)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where the store's directory should be: a listing of it fails.
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collector err = %v, want the listing error", err)
	}
	if items != nil {
		t.Errorf("items = %+v beside an unlistable store, want none", items)
	}
}

// A kept seat with no bead recorded says so in its summary, and its item has no
// bead.
func TestKeptSeatAttentionNamesAnUnrecordedBead(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	keptSeatSince(t, f.d.config.TownRoot, "capable", "", 3*time.Hour)
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	if items[0].Bead != "" || !strings.Contains(items[0].Summary, "on no recorded bead: ") {
		t.Errorf("item = %+v, want no bead and the summary to say no recorded bead", items[0])
	}
}

// An alarm-only gateway tally raises its kept-seat item unless the seat's agent
// bead reads nuked: a nuked seat is already gone, so the tally alone raises
// nothing for it (gt-5bch6b). The working and failed-read rows are
// TestAlarmOnlyKeptSeatItem's; this pins the collector on the loop that swaps
// the item builder in.
func TestKeptSeatAttentionSkipsANukedAlarmOnlySeat(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	id := keptSeatAgentID(f, "capable")
	f.d.openWorkBeads = keptSeatHookReader{issues: map[string]*beads.Issue{
		id: {ID: id, Description: "agent_state: " + string(beads.AgentStateNuked)},
	}}.open
	f.d.gatewayTallies = map[string]gatewayTally{
		gatewayTallyKey(attentionRig, "capable"): {alarm: "cannot count gateway tries for gastown/capable"},
	}
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none for a nuked seat", items)
	}
}

// A seat kept with the retired-record reason whose polecat now hooks a
// different bead that is still live work — open, hooked or in progress — raises
// no item: the seat was slung new work, not left waiting for the overseer
// (gt-jw437c, gt-b87gmu).
func TestKeptSeatAttentionSkipsAReSlungSeat(t *testing.T) {
	t.Parallel()
	for _, status := range []string{string(beads.StatusOpen), beads.StatusHooked, string(beads.StatusInProgress)} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			keptSeatRetiredSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
			keptSeatHooked(t, f, "capable", "gt-new1", status)
			c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

			items, err := c.collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 0 {
				t.Fatalf("items = %+v, want none for a seat slung new work", items)
			}
		})
	}
}

// A seat still raises its item when any one of the three does not hold: the
// hook names the record's own bead, there is no hook, the hooked bead is no
// longer live work, the reason is another one, or the hook cannot be read —
// never a skip (gt-jw437c, gt-b87gmu). A gateway alarm raises whatever the hook
// says.
func TestKeptSeatAttentionRaisesForASeatStillKept(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, f *attentionFixture)
		want  string
		// wantLog is the read failure the row drives, as the collector logs it:
		// empty for a row that asks no hook and so logs nothing.
		wantLog string
	}{
		{
			name: "the record's own bead",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatRetiredSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
				keptSeatHooked(t, f, "capable", "gt-old1", string(beads.StatusOpen))
			},
			want: retiredRecordKeepReason,
		},
		{
			name: "no hook",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatRetiredSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
				keptSeatHooked(t, f, "capable", "", "")
			},
			want: retiredRecordKeepReason,
		},
		{
			name: "the hooked bead is no longer live work",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatRetiredSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
				keptSeatHooked(t, f, "capable", "gt-new1", string(beads.StatusClosed))
			},
			want: retiredRecordKeepReason,
		},
		{
			name: "another keep reason, however the hook reads",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
				keptSeatHooked(t, f, "capable", "gt-new1", string(beads.StatusOpen))
			},
			want: keptSeatReason,
		},
		{
			name: "a sandbox the gateway has not confirmed gone",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatRecord(t, f.d.config.TownRoot, "capable", "gt-old1", sandboxNotGoneReason, 3*time.Hour)
				keptSeatHooked(t, f, "capable", "gt-new1", string(beads.StatusOpen))
			},
			want: sandboxNotGoneReason,
		},
		{
			name: "the agent bead cannot be read",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatRetiredSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
				id := keptSeatAgentID(f, "capable")
				f.d.openWorkBeads = keptSeatHookReader{
					fail: map[string]error{id: errors.New("bd show: store unavailable")},
				}.open
			},
			want:    retiredRecordKeepReason,
			wantLog: "cannot read its hook",
		},
		{
			name: "the hooked bead cannot be read",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatRetiredSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
				id := keptSeatAgentID(f, "capable")
				f.d.openWorkBeads = keptSeatHookReader{
					issues: map[string]*beads.Issue{id: {ID: id, Description: "hook_bead: gt-new1"}},
					fail:   map[string]error{"gt-new1": errors.New("bd show: store unavailable")},
				}.open
			},
			want:    retiredRecordKeepReason,
			wantLog: "cannot read the hooked bead gt-new1",
		},
		{
			name: "the hooked bead reads as no bead",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatRetiredSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
				id := keptSeatAgentID(f, "capable")
				f.d.openWorkBeads = keptSeatHookReader{
					issues: map[string]*beads.Issue{id: {ID: id, Description: "hook_bead: gt-new1"}},
					noBead: map[string]bool{"gt-new1": true},
				}.open
			},
			want:    retiredRecordKeepReason,
			wantLog: "the hooked bead gt-new1 reads as no bead",
		},
		{
			name: "a gateway alarm",
			setup: func(t *testing.T, f *attentionFixture) {
				keptSeatRetiredSince(t, f.d.config.TownRoot, "capable", "gt-old1", 3*time.Hour)
				keptSeatHooked(t, f, "capable", "gt-new1", string(beads.StatusOpen))
				f.d.gatewayTallies = map[string]gatewayTally{
					gatewayTallyKey(attentionRig, "capable"): {alarm: "cannot count gateway tries for gastown/capable"},
				}
			},
			want: "cannot count gateway tries",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			tt.setup(t, f)
			c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

			items, err := c.collect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want one", items)
			}
			if !strings.Contains(items[0].Summary, ": "+tt.want) {
				t.Errorf("summary = %q, want it to carry the reason %q", items[0].Summary, tt.want)
			}
			if tt.wantLog == "" {
				if got := withoutAgentBeadShadow(f.logs.String()); got != "" {
					t.Errorf("logged %q, want no read failure logged", got)
				}
			} else if !strings.Contains(f.logs.String(), tt.wantLog) {
				t.Errorf("logged %q, want the read failure %q", f.logs.String(), tt.wantLog)
			}
		})
	}
}

// withoutAgentBeadShadow drops the shadow window's lines (gt-8adi4i). The
// collector reads the agent bead through AgentRecord, which reports where the
// seat's intent record and that bead disagree into the same daemon log; those
// lines are not the read failure these rows are about.
func withoutAgentBeadShadow(log string) string {
	var kept []string
	for _, line := range strings.Split(log, "\n") {
		if line != "" && !strings.HasPrefix(line, agentBeadShadowLogPrefix) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// beadIsLiveWork keeps livework.Check's two error texts: a read that fails is
// "cannot read the hooked bead <id>: <cause>" and a read that returns no bead
// is "the hooked bead <id> reads as no bead". Neither may read as (false, nil),
// which the seat takes for a bead no longer under way and skips as idle
// (gt-pihggb, gt-jw437c).
func TestBeadIsLiveWorkReadFailureAndNoBeadAreErrors(t *testing.T) {
	t.Parallel()
	cause := errors.New("bd show: store unavailable")
	tests := []struct {
		name    string
		reader  keptSeatHookReader
		wantErr string
	}{
		{
			name:    "a failed read",
			reader:  keptSeatHookReader{fail: map[string]error{"gt-new1": cause}},
			wantErr: "cannot read the hooked bead gt-new1: bd show: store unavailable",
		},
		{
			name:    "a read that returns no bead",
			reader:  keptSeatHookReader{noBead: map[string]bool{"gt-new1": true}},
			wantErr: "the hooked bead gt-new1 reads as no bead",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			f.d.openWorkBeads = tt.reader.open
			h := &patrolScanHost{d: f.d}

			live, err := beadIsLiveWork(h, attentionRig, "gt-new1")
			if live {
				t.Errorf("beadIsLiveWork = true beside %v, want false", err)
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("beadIsLiveWork err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// keptSeatNuked answers whether the seat's agent bead reads agent_state nuked:
// true for nuked, false for another state, and false with the cause on a read
// that fails (gt-btnzan).
func TestKeptSeatNuked(t *testing.T) {
	t.Parallel()
	cause := errors.New("bd show: store unavailable")
	tests := []struct {
		name    string
		state   string
		fail    bool
		want    bool
		wantErr string
	}{
		{name: "an agent bead reading nuked", state: string(beads.AgentStateNuked), want: true},
		{name: "an agent bead reading working", state: string(beads.AgentStateWorking)},
		{
			name:    "a failed read",
			fail:    true,
			wantErr: "cannot read its agent bead: bd show: store unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			id := keptSeatAgentID(f, "capable")
			r := keptSeatHookReader{issues: map[string]*beads.Issue{
				id: {ID: id, Description: "agent_state: " + tt.state},
			}}
			if tt.fail {
				r = keptSeatHookReader{fail: map[string]error{id: cause}}
			}
			f.d.openWorkBeads = r.open

			nuked, err := f.src.keptSeatNuked(attentionRig, "capable")
			if nuked != tt.want {
				t.Errorf("keptSeatNuked = %t beside %v, want %t", nuked, err, tt.want)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("keptSeatNuked err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("keptSeatNuked err = %v, want it to carry %q", err, tt.wantErr)
			}
			if !errors.Is(err, cause) {
				t.Errorf("keptSeatNuked err = %v, want it to wrap the cause", err)
			}
		})
	}
}

// alarmOnlyKeptSeatItem builds the alarm-only kept-seat item unless the seat's
// agent bead reads nuked: nuked raises nothing, and a failed read raises the
// item all the same and logs the seat and the error (gt-kgawkr).
func TestAlarmOnlyKeptSeatItem(t *testing.T) {
	t.Parallel()
	const alarm = "cannot count gateway tries for gastown/capable"
	cause := errors.New("bd show: store unavailable")
	tests := []struct {
		name, state  string
		fail, wantOK bool
	}{
		{name: "an agent bead reading nuked", state: string(beads.AgentStateNuked)},
		{name: "an agent bead reading working", state: string(beads.AgentStateWorking), wantOK: true},
		{name: "a failed read", fail: true, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			id := keptSeatAgentID(f, "capable")
			r := keptSeatHookReader{issues: map[string]*beads.Issue{id: {ID: id, Description: "agent_state: " + tt.state}}}
			if tt.fail {
				r = keptSeatHookReader{fail: map[string]error{id: cause}}
			}
			f.d.openWorkBeads = r.open

			item, ok := f.src.alarmOnlyKeptSeatItem(attentionRig, "capable", alarm)
			if ok != tt.wantOK {
				t.Fatalf("alarmOnlyKeptSeatItem ok = %t, want %t", ok, tt.wantOK)
			}
			if !tt.wantOK {
				if item != (attention.Item{}) {
					t.Errorf("item = %+v beside ok=false, want the zero item", item)
				}
				return
			}
			want := "gastown/capable kept with no readable record on no recorded bead: " + alarm
			if item.Key != "sandbox-seat-kept:gastown/capable" || item.Kind != attention.KindSandboxSeatKept ||
				item.Rig != attentionRig || item.Bead != "" || item.Summary != want {
				t.Errorf("item = %+v, want the alarm-only item of gastown/capable, summary %q", item, want)
			}
			logs := f.logs.String()
			if tt.fail && (!strings.Contains(logs, "gastown/capable") || !strings.Contains(logs, cause.Error())) {
				t.Errorf("logged %q, want the seat gastown/capable and the error %q", logs, cause.Error())
			}
		})
	}
}

// The retired-record reason this collector skips on is the one the seat end
// gives a record that does not say stop: the seat end returns this constant,
// and the test pins that its keepReason still answers with it (gt-jw437c,
// gt-b87gmu).
func TestKeptSeatAttentionRetiredReasonMatchesTheSeatEnd(t *testing.T) {
	t.Parallel()
	const branch = "polecat/nux/gt-old1+x1"
	e := sandboxSeatEnd{
		SandboxSeat: true, SandboxGone: true, Landed: true, Retired: false,
		Branch: branch, LandedBranch: branch, CleanupStatus: polecat.CleanupClean,
	}
	if got := e.keepReason(); got != retiredRecordKeepReason {
		t.Errorf("retiredRecordKeepReason = %q, want the seat end's reason %q", retiredRecordKeepReason, got)
	}
}

// The collector is in the queue's set and owns its own items: a failed
// collection keeps the previous tick's kept-seat item and no other kind's.
func TestKeptSeatAttentionIsInTheCollectorSet(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)
	kept := attention.Item{Key: "sandbox-seat-kept:gastown/capable", Kind: attention.KindSandboxSeatKept, Severity: attention.SeverityHigh}
	other := attention.Item{Key: "config-parse-failed", Kind: attention.KindConfigParseFailed, Severity: attention.SeverityHigh}
	prev := attention.State{Items: []attention.Item{kept, other}}

	got := c.kept(prev)
	if len(got) != 1 || got[0].Key != kept.Key {
		t.Errorf("kept(prev) = %+v, want only the kept-seat item", got)
	}
}
