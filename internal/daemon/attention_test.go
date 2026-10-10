package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// attentionRig is the one rig every attention test's sources report.
const attentionRig = "gastown"

// attentionFixture is a daemon wired for an attention tick: a temp town, a
// logger a test can read, and source funcs that default to answering nothing.
type attentionFixture struct {
	d    *Daemon
	logs *bytes.Buffer
	now  time.Time
	src  *attentionSources
}

// newAttentionFixture builds the fixture at now. Every source answers an
// empty, healthy town until the test replaces it.
func newAttentionFixture(t *testing.T, now time.Time) *attentionFixture {
	t.Helper()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(now),
	}
	src := &attentionSources{
		d:           d,
		now:         now,
		landingRigs: func() []string { return []string{attentionRig} },
		redMain:     func(context.Context, string) ([]*beads.Issue, error) { return nil, nil },
		escalations: func(context.Context) ([]*beads.Issue, error) { return nil, nil },
		rework:      func(context.Context, string) ([]*beads.Issue, error) { return nil, nil },
		reworkNote:  func(context.Context, string, string) (string, error) { return "", nil },
		slots:       func() (slot.Report, error) { return slot.Report{}, nil },
		pidAlive:    func(int) bool { return true },
		bdLatency:   func(context.Context) (time.Duration, error) { return 0, nil },
		refusals:    func() ([]attention.Refusal, error) { return nil, nil },
		refusalBead: func(context.Context, string, string) (*beads.Issue, error) { return nil, nil },

		landingState:  func(string) landingState { return landingState{} },
		readyToLand:   func(context.Context, string) (readyQueue, error) { return readyQueue{}, nil },
		tierSweep:     func(string) (tierSweepState, error) { return tierSweepState{}, nil },
		tierSweepRigs: func() []string { return nil },
		seats:         func() ([]townhealth.Seat, error) { return nil, nil },
		seatWork:      func(string, string) (bool, error) { return false, nil },
		remoteTip:     func(string) (string, error) { return "", nil },
		landedCommit:  func(string, string) (bool, error) { return false, nil },
		commitInfo:    func(string, string) (string, string) { return "", "" },
		tips:          &directPushTips{},
		riskLandings:  func(string, time.Time) ([]land.LandingRecord, error) { return nil, nil },
		riskNotes:     func(context.Context, string, string) (string, error) { return "", nil },

		reworkNotes: map[string]reworkNote{},
	}
	return &attentionFixture{d: d, logs: logs, now: now, src: src}
}

// tick runs one attention tick at at, reconciling against the queue on disk
// exactly the way the daemon does, and returns the state it left there.
func (f *attentionFixture) tick(t *testing.T, at time.Time) attention.State {
	t.Helper()
	prev, err := attention.ReadState(f.d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	acks, err := attention.ReadAcks(f.d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	f.src.now = at
	f.d.attentionTick(context.Background(), f.src, prev, acks, at)
	st, err := attention.ReadState(f.d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// collect runs one collector from the fixture's sources.
func (f *attentionFixture) collect(t *testing.T, c func(context.Context) ([]attention.Item, error)) []attention.Item {
	t.Helper()
	items, err := c(context.Background())
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	return items
}

// itemByKey is the named item, or a test failure.
func itemByKey(t *testing.T, st attention.State, key string) attention.Item {
	t.Helper()
	it, ok := attention.Find(st, key)
	if !ok {
		t.Fatalf("no item %q in %+v", key, st.Items)
	}
	return it
}

func TestAttentionCollector_RedMain(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.redMain = func(context.Context, string) ([]*beads.Issue, error) {
		return []*beads.Issue{{
			ID: "gt-red", Status: "open",
			Title: landworker.RedMainTitle(attentionRig, "internal/cmd"),
		}}, nil
	}

	items := f.collect(t, f.src.collectRedMain)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one per open red-main bead", items)
	}
	got := items[0]
	if got.Key != "red-main:gastown:internal/cmd" || got.Kind != attention.KindRedMain ||
		got.Rig != attentionRig || got.Bead != "gt-red" {
		t.Errorf("item = %+v, want the rig and the package in the key", got)
	}

	// The bead closing is the clear: the read lists open beads, so a closed
	// one is not observed and the item drops.
	f.src.redMain = func(context.Context, string) ([]*beads.Issue, error) { return nil, nil }
	if items := f.collect(t, f.src.collectRedMain); len(items) != 0 {
		t.Errorf("items = %+v, want none once the bead closes", items)
	}
}

func TestAttentionCollector_Escalation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.escalations = func(context.Context) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "hq-9", Title: "dolt is wedged"}}, nil
	}

	items := f.collect(t, f.src.collectEscalations)
	if len(items) != 1 || items[0].Key != "esc:hq-9" || items[0].Bead != "hq-9" ||
		items[0].Kind != attention.KindEscalation || items[0].Summary != "dolt is wedged" {
		t.Fatalf("items = %+v, want one escalation item naming hq-9", items)
	}

	f.src.escalations = func(context.Context) ([]*beads.Issue, error) { return nil, nil }
	if items := f.collect(t, f.src.collectEscalations); len(items) != 0 {
		t.Errorf("items = %+v, want none with no open escalation", items)
	}
}

// reworkNotes builds a bead's notes with n MERGE REJECTION blocks.
func reworkNotes(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(land.FormatRejectionNote(land.RejectionNote{
			Attempt: i, Kind: "gate", Reason: "make test exit 2",
			Branch: "polecat/x/gt-r", Target: "main", MR: "gt-r", Head: "abc123",
		}))
		b.WriteString("\n")
	}
	return b.String()
}

func TestAttentionCollector_RejectedTwice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.rework = func(context.Context, string) ([]*beads.Issue, error) {
		return []*beads.Issue{
			{ID: "gt-twice", Status: "open", UpdatedAt: "t2", Notes: reworkNotes(2)},
			{ID: "gt-once", Status: "open", UpdatedAt: "t1", Notes: reworkNotes(1)},
			{ID: "gt-parked", Status: "deferred", UpdatedAt: "t3", Notes: reworkNotes(2)},
		}, nil
	}

	items := f.collect(t, f.src.collectRejectedTwice)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want only the twice-rejected actionable bead", items)
	}
	if items[0].Key != "rework:gt-twice" || items[0].Kind != attention.KindRejectedTwice {
		t.Errorf("item = %+v, want rework:gt-twice", items[0])
	}

	// Once the bead lands it is closed, so the read stops listing it.
	f.src.rework = func(context.Context, string) ([]*beads.Issue, error) { return nil, nil }
	if items := f.collect(t, f.src.collectRejectedTwice); len(items) != 0 {
		t.Errorf("items = %+v, want none once the bead lands", items)
	}
}

