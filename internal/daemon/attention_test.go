package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
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
	f.src.redMain = func(context.Context, string) ([]*beads.Issue, error) {
		return []*beads.Issue{{ID: "gt-red", Status: "open", Title: landworker.RedMainTitle(attentionRig, "internal/cmd")}}, nil
	}

	f.tick(t, now)
	want := "attention: +red-main:gastown:internal/cmd red main (gastown): internal/cmd"
	if got := f.logs.String(); !strings.Contains(got, want) {
		t.Errorf("log = %q, want %q", got, want)
	}

	// A beat with no change says nothing.
	f.logs.Reset()
	f.tick(t, now.Add(time.Minute))
	if got := f.logs.String(); got != "" {
		t.Errorf("log = %q, want nothing on a tick with no change", got)
	}

	// The condition clearing is one line, with the minus sign.
	f.logs.Reset()
	f.src.redMain = func(context.Context, string) ([]*beads.Issue, error) { return nil, nil }
	f.tick(t, now.Add(2*time.Minute))
	if got := f.logs.String(); !strings.Contains(got, "attention: -red-main:gastown:internal/cmd") {
		t.Errorf("log = %q, want the cleared line", got)
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
	if strings.Count(logs.String(), "attention: +esc:hq-9") != 1 {
		t.Errorf("log = %q, want one new-item line, not one per beat", logs.String())
	}
}
