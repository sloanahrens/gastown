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
	"github.com/steveyegge/gastown/internal/overseer"
	"github.com/steveyegge/gastown/internal/question"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// overseerRig is the silent-overseer collector's world: a heartbeat the test
// moves, ready work it sets, and a push that records its calls. The marker is
// the real file under the fixture's town, so "once per period" is tested
// through the same code a restart goes through.
type overseerRig struct {
	hb       overseer.Heartbeat
	have     bool
	hbErr    error
	ready    int
	pushes   []string
	pushErr  error
	markErr  error
	pushName string
}

func (r *overseerRig) install(f *attentionFixture) {
	root := f.d.config.TownRoot
	f.src.overseer = overseerReads{
		heartbeat:   func() (overseer.Heartbeat, bool, error) { return r.hb, r.have, r.hbErr },
		silentAfter: 90 * time.Minute,
		readyWork:   func(time.Time) int { return r.ready },
		pushName:    r.pushName,
		push: func(name, title, body string) error {
			if r.pushErr != nil {
				return r.pushErr
			}
			r.pushes = append(r.pushes, name+"|"+title+"|"+body)
			return nil
		},
		pushed: func() (overseer.SilentPush, bool, error) { return overseer.ReadSilentPush(root) },
		markPushed: func(p overseer.SilentPush) error {
			if r.markErr != nil {
				return r.markErr
			}
			return overseer.WriteSilentPush(root, p)
		},
	}
}

func (r *overseerRig) beat(at time.Time) {
	r.hb, r.have = overseer.Heartbeat{TS: at, State: overseer.StateWorking}, true
}

// overseerItems runs the silent-overseer collector at now.
func overseerItems(t *testing.T, f *attentionFixture, now time.Time) []attention.Item {
	t.Helper()
	f.src.now = now
	for _, c := range f.src.questionCollectors() {
		if c.kind == attention.KindOverseerSilent {
			return f.collect(t, c.collect)
		}
	}
	t.Fatal("no silent-overseer collector registered")
	return nil
}

func TestOverseerSilentFiresOnlyWithAStaleHeartbeatAndWaitingWork(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	fresh := now.Add(-10 * time.Minute)

	for _, tc := range []struct {
		name  string
		beat  *time.Time
		ready int
		// ask files one question: "" none, "new" with no comment,
		// "answered" with Sloan's reply, "awaiting" with the overseer's note.
		ask  string
		want bool
	}{
		{"fresh heartbeat, ready work", &fresh, 3, "", false},
		{"fresh heartbeat, new question", &fresh, 0, "new", false},
		{"stale heartbeat, nothing waiting", &stale, 0, "", false},
		{"stale heartbeat, ready work", &stale, 2, "", true},
		{"stale heartbeat, new question", &stale, 0, "new", true},
		{"stale heartbeat, question Sloan answered", &stale, 0, "answered", true},
		{"stale heartbeat, question waiting on Sloan", &stale, 0, "awaiting", false},
		{"no heartbeat ever, ready work", nil, 1, "", true},
		{"no heartbeat ever, nothing waiting", nil, 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			r := &overseerRig{ready: tc.ready}
			if tc.beat != nil {
				r.beat(*tc.beat)
			}
			r.install(f)
			l := &questionLedger{}
			switch tc.ask {
			case "new":
				l.ask("hq-q", "q", now.Add(-time.Hour))
			case "answered":
				l.ask("hq-q", "q", now.Add(-time.Hour))
				l.comment("hq-q", question.OverseerPrefix+" asked", now.Add(-50*time.Minute))
				l.comment("hq-q", "yes", now.Add(-5*time.Minute))
			case "awaiting":
				l.ask("hq-q", "q", now.Add(-time.Hour))
				l.comment("hq-q", question.OverseerPrefix+" asked", now.Add(-50*time.Minute))
			}
			f.src.questions = l.reads()

			items := overseerItems(t, f, now)
			if got := len(items) == 1; got != tc.want {
				t.Fatalf("items = %+v, want fired=%v", items, tc.want)
			}
			if !tc.want {
				return
			}
			it := items[0]
			if it.Key != "overseer:silent" || it.Kind != attention.KindOverseerSilent || it.Severity != attention.SeverityLow {
				t.Errorf("item = %+v", it)
			}
			if rule := attention.RuleFor(it); rule != attention.RuleOverseerSilent {
				t.Errorf("rule = %q", rule)
			}
		})
	}
}