// The list carries no notes, so a candidate's notes come from one bd show,
// cached by updated_at: a bead that did not change is not shown again.
func TestAttentionCollector_RejectedTwiceCachesNotesByUpdatedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	shows := 0
	f.src.rework = func(context.Context, string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "gt-twice", Status: "open", UpdatedAt: "t2"}}, nil
	}
	f.src.reworkNote = func(context.Context, string, string) (string, error) {
		shows++
		return reworkNotes(2), nil
	}

	if items := f.collect(t, f.src.collectRejectedTwice); len(items) != 1 {
		t.Fatalf("items = %+v, want the twice-rejected bead", items)
	}
	if shows != 1 {
		t.Fatalf("shows = %d, want one bd show", shows)
	}
	if items := f.collect(t, f.src.collectRejectedTwice); len(items) != 1 {
		t.Fatalf("items = %+v, want the item on the second tick too", items)
	}
	if shows != 1 {
		t.Errorf("shows = %d, want the unchanged bead to be cached, not re-read", shows)
	}

	f.src.rework = func(context.Context, string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "gt-twice", Status: "open", UpdatedAt: "t3"}}, nil
	}
	f.collect(t, f.src.collectRejectedTwice)
	if shows != 2 {
		t.Errorf("shows = %d, want a changed bead to be re-read", shows)
	}
}

func TestAttentionCollector_SlotHeld(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.slots = func() (slot.Report, error) {
		return slot.Report{Slots: []slot.SlotState{
			{Index: 0, Held: true, Owner: &slot.Owner{Role: "refinery", PID: 4242, AcquiredAt: now.Add(-40 * time.Minute)}},
			{Index: 1, Held: true, Owner: &slot.Owner{Role: "polecat", PID: 4243, AcquiredAt: now.Add(-5 * time.Minute)}},
		}}, nil
	}

	items := f.collect(t, f.src.collectSlotHeld)
	if len(items) != 1 || items[0].Key != "slot:slot0" || items[0].Kind != attention.KindSlotHeld {
		t.Fatalf("items = %+v, want only the 40-minute hold", items)
	}
}

func TestAttentionCollector_SlotDeadHolder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.slots = func() (slot.Report, error) {
		return slot.Report{Slots: []slot.SlotState{
			{Index: 0, Held: true, Owner: &slot.Owner{Role: "refinery", PID: 4242, AcquiredAt: now.Add(-2 * time.Minute)}},
			{Index: 1, Held: true, Owner: &slot.Owner{Role: "polecat", PID: 4243, AcquiredAt: now.Add(-2 * time.Minute)}},
		}}, nil
	}
	f.src.pidAlive = func(pid int) bool { return pid != 4242 }

	items := f.collect(t, f.src.collectSlotDeadHolder)
	if len(items) != 1 || items[0].Key != "slot-dead:slot0" || items[0].Kind != attention.KindSlotDeadHolder {
		t.Fatalf("items = %+v, want only the dead holder", items)
	}
}

func TestAttentionCollector_BDSlow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	t.Run("a slow read is an item", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		f.src.bdLatency = func(context.Context) (time.Duration, error) { return 6 * time.Second, nil }
		items := f.collect(t, f.src.collectBDSlow)
		if len(items) != 1 || items[0].Key != "bd-slow" || items[0].Kind != attention.KindBDSlow {
			t.Fatalf("items = %+v, want one bd-slow item", items)
		}
	})
	t.Run("a red dolt verdict is an item even when the read was fast", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		f.src.report = &townhealth.Report{Fields: []townhealth.Field{
			{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Red, Value: "unreachable", Detail: "connection refused"},
		}}
		items := f.collect(t, f.src.collectBDSlow)
		if len(items) != 1 || !strings.Contains(items[0].Summary, "unreachable") {
			t.Fatalf("items = %+v, want one bd-slow item naming the red Dolt", items)
		}
	})
	t.Run("a fast read and a green dolt is not an item", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		f.src.report = &townhealth.Report{Fields: []townhealth.Field{
			{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Green, Value: "p50 4ms"},
		}}
		if items := f.collect(t, f.src.collectBDSlow); len(items) != 0 {
			t.Fatalf("items = %+v, want none", items)
		}
	})
	t.Run("a failed read still raises the item when dolt is red", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		f.src.report = &townhealth.Report{Fields: []townhealth.Field{
			{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Red, Value: "unreachable", Detail: "connection refused"},
		}}
		// bd is down: the probe does not answer at all. The report the same
		// tick already made is what raises the item.
		f.src.bdLatency = func(context.Context) (time.Duration, error) {
			return 0, os.ErrDeadlineExceeded
		}
		items := f.collect(t, f.src.collectBDSlow)
		if len(items) != 1 || items[0].Key != "bd-slow" || items[0].Kind != attention.KindBDSlow ||
			items[0].Severity != attention.SeverityHigh {
			t.Fatalf("items = %+v, want one high-severity bd-slow item", items)
		}
		if !strings.Contains(items[0].Summary, "unreachable") {
			t.Errorf("summary = %q, want it to name the red Dolt", items[0].Summary)
		}
	})
	t.Run("a failed read with no red verdict is still the item", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		f.src.report = &townhealth.Report{Fields: []townhealth.Field{
			{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Green, Value: "p50 4ms"},
		}}
		f.src.bdLatency = func(context.Context) (time.Duration, error) {
			return 0, os.ErrDeadlineExceeded
		}
		items := f.collect(t, f.src.collectBDSlow)
		if len(items) != 1 || items[0].Key != "bd-slow" {
			t.Fatalf("items = %+v, want the read that did not answer to be the item", items)
		}
	})
}

// gt-cuzjj: the daemon writes the surviving acks back, so an ack dies with its
// item. A key that cleared comes back unacked on a recurrence instead of
// hiding behind the ack the previous item left behind.
func TestAttentionTick_PrunesAcksWhoseItemCleared(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	town := f.d.config.TownRoot
	const redKey = "red-main:gastown:internal/cmd"
	raised := func(context.Context, string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "gt-red", Status: "open", Title: landworker.RedMainTitle(attentionRig, "internal/cmd")}}, nil
	}
	cleared := func(context.Context, string) ([]*beads.Issue, error) { return nil, nil }

	f.src.redMain = raised
	itemByKey(t, f.tick(t, now), redKey)

	if err := attention.Acknowledge(town, redKey, now); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if it := itemByKey(t, f.tick(t, now.Add(time.Minute)), redKey); it.AckedAt == nil {
		t.Fatalf("item = %+v, want the ack recorded", it)
	}

	// The condition clears: the item goes, and its ack with it.
	f.src.redMain = cleared
	if _, ok := attention.Find(f.tick(t, now.Add(2*time.Minute)), redKey); ok {
		t.Fatalf("item %q survived its condition clearing", redKey)
	}
	acks, err := attention.ReadAcks(town)
	if err != nil {
		t.Fatalf("ReadAcks: %v", err)
	}
	if attention.AckedKey(acks, redKey) {
		t.Errorf("acks = %+v, want the cleared item's ack dropped", acks.Acks)
	}

	// A recurrence is a new item: the dead ack must not hide it.
	f.src.redMain = raised
	if it := itemByKey(t, f.tick(t, now.Add(3*time.Minute)), redKey); it.AckedAt != nil {
		t.Errorf("item = %+v, want the recurrence unacked", it)
	}
}

// refusal is one recorded gt done refusal at head, for a refusal collector
// test.
func refusal(bead, rig, head string) attention.Refusal {
	return attention.Refusal{
		Bead: bead, Rig: rig, Worker: "emerald", Branch: "polecat/emerald/" + bead,
		Head: head, Kind: attention.KindRevertGuard,
		Summary: "branch reverts 2 merged commit(s): aaaa1234 do the thing",
	}
}

// TestAttentionCollector_RevertRefused (gt-vsct7.6): a record from the last
// 48 h whose bead is open and not since resubmitted at another head is an
// item; the bead closing or a different READY TO LAND head clears it.
func TestAttentionCollector_RevertRefused(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const head = "abcdef1234567890"
	readyAt := func(h string) string {
		return land.FormatReadyNote(land.Work{Branch: "b", Head: h, Target: "main", Worker: "emerald"})
	}

	t.Run("an open refused bead is one item", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		ref := refusal("gt-x", attentionRig, head)
		ref.TS = now.Add(-time.Hour)
		f.src.refusals = func() ([]attention.Refusal, error) { return []attention.Refusal{ref}, nil }
		f.src.refusalBead = func(context.Context, string, string) (*beads.Issue, error) {
			return &beads.Issue{ID: "gt-x", Status: "open"}, nil
		}
		items := f.collect(t, f.src.collectRevertRefused)
		if len(items) != 1 {
			t.Fatalf("items = %+v, want the refused bead", items)
		}
		got := items[0]
		if got.Key != "refused:gt-x:abcdef123456" || got.Kind != attention.KindRevertRefused ||
			got.Bead != "gt-x" || got.Rig != attentionRig || got.SHA != head {
			t.Errorf("item = %+v, want refused:gt-x:abcdef123456", got)
		}
	})

	t.Run("a closed bead clears it", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		ref := refusal("gt-x", attentionRig, head)
		ref.TS = now.Add(-time.Hour)
		f.src.refusals = func() ([]attention.Refusal, error) { return []attention.Refusal{ref}, nil }
		f.src.refusalBead = func(context.Context, string, string) (*beads.Issue, error) {
			return &beads.Issue{ID: "gt-x", Status: "closed"}, nil
		}
		if items := f.collect(t, f.src.collectRevertRefused); len(items) != 0 {
			t.Errorf("items = %+v, want none for a closed bead", items)
		}
	})

	t.Run("a later submission at another head clears it", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		ref := refusal("gt-x", attentionRig, head)
		ref.TS = now.Add(-time.Hour)
		f.src.refusals = func() ([]attention.Refusal, error) { return []attention.Refusal{ref}, nil }
		f.src.refusalBead = func(context.Context, string, string) (*beads.Issue, error) {
			return &beads.Issue{ID: "gt-x", Status: "open", Notes: readyAt("9999999999999999")}, nil
		}
		if items := f.collect(t, f.src.collectRevertRefused); len(items) != 0 {
			t.Errorf("items = %+v, want none once resubmitted at another head", items)
		}
	})

	t.Run("a READY note at the refused head keeps it", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		ref := refusal("gt-x", attentionRig, head)
		ref.TS = now.Add(-time.Hour)
		f.src.refusals = func() ([]attention.Refusal, error) { return []attention.Refusal{ref}, nil }
		f.src.refusalBead = func(context.Context, string, string) (*beads.Issue, error) {
			return &beads.Issue{ID: "gt-x", Status: "open", Notes: readyAt(head)}, nil
		}
		if items := f.collect(t, f.src.collectRevertRefused); len(items) != 1 {
			t.Errorf("items = %+v, want the item while the head still matches", items)
		}
	})

	t.Run("a refusal older than the window is not an item", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		ref := refusal("gt-x", attentionRig, head)
		ref.TS = now.Add(-49 * time.Hour)
		f.src.refusals = func() ([]attention.Refusal, error) { return []attention.Refusal{ref}, nil }
		f.src.refusalBead = func(context.Context, string, string) (*beads.Issue, error) {
			return &beads.Issue{ID: "gt-x", Status: "open"}, nil
		}
		if items := f.collect(t, f.src.collectRevertRefused); len(items) != 0 {
			t.Errorf("items = %+v, want none past the 48 h window", items)
		}
	})

	t.Run("a failed ledger read is an error", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, now)
		f.src.refusals = func() ([]attention.Refusal, error) { return nil, os.ErrDeadlineExceeded }
		if _, err := f.src.collectRevertRefused(context.Background()); err == nil {
			t.Error("a failed ledger read returned items, want an error")
		}
	})
}