// The event the rule raises is info: it never wakes the overseer it is about.
func TestOverseerSilentEventIsInfoAndNeverWakes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	r := &overseerRig{ready: 1}
	r.beat(now.Add(-3 * time.Hour))
	r.install(f)

	f.tick(t, now)
	var got *attention.Event
	for _, e := range f.events(t) {
		if e.Key == "overseer:silent" {
			got = &e
		}
	}
	if got == nil {
		t.Fatalf("no overseer:silent event in %+v", f.events(t))
	}
	if got.Rule != attention.RuleOverseerSilent || got.EffectiveHandled() != attention.HandledInfo || got.Wake {
		t.Errorf("event = %+v, want rule overseer.silent, info, no wake", *got)
	}

	// The beat arrives: the item clears.
	r.beat(now.Add(time.Minute))
	st := f.tick(t, now.Add(2*time.Minute))
	if _, ok := attention.Find(st, "overseer:silent"); ok {
		t.Error("overseer:silent still held after a fresh heartbeat")
	}
}

func TestOverseerSilentPushesOncePerSilentPeriod(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, t0)
	r := &overseerRig{ready: 2, pushName: "ntfy"}
	last := t0.Add(-2 * time.Hour)
	r.beat(last)
	r.install(f)

	overseerItems(t, f, t0)
	if len(r.pushes) != 1 {
		t.Fatalf("pushes = %v, want one", r.pushes)
	}
	if !strings.HasPrefix(r.pushes[0], "ntfy|Overseer is silent|") || !strings.Contains(r.pushes[0], "2 ready bead(s)") {
		t.Errorf("push = %q", r.pushes[0])
	}

	// Ticks later in the same period, with the work count flapping to zero and
	// back: still one push.
	overseerItems(t, f, t0.Add(3*time.Minute))
	r.ready = 0
	overseerItems(t, f, t0.Add(6*time.Minute))
	r.ready = 5
	overseerItems(t, f, t0.Add(9*time.Minute))
	if len(r.pushes) != 1 {
		t.Fatalf("pushes after the same period = %v, want still one", r.pushes)
	}

	// A new beat ends the period; going stale again begins another.
	next := t0.Add(time.Hour)
	r.beat(next)
	overseerItems(t, f, next.Add(30*time.Minute))
	if len(r.pushes) != 1 {
		t.Fatalf("pushed while the heartbeat was fresh: %v", r.pushes)
	}
	overseerItems(t, f, next.Add(2*time.Hour))
	if len(r.pushes) != 2 {
		t.Errorf("pushes = %v, want a second one for the second period", r.pushes)
	}
}

// A restart carries nothing in memory; the marker file is the whole memory.
func TestOverseerSilentPushSurvivesARestart(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, t0)
	r := &overseerRig{ready: 1, pushName: "ntfy"}
	r.beat(t0.Add(-2 * time.Hour))
	r.install(f)
	overseerItems(t, f, t0)

	r2 := &overseerRig{ready: 1, pushName: "ntfy", hb: r.hb, have: true}
	r2.install(f)
	overseerItems(t, f, t0.Add(time.Minute))
	if len(r.pushes)+len(r2.pushes) != 1 {
		t.Errorf("pushes = %v then %v, want one in all", r.pushes, r2.pushes)
	}
}