// A collector whose source fails keeps the previous tick's items of its kind:
// an unanswered query must not clear a real alarm.
func TestAttentionTick_FailedCollectorKeepsItsItems(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.redMain = func(context.Context, string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "gt-red", Status: "open", Title: landworker.RedMainTitle(attentionRig, "internal/cmd")}}, nil
	}
	f.src.escalations = func(context.Context) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "hq-9", Title: "dolt is wedged"}}, nil
	}
	first := f.tick(t, now)
	if _, ok := attention.Find(first, "red-main:gastown:internal/cmd"); !ok {
		t.Fatalf("first tick = %+v, want the red-main item", first.Items)
	}
	if _, ok := attention.Find(first, "esc:hq-9"); !ok {
		t.Fatalf("first tick = %+v, want the escalation item", first.Items)
	}

	// The red-main read fails; the escalation read answers, now empty.
	f.logs.Reset()
	f.src.redMain = func(context.Context, string) ([]*beads.Issue, error) {
		return nil, os.ErrDeadlineExceeded
	}
	f.src.escalations = func(context.Context) ([]*beads.Issue, error) { return nil, nil }
	second := f.tick(t, now.Add(time.Minute))

	kept := itemByKey(t, second, "red-main:gastown:internal/cmd")
	if !kept.LastSeen.Equal(now.Add(time.Minute)) {
		t.Errorf("red-main LastSeen = %v, want the second tick's time", kept.LastSeen)
	}
	if !kept.FirstSeen.Equal(now) {
		t.Errorf("red-main FirstSeen = %v, want the first tick's time kept", kept.FirstSeen)
	}
	if _, ok := attention.Find(second, "esc:hq-9"); ok {
		t.Errorf("escalation item survived a successful empty read: %+v", second.Items)
	}
	if got := f.logs.String(); !strings.Contains(got, "attention: collector red-main") {
		t.Errorf("log = %q, want one line naming the failed collector", got)
	}
}

func TestAttentionTick_LogsOneLinePerTransitionAndNothingOnNoChange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	raised := func(context.Context, string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "gt-red", Status: "open", Title: landworker.RedMainTitle(attentionRig, "internal/cmd")}}, nil
	}
	gone := func(context.Context, string) ([]*beads.Issue, error) { return nil, nil }
	const line = "red-main:gastown:internal/cmd red main (gastown): internal/cmd"

	// A new item names the transition, so the line reads on its own.
	f.src.redMain = raised
	f.tick(t, now)
	want := "attention: raised " + line + "\n"
	if got := f.logs.String(); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}

	// A beat with no change says nothing.
	f.logs.Reset()
	f.tick(t, now.Add(time.Minute))
	if got := f.logs.String(); got != "" {
		t.Errorf("log = %q, want nothing on a tick with no change", got)
	}

	// A cleared item carries the raised item's text verbatim, so the word is
	// the only thing distinguishing the two lines (gt-t4n5r).
	f.logs.Reset()
	f.src.redMain = gone
	f.tick(t, now.Add(2*time.Minute))
	want = "attention: cleared " + line + "\n"
	if got := f.logs.String(); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}

	// The tick after a clear raises the item again, under the same word.
	f.logs.Reset()
	f.src.redMain = raised
	f.tick(t, now.Add(3*time.Minute))
	want = "attention: raised " + line + "\n"
	if got := f.logs.String(); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

// writeAttention is the heartbeat step: it reads the daemon's own sources and
// writes state.json every tick, so gt attention shows the queue without the
// daemon being asked.
func TestWriteAttention_WritesTheQueueFromTheDaemonsOwnReads(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	writeJSONFile(t, filepath.Join(town, "mayor", "rigs.json"), map[string]any{
		"version": 1, "rigs": map[string]any{attentionRig: map[string]any{}},
	})
	bd := &labelBeads{
		issuesByLabel: map[string][]*beads.Issue{
			landworker.LabelRedMain: {{
				ID: "gt-red", Status: "open",
				Title: landworker.RedMainTitle(attentionRig, "internal/cmd"),
			}},
		},
		wispsByLabel: map[string][]*beads.Issue{
			// A real record beside the mail carrier routed for it: the shared
			// query drops the carrier (gt-9k2bx).
			"gt:escalation": {
				{ID: "hq-9", Status: "open", Title: "dolt is wedged", Labels: []string{"gt:escalation"}, CreatedAt: now.Add(-90 * time.Minute).Format(time.RFC3339)},
				{ID: "hq-msg", Status: "open", Title: "escalation carrier", Labels: []string{"gt:escalation", "gt:message"}},
			},
		},
	}
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(now),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			LandingWorker: &LandingWorkerConfig{Enabled: true},
		}},
		openWorkBeads: bd.open,
	}

	d.writeAttention()

	st, err := attention.ReadState(town)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Updated.Equal(now) {
		t.Errorf("state.Updated = %v, want the tick time", st.Updated)
	}
	if _, ok := attention.Find(st, "red-main:gastown:internal/cmd"); !ok {
		t.Errorf("state = %+v, want the red-main item", st.Items)
	}
	if _, ok := attention.Find(st, "esc:hq-9"); !ok {
		t.Errorf("state = %+v, want the escalation item", st.Items)
	}
	if _, ok := attention.Find(st, "esc:hq-msg"); ok {
		t.Errorf("state = %+v, want the mail carrier dropped", st.Items)
	}

	// The next tick carries the state in lastAttention, so a recurrence of
	// the same condition keeps its first_seen rather than raising a new item.
	first := itemByKey(t, st, "esc:hq-9")
	d.writeAttention()
	st2, err := attention.ReadState(town)
	if err != nil {
		t.Fatal(err)
	}
	again := itemByKey(t, st2, "esc:hq-9")
	if !again.FirstSeen.Equal(first.FirstSeen) {
		t.Errorf("FirstSeen = %v, want the first tick's %v", again.FirstSeen, first.FirstSeen)
	}
	if strings.Count(logs.String(), "attention: raised esc:hq-9") != 1 {
		t.Errorf("log = %q, want one new-item line, not one per beat", logs.String())
	}
}

// gt-vsct7.3: a rig whose landing pass has been on the same bead longer than
// the threshold raises landing-stuck, and the item clears when the pass moves
// on. The clock is the fixture's fake one. A pass that has reported no stage
// is judged against the whole landing budget, the fast work before the gate.
func TestAttentionLandingStuck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)

	// Exactly at the threshold is not yet past it.
	f.src.landingState = func(string) landingState {
		return landingState{bead: "gt-a", since: now.Add(-townhealth.LandingWaitBudget)}
	}
	if items := f.collect(t, f.src.collectLandingStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none at exactly the threshold", items)
	}

	f.src.landingState = func(string) landingState {
		return landingState{bead: "gt-a", since: now.Add(-townhealth.LandingWaitBudget - time.Minute)}
	}
	items := f.collect(t, f.src.collectLandingStuck)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one stuck landing", items)
	}
	got := items[0]
	if got.Key != "landing-stuck:gastown:gt-a" || got.Kind != attention.KindLandingStuck ||
		got.Rig != attentionRig || got.Bead != "gt-a" || got.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want the rig and bead in the key, high severity", got)
	}

	// The pass moves on to the next bead: the condition no longer holds.
	f.src.landingState = func(string) landingState {
		return landingState{bead: "gt-b", since: now}
	}
	if items := f.collect(t, f.src.collectLandingStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none once the pass moved on", items)
	}
}

// gt-84gcp, the reported incident: the alarm judges the stage the pass is
// running, not the whole landing, so a healthy landing that has been waiting
// on CI for eight minutes — inside the CI wait's own budget — is not an item,
// and the CI minutes do not count against the review that follows it.
func TestAttentionLandingStuckJudgesTheStage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	ciBudget := f.d.attentionLandingStuckBudget(land.StageCI)
	omBudget := f.d.attentionLandingStuckBudget(land.StageOM)

	// The incident: eight minutes into the CI wait, the old flat threshold, is
	// healthy — the candidate gate may legitimately wait on the runner.
	f.src.landingState = func(string) landingState {
		return landingState{bead: "gt-a", since: now.Add(-8 * time.Minute), stage: land.StageCI, stageSince: now.Add(-8 * time.Minute)}
	}
	if items := f.collect(t, f.src.collectLandingStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none for a healthy CI wait", items)
	}

	// A long pipeline is still healthy while the stage it is running is not
	// wedged: twenty minutes of in-flight time, three of them in the CI wait.
	f.src.landingState = func(string) landingState {
		return landingState{bead: "gt-a", since: now.Add(-20 * time.Minute), stage: land.StageCI, stageSince: now.Add(-3 * time.Minute)}
	}
	if items := f.collect(t, f.src.collectLandingStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none for a stage inside its budget", items)
	}

	// The review is judged by its own, shorter budget, and the item names the
	// stage it outlived.
	f.src.landingState = func(string) landingState {
		return landingState{bead: "gt-a", since: now.Add(-20 * time.Minute), stage: land.StageOM, stageSince: now.Add(-omBudget - time.Minute)}
	}
	items := f.collect(t, f.src.collectLandingStuck)
	if len(items) != 1 || items[0].Bead != "gt-a" {
		t.Fatalf("items = %+v, want one wedged review", items)
	}
	if !strings.Contains(items[0].Summary, land.StageOM) {
		t.Errorf("summary = %q, want it to name the %s stage", items[0].Summary, land.StageOM)
	}

	// A CI wait past its own budget is the item.
	f.src.landingState = func(string) landingState {
		return landingState{bead: "gt-a", since: now.Add(-40 * time.Minute), stage: land.StageCI, stageSince: now.Add(-ciBudget - time.Minute)}
	}
	if items := f.collect(t, f.src.collectLandingStuck); len(items) != 1 {
		t.Fatalf("items = %+v, want one wedged CI wait", items)
	}
}

// gt-84gcp: the stage belongs to the bead in flight. Entering a new bead
// clears it, so the next landing's CI wait is not judged by the last one's.
func TestLandingStatesStageFollowsTheBead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	var l landingStates
	l.setBead("gastown", "gt-a", now)
	l.setStage("gastown", land.StageCI, now.Add(time.Minute))

	p := l.get("gastown")
	if p.stage != land.StageCI || !p.stageSince.Equal(now.Add(time.Minute)) {
		t.Fatalf("state = %+v, want the CI stage from a minute in", p)
	}

	l.setBead("gastown", "gt-b", now.Add(2*time.Minute))
	if p := l.get("gastown"); p.stage != "" || !p.stageSince.IsZero() {
		t.Fatalf("state = %+v, want no stage on the next bead", p)
	}

	l.setStage("gastown", land.StageOM, now.Add(3*time.Minute))
	l.endPass("gastown")
	if p := l.get("gastown"); p.stage != "" || !p.stageSince.IsZero() {
		t.Fatalf("state = %+v, want the stage cleared with the pass", p)
	}
}

// The item clears in the queue, not just in one collector: the next tick stops
// returning it and Reconcile drops it.
func TestAttentionLandingStuckClearsWhenThePassMoves(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.landingState = func(string) landingState {
		return landingState{bead: "gt-a", since: now.Add(-townhealth.LandingWaitBudget - time.Minute)}
	}
	st := f.tick(t, now)
	itemByKey(t, st, "landing-stuck:gastown:gt-a")

	f.src.landingState = func(string) landingState { return landingState{} }
	st = f.tick(t, now.Add(time.Minute))
	if _, ok := attention.Find(st, "landing-stuck:gastown:gt-a"); ok {
		t.Fatalf("state = %+v, want the landed item cleared", st.Items)
	}
}

// gt-m36as: queue-stuck judges the oldest pending submission's own age, so a
// rig with ready-to-land work raises no item while its oldest submission is
// inside the budget, and one once it is past it; a rig with nothing waiting
// never does.
func TestAttentionQueueStuck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.readyToLand = func(context.Context, string) (readyQueue, error) {
		return readyQueue{count: 1, oldest: now.Add(-townhealth.LandingWaitBudget), bead: "gt-a"}, nil
	}
	if items := f.collect(t, f.src.collectQueueStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none at exactly the budget", items)
	}

	f.src.readyToLand = func(context.Context, string) (readyQueue, error) {
		return readyQueue{count: 1, oldest: now.Add(-townhealth.LandingWaitBudget - time.Minute), bead: "gt-a"}, nil
	}
	items := f.collect(t, f.src.collectQueueStuck)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one stuck queue", items)
	}
	if got := items[0]; got.Key != "queue-stuck:gastown" || got.Kind != attention.KindQueueStuck ||
		got.Rig != attentionRig || got.Severity != attention.SeverityHigh || got.Bead != "gt-a" {
		t.Errorf("item = %+v, want queue-stuck:gastown naming gt-a", got)
	}

	// Nothing waiting: no item however long the rig has been quiet.
	f.src.readyToLand = func(context.Context, string) (readyQueue, error) {
		return readyQueue{}, nil
	}
	if items := f.collect(t, f.src.collectQueueStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none with an empty queue", items)
	}

	// A waiting bead whose submission time cannot be read has no age to
	// judge; the landing health field reports the unreadable stamp.
	f.src.readyToLand = func(context.Context, string) (readyQueue, error) {
		return readyQueue{count: 1}, nil
	}
	if items := f.collect(t, f.src.collectQueueStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none without a submission time", items)
	}
}