func TestOverseerSilentPushFailureRetriesAndUnreadableMarkerDoesNotPush(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	t.Run("failed push is tried again", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, t0)
		r := &overseerRig{ready: 1, pushName: "ntfy", pushErr: errors.New("connection refused")}
		r.beat(t0.Add(-2 * time.Hour))
		r.install(f)
		if items := overseerItems(t, f, t0); len(items) != 1 {
			t.Fatalf("a failed push dropped the item: %v", items)
		}
		if !strings.Contains(f.logs.String(), `push through "ntfy" failed`) {
			t.Errorf("the failure was not logged: %q", f.logs.String())
		}
		r.pushErr = nil
		overseerItems(t, f, t0.Add(time.Minute))
		if len(r.pushes) != 1 {
			t.Errorf("pushes = %v, want the retry to land once", r.pushes)
		}
	})

	t.Run("unreadable marker", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, t0)
		r := &overseerRig{ready: 1, pushName: "ntfy"}
		r.beat(t0.Add(-2 * time.Hour))
		r.install(f)
		if err := os.MkdirAll(overseer.Dir(f.d.config.TownRoot), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(overseer.SilentPushPath(f.d.config.TownRoot), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if items := overseerItems(t, f, t0); len(items) != 1 {
			t.Fatalf("items = %v", items)
		}
		if len(r.pushes) != 0 {
			t.Errorf("pushed past an unreadable marker: %v", r.pushes)
		}
	})

	t.Run("no command configured", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, t0)
		r := &overseerRig{ready: 1}
		r.beat(t0.Add(-2 * time.Hour))
		r.install(f)
		if items := overseerItems(t, f, t0); len(items) != 1 {
			t.Fatalf("items = %v", items)
		}
		if len(r.pushes) != 0 {
			t.Errorf("pushed with no silent_push configured: %v", r.pushes)
		}
	})
}

// The rule acts on nothing: the only file it may write is its own marker, and
// the fixture's daemon has no tmux, so a call into a session would panic.
func TestOverseerSilentTakesNoActionOnAnySession(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, t0)
	r := &overseerRig{ready: 4, pushName: "ntfy"}
	r.beat(t0.Add(-5 * time.Hour))
	r.install(f)

	overseerItems(t, f, t0)

	var written []string
	root := f.d.config.TownRoot
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			written = append(written, rel)
		}
		return nil
	})
	want := filepath.Join(".runtime", "overseer", "silent-push.json")
	if len(written) != 1 || written[0] != want {
		t.Errorf("files written = %v, want only %s", written, want)
	}
}

// A heartbeat that cannot be read, or questions that cannot be read when they
// are the only evidence, fail the collector: the queue keeps what it had
// rather than raising or clearing on a guess.
func TestOverseerSilentFailsTheCollectorOnAnUnreadableInput(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	f := newAttentionFixture(t, now)
	r := &overseerRig{hbErr: errors.New("parse"), ready: 1}
	r.install(f)
	f.src.now = now
	for _, c := range f.src.questionCollectors() {
		if c.kind != attention.KindOverseerSilent {
			continue
		}
		if _, err := c.collect(context.Background()); err == nil {
			t.Error("a corrupt heartbeat read as fine")
		}
	}

	f2 := newAttentionFixture(t, now)
	r2 := &overseerRig{ready: 0}
	r2.beat(now.Add(-3 * time.Hour))
	r2.install(f2)
	f2.src.questions = (&questionLedger{err: errors.New("dolt down")}).reads()
	f2.src.now = now
	for _, c := range f2.src.questionCollectors() {
		if c.kind != attention.KindOverseerSilent {
			continue
		}
		if _, err := c.collect(context.Background()); err == nil {
			t.Error("unreadable questions read as none waiting")
		}
	}
}

func TestOverseerReadyWorkOfTicks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tick := func(age time.Duration, candidates int) townhealth.DispatchTick {
		return townhealth.DispatchTick{At: now.Add(-age), Candidates: candidates}
	}
	for _, tc := range []struct {
		name  string
		ticks []townhealth.DispatchTick
		want  int
	}{
		{"no ticks", nil, 0},
		{"newest tick counts", []townhealth.DispatchTick{tick(5*time.Minute, 9), tick(time.Minute, 2)}, 2},
		{"newest tick has none", []townhealth.DispatchTick{tick(2*time.Minute, 4), tick(time.Minute, 0)}, 0},
		{"too old to say", []townhealth.DispatchTick{tick(time.Hour, 6)}, 0},
	} {
		if got := readyWorkOfTicks(tc.ticks, now); got != tc.want {
			t.Errorf("%s: readyWorkOfTicks = %d, want %d", tc.name, got, tc.want)
		}
	}
}