// gt-cpefw: queue-stuck scales its allowance with the queue's depth, the same
// townhealth.LandingWaitLimits the health field uses, so a healthy serial
// queue of three is not an item while a single submission past one budget
// still is.
func TestAttentionQueueStuckScalesWithDepth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)

	// Three waiting, the oldest 20m in: under three budgets, healthy.
	f.src.readyToLand = func(context.Context, string) (readyQueue, error) {
		return readyQueue{count: 3, oldest: now.Add(-20 * time.Minute), bead: "gt-busy"}, nil
	}
	if items := f.collect(t, f.src.collectQueueStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none for a three-deep queue inside three budgets", items)
	}

	// The same queue once the oldest has waited past three budgets.
	f.src.readyToLand = func(context.Context, string) (readyQueue, error) {
		return readyQueue{count: 3, oldest: now.Add(-3*townhealth.LandingWaitBudget - time.Minute), bead: "gt-busy"}, nil
	}
	items := f.collect(t, f.src.collectQueueStuck)
	if len(items) != 1 || items[0].Key != "queue-stuck:gastown" || items[0].Bead != "gt-busy" {
		t.Fatalf("items = %+v, want queue-stuck:gastown naming gt-busy past three budgets", items)
	}

	// A single submission keeps the flat budget: past one, it is an item
	// however healthy the rig has otherwise been.
	f.src.readyToLand = func(context.Context, string) (readyQueue, error) {
		return readyQueue{count: 1, oldest: now.Add(-townhealth.LandingWaitBudget - time.Minute), bead: "gt-solo"}, nil
	}
	items = f.collect(t, f.src.collectQueueStuck)
	if len(items) != 1 || items[0].Bead != "gt-solo" {
		t.Fatalf("items = %+v, want the lone stale submission to still be an item", items)
	}
}

// gt-m36as, the reported incident: a landing submitted after hours of quiet is
// judged by the submission's own age, not by the time since the rig last
// landed, so the first landing of the day is not instantly an item.
func TestAttentionQueueStuckJudgesTheSubmissionAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	submitted := now.Add(-time.Minute)
	f := newAttentionFixture(t, now)
	f.src.readyToLand = func(context.Context, string) (readyQueue, error) {
		return readyQueue{count: 1, oldest: submitted, bead: "gt-a"}, nil
	}
	if items := f.collect(t, f.src.collectQueueStuck); len(items) != 0 {
		t.Fatalf("items = %+v, want none for a submission a minute old after a long quiet", items)
	}

	// The same bead, still waiting a budget later: now the queue is the
	// problem, and the item names the bead and its wait.
	f.src.now = submitted.Add(townhealth.LandingWaitBudget + time.Minute)
	items := f.collect(t, f.src.collectQueueStuck)
	if len(items) != 1 || items[0].Key != "queue-stuck:gastown" || items[0].Bead != "gt-a" {
		t.Fatalf("items = %+v, want queue-stuck:gastown naming gt-a once its wait passed the budget", items)
	}
}

// gt-vsct7.3: a running polecat whose intent record shows no progress for the
// threshold, and which holds assigned open work, raises stall:<rig>/<name>.
// The walk is the same one townhealth judges (healthSources.Seats), so the
// item reads the samples the liveness sampler already wrote.
func TestAttentionPolecatStall(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}

	writeSeat := func(name string, rec intent.Record) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(town, attentionRig, "polecats", name), 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSONFile(t, intent.Seat{Rig: attentionRig, Role: constants.RolePolecat, Name: name}.Path(town), rec)
	}
	quiet := now.Add(-attentionPolecatStall - time.Minute)
	writeSeat("opal", intent.Record{
		Desired:  intent.DesiredRun,
		Progress: &intent.Progress{SampledAt: now.Add(-time.Minute), ChangedAt: quiet},
	})

	src := &attentionSources{
		d:           d,
		now:         now,
		landingRigs: func() []string { return []string{attentionRig} },
		seats: func() ([]townhealth.Seat, error) {
			return (&healthSources{d: d, evidence: time.Hour, now: now}).Seats()
		},
		seatWork:     func(string, string) (bool, error) { return true, nil },
		landingState: func(string) landingState { return landingState{} },
	}
	items, err := src.collectPolecatStall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one stalled polecat", items)
	}
	got := items[0]
	if got.Key != "stall:gastown/opal" || got.Kind != attention.KindPolecatStall ||
		got.Rig != attentionRig || got.Severity != attention.SeverityLow {
		t.Errorf("item = %+v, want stall:gastown/opal at low severity", got)
	}

	// A seat with no assigned work is idle, not stalled.
	src.seatWork = func(string, string) (bool, error) { return false, nil }
	if items, err := src.collectPolecatStall(context.Background()); err != nil || len(items) != 0 {
		t.Fatalf("items = %+v (err %v), want none for a seat holding no work", items, err)
	}
	src.seatWork = func(string, string) (bool, error) { return true, nil }

	// An unanswered work read is UNKNOWN, not "no work": the collector fails
	// and the tick keeps the kind's previous items.
	src.seatWork = func(string, string) (bool, error) { return false, errors.New("bd is down") }
	if _, err := src.collectPolecatStall(context.Background()); err == nil {
		t.Fatal("want the collector to fail when the seat's work cannot be read")
	}
	src.seatWork = func(string, string) (bool, error) { return true, nil }

	// Progress moving again clears it.
	writeSeat("opal", intent.Record{
		Desired:  intent.DesiredRun,
		Progress: &intent.Progress{SampledAt: now.Add(-time.Minute), ChangedAt: now.Add(-time.Minute)},
	})
	if items, err := src.collectPolecatStall(context.Background()); err != nil || len(items) != 0 {
		t.Fatalf("items = %+v (err %v), want none once the seat moved", items, err)
	}
}

// A frozen seat and a seat that is not a polecat are never stall items.
func TestAttentionPolecatStallSkipsFrozenAndOtherRoles(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	quiet := now.Add(-attentionPolecatStall - time.Minute)
	f := newAttentionFixture(t, now)
	f.src.seats = func() ([]townhealth.Seat, error) {
		return []townhealth.Seat{
			{Rig: attentionRig, Name: "polecat/frozen", Run: true, Frozen: true, Sampled: now.Add(-time.Minute), Changed: quiet},
			{Rig: attentionRig, Name: "witness", Run: true, Sampled: now.Add(-time.Minute), Changed: quiet},
			{Rig: attentionRig, Name: "polecat/stopped", Sampled: now.Add(-time.Minute), Changed: quiet},
		}, nil
	}
	f.src.seatWork = func(string, string) (bool, error) {
		t.Fatal("no seat here may be asked for work")
		return false, nil
	}
	if items := f.collect(t, f.src.collectPolecatStall); len(items) != 0 {
		t.Fatalf("items = %+v, want none for frozen, non-polecat or stopped seats", items)
	}
}

// gt-vsct7.3: a tip that reached origin with no landing record naming it
// raises direct-push:<rig>:<sha12> with the commit's author and subject. A
// worker landing raises nothing, the tip is judged one tick after it moves so
// the record has time to appear, and the item clears a day after the tip was
// first seen.
func TestAttentionDirectPush(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.tips = &directPushTips{}

	tip := "aaaa000000000000000000000000000000000000"
	landed := map[string]bool{}
	f.src.remoteTip = func(string) (string, error) { return tip, nil }
	f.src.landedCommit = func(string, string) (bool, error) { return landed[tip], nil }
	f.src.commitInfo = func(string, string) (string, string) { return "Alice", "push straight to main" }

	// The first read is a baseline: the tip already on origin is not a move.
	if items := f.collect(t, f.src.collectDirectPush); len(items) != 0 {
		t.Fatalf("items = %+v, want none for the first reading", items)
	}

	// The landing worker lands: the tip moves, and its record is there by the
	// time the next tick judges it.
	tip = "bbbb000000000000000000000000000000000000"
	f.src.now = now.Add(time.Minute)
	if items := f.collect(t, f.src.collectDirectPush); len(items) != 0 {
		t.Fatalf("items = %+v, want nothing on the tick the tip moved", items)
	}
	landed[tip] = true
	f.src.now = now.Add(2 * time.Minute)
	if items := f.collect(t, f.src.collectDirectPush); len(items) != 0 {
		t.Fatalf("items = %+v, want nothing for a recorded landing", items)
	}

	// A tip nothing recorded: one item, with the commit's author and subject.
	tip = "cccc000000000000000000000000000000000000"
	f.src.now = now.Add(3 * time.Minute)
	if items := f.collect(t, f.src.collectDirectPush); len(items) != 0 {
		t.Fatalf("items = %+v, want nothing on the grace tick", items)
	}
	f.src.now = now.Add(4 * time.Minute)
	items := f.collect(t, f.src.collectDirectPush)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one direct push", items)
	}
	got := items[0]
	if got.Key != "direct-push:gastown:cccc00000000" || got.Kind != attention.KindDirectPush ||
		got.Rig != attentionRig || got.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want the twelve-character sha in the key", got)
	}
	if got.SHA != tip {
		t.Errorf("SHA = %q, want the full %q", got.SHA, tip)
	}
	if !strings.Contains(got.Summary, "Alice") || !strings.Contains(got.Summary, "push straight to main") {
		t.Errorf("summary = %q, want the commit's author and subject", got.Summary)
	}

	// It holds while the tip stays.
	f.src.now = now.Add(5 * time.Minute)
	if items := f.collect(t, f.src.collectDirectPush); len(items) != 1 {
		t.Fatalf("items = %+v, want the item to hold", items)
	}

	// A day after the tip was first seen it is dropped, so an unacked push
	// does not hold forever.
	f.src.now = now.Add(3*time.Minute + attentionDirectPushHold)
	if items := f.collect(t, f.src.collectDirectPush); len(items) != 0 {
		t.Fatalf("items = %+v, want the item expired after the hold", items)
	}
}

// gt-vsct7.4: a landing that touched a risk path raises risk:<bead>:<head12>
// until a review note names that head. Only an "OVERSEER REVIEW <head>
// PASS|FAIL" line clears it: the om-bypass marker "OVERSEER REVIEWED" and a
// review of another head both leave it holding.
func TestAttentionRiskPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	head := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	f.src.riskLandings = func(string, time.Time) ([]land.LandingRecord, error) {
		return []land.LandingRecord{
			{BeadID: "gt-abc", Head: head, RiskPaths: []string{"internal/daemon/attention.go"}, LandedAt: now.Add(-time.Hour)},
			// A landing that touched nothing on the list is not an item.
			{BeadID: "gt-doc", Head: other, LandedAt: now.Add(-time.Hour)},
		}, nil
	}
	notes := ""
	f.src.riskNotes = func(context.Context, string, string) (string, error) { return notes, nil }

	items := f.collect(t, f.src.collectRiskPaths)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one risk-path item", items)
	}
	got := items[0]
	if got.Key != "risk:gt-abc:"+head[:12] || got.Kind != attention.KindRiskPath ||
		got.Rig != attentionRig || got.Bead != "gt-abc" || got.SHA != head || got.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want risk:gt-abc:<head12> with the full head in SHA", got)
	}
	if !strings.Contains(got.Summary, "internal/daemon/attention.go") {
		t.Errorf("summary = %q, want the touched path", got.Summary)
	}

	// The om-bypass marker is not a review of this head.
	notes = land.OverseerReviewedMarker + " " + head
	if items := f.collect(t, f.src.collectRiskPaths); len(items) != 1 {
		t.Errorf("items = %+v, want the item held for %q", items, land.OverseerReviewedMarker)
	}
	// A review of another head does not clear this one.
	notes = land.OverseerReviewMarker + " " + other + " PASS"
	if items := f.collect(t, f.src.collectRiskPaths); len(items) != 1 {
		t.Errorf("items = %+v, want the item held for a review of another head", items)
	}
	// A FAIL review of this head clears it: the overseer files the follow-up.
	notes = land.OverseerReviewMarker + " " + head + " FAIL"
	if items := f.collect(t, f.src.collectRiskPaths); len(items) != 0 {
		t.Errorf("items = %+v, want a FAIL review of this head to clear it", items)
	}
	// So does a PASS.
	notes = land.OverseerReviewMarker + " " + head + " PASS"
	if items := f.collect(t, f.src.collectRiskPaths); len(items) != 0 {
		t.Errorf("items = %+v, want a PASS review of this head to clear it", items)
	}
	// WAIVED and AUDITED clear it too, but only with their reason (gt-r5ne9).
	for _, tc := range []struct {
		notes string
		held  bool
	}{
		{land.OverseerReviewMarker + " " + head + " WAIVED: docs-only", false},
		{land.OverseerReviewMarker + " " + head + " AUDITED: file-level audit", false},
		{land.OverseerReviewMarker + " " + head + " WAIVED", true},
		{land.OverseerReviewMarker + " " + other + " AUDITED: file-level audit", true},
	} {
		notes = tc.notes
		if items := f.collect(t, f.src.collectRiskPaths); (len(items) == 1) != tc.held || len(items) > 1 {
			t.Errorf("notes %q: items = %+v, want held = %v", tc.notes, items, tc.held)
		}
	}
}

// The collector is wired into the tick, and two records naming one bead cost
// one bd show.
func TestAttentionRiskPathTickAndOneReadPerBead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.riskLandings = func(string, time.Time) ([]land.LandingRecord, error) {
		return []land.LandingRecord{
			{BeadID: "gt-abc", Head: strings.Repeat("a", 40), RiskPaths: []string{"a"}, LandedAt: now},
			{BeadID: "gt-abc", Head: strings.Repeat("c", 40), RiskPaths: []string{"b"}, LandedAt: now},
		}, nil
	}
	reads := 0
	f.src.riskNotes = func(context.Context, string, string) (string, error) { reads++; return "", nil }

	st := f.tick(t, now)
	var risk []attention.Item
	for _, it := range st.Items {
		if it.Kind == attention.KindRiskPath {
			risk = append(risk, it)
		}
	}
	if len(risk) != 2 {
		t.Fatalf("risk items = %+v, want one per landing head", risk)
	}
	if reads != 1 {
		t.Errorf("bd shows = %d, want one per bead per tick", reads)
	}
}

// A failed landings read emits no items and keeps the previous tick's.
func TestAttentionRiskPathKeepsItemsWhenTheReadFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.riskLandings = func(string, time.Time) ([]land.LandingRecord, error) {
		return nil, errors.New("landings unreadable")
	}
	prev := attention.State{Updated: now, Items: []attention.Item{{
		Key: "risk:gt-abc:aaaaaaaaaaaa", Kind: attention.KindRiskPath,
		Severity: attention.SeverityHigh, Rig: attentionRig, Summary: "risky landing",
	}}}
	collectors := []attentionCollector{{kind: attention.KindRiskPath, collect: f.src.collectRiskPaths}}
	observed := collectAttention(context.Background(), prev, collectors, func(string, ...any) {})
	if len(observed) != 1 || observed[0].Key != "risk:gt-abc:aaaaaaaaaaaa" {
		t.Fatalf("observed = %+v, want the previous item kept", observed)
	}
}

// A failed ls-remote emits no items and keeps the previous tick's.
func TestAttentionDirectPushKeepsItemsWhenTheReadFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.tips = &directPushTips{}
	f.src.remoteTip = func(string) (string, error) { return "", errors.New("origin unreachable") }

	prev := attention.State{Updated: now, Items: []attention.Item{{
		Key: "direct-push:gastown:cccc00000000", Kind: attention.KindDirectPush,
		Severity: attention.SeverityHigh, Rig: attentionRig, Summary: "main -> cccc00000000 with no landing record",
	}}}
	collectors := []attentionCollector{{kind: attention.KindDirectPush, collect: f.src.collectDirectPush}}
	observed := collectAttention(context.Background(), prev, collectors, func(string, ...any) {})
	if len(observed) != 1 || observed[0].Key != "direct-push:gastown:cccc00000000" {
		t.Fatalf("observed = %+v, want the previous item kept", observed)
	}
}

// The observed tip and the raised push survive a daemon restart: a fresh
// collector reading tips.json off disk raises the same item without judging
// the tip again.
func TestDirectPushTipsSurviveARestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	const sha = "cccc000000000000000000000000000000000000"
	if err := writeDirectPushTips(town, directPushTips{Rigs: []directPushTip{{
		Rig: attentionRig, Tip: sha, Pushes: []directPush{{SHA: sha, FirstSeen: now, Author: "Alice", Subject: "straight to main"}},
	}}}, now); err != nil {
		t.Fatal(err)
	}

	tips, err := readDirectPushTips(town)
	if err != nil {
		t.Fatal(err)
	}
	src := &attentionSources{
		d:            &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)},
		now:          now.Add(time.Minute),
		landingRigs:  func() []string { return []string{attentionRig} },
		tips:         &tips,
		remoteTip:    func(string) (string, error) { return sha, nil },
		landedCommit: func(string, string) (bool, error) { return false, nil },
		commitInfo:   func(string, string) (string, string) { return "", "" },
	}
	items, err := src.collectDirectPush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "direct-push:gastown:cccc00000000" {
		t.Fatalf("items = %+v, want the persisted push raised again", items)
	}
}

// TestCollectTierSweepRed_RaisesTheRedTiersAndClearsOnGreen walks one rig's
// sweep record from RED to GREEN through a full tick, so the item's clear is
// the reconcile's, not a collector-local special case (gt-vsct7.5).
func TestCollectTierSweepRed_RaisesTheRedTiersAndClearsOnGreen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.tierSweepRigs = func() []string { return []string{attentionRig} }
	f.src.tierSweep = func(string) (tierSweepState, error) {
		return tierSweepState{Tiers: map[string]tierSweepTierResult{
			"shell":       {Verdict: tierSweepRed, Passed: 5, Failed: 1, FailedNames: []string{"scripts/x.sh"}},
			"integration": {Verdict: tierSweepGreen, Passed: 29},
		}}, nil
	}

	st := f.tick(t, now)
	it, ok := attention.Find(st, "sweep:gastown:shell")
	if !ok {
		t.Fatalf("items = %+v, want sweep:gastown:shell", st.Items)
	}
	if it.Kind != attention.KindTierSweepRed || it.Severity != attention.SeverityHigh || it.Rig != attentionRig {
		t.Errorf("item = %+v", it)
	}
	if _, ok := attention.Find(st, "sweep:gastown:integration"); ok {
		t.Error("a GREEN tier raised an item")
	}

	// The tier goes green: the next tick clears the item.
	f.src.tierSweep = func(string) (tierSweepState, error) {
		return tierSweepState{Tiers: map[string]tierSweepTierResult{
			"shell":       {Verdict: tierSweepGreen, Passed: 6},
			"integration": {Verdict: tierSweepGreen, Passed: 29},
		}}, nil
	}
	st = f.tick(t, now.Add(time.Minute))
	if _, ok := attention.Find(st, "sweep:gastown:shell"); ok {
		t.Errorf("items = %+v, want the shell item cleared", st.Items)
	}
}

// TestCollectTierSweepRed_NoRigsWhenThePatrolIsOff pins that turning the patrol
// off clears its items rather than holding a stale red forever.
func TestCollectTierSweepRed_NoRigsWhenThePatrolIsOff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.tierSweep = func(string) (tierSweepState, error) {
		return tierSweepState{Tiers: map[string]tierSweepTierResult{
			"shell": {Verdict: tierSweepRed, Failed: 1},
		}}, nil
	}
	if items := f.collect(t, f.src.collectTierSweepRed); len(items) != 0 {
		t.Errorf("items = %+v, want none: the fixture's sweep patrol is off", items)
	}
}
