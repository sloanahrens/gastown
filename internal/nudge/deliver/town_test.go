package deliver

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
)

// townLog records what a Town logged and which DND levels it read.
type townLog struct {
	mu     sync.Mutex
	logged []string // "sender rig target message"
	read   []string // agent bead ids
	levels map[string]string
	err    error
}

func (l *townLog) log(_, sender, rig, target, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logged = append(l.logged, strings.Join([]string{sender, rig, target, message}, " "))
}

func (l *townLog) level(_, id string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.read = append(l.read, id)
	return l.levels[id], l.err
}

func (l *townLog) entries() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.logged...)
}

// testTown is a Town over ft in a scratch town whose gastown rig has prefix gt.
func testTown(t *testing.T, ft *deliveryTmux) (*Town, *townLog) {
	t.Helper()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	l := &townLog{levels: map[string]string{}}
	return &Town{
		Delivery:          testDelivery(ft, t.TempDir(), &pollerLog{}, &bytes.Buffer{}),
		Registry:          reg,
		NotificationLevel: l.level,
		Log:               l.log,
	}, l
}

func TestTownNudgesMayorByRoleShortcut(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, "hq-mayor")
	ft.SetIdle("hq-mayor", true)
	town, l := testTown(t, ft)

	if err := town.Nudge(t.Context(), "mayor/", "dispatch", "unknown"); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if got := ft.Sent("hq-mayor"); len(got) != 1 || !strings.Contains(got[0], "[from unknown] dispatch") {
		t.Fatalf("sent = %q, want the nudge delivered to hq-mayor", got)
	}
	if got := l.entries(); len(got) != 1 || got[0] != "unknown  hq-mayor dispatch" {
		t.Errorf("logged = %q, want one entry for hq-mayor", got)
	}
	if len(l.read) != 1 || l.read[0] != "hq-mayor" {
		t.Errorf("DND read %v, want hq-mayor's bead", l.read)
	}
}

func TestTownSkipsMutedTarget(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, "hq-mayor")
	ft.SetIdle("hq-mayor", true)
	town, l := testTown(t, ft)
	l.levels["hq-mayor"] = beads.NotifyMuted

	if err := town.Nudge(t.Context(), "mayor", "dispatch", "unknown"); err != nil {
		t.Fatalf("Nudge to a muted target = %v, want a silent skip", err)
	}
	if got := ft.Sent("hq-mayor"); len(got) != 0 {
		t.Fatalf("sent = %q, want nothing to a muted target", got)
	}
	if got := l.entries(); len(got) != 0 {
		t.Errorf("logged = %q, want nothing", got)
	}
}

func TestTownDeliversWhenDNDUnreadable(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, "hq-mayor")
	ft.SetIdle("hq-mayor", true)
	town, l := testTown(t, ft)
	l.err = errors.New("no such bead")

	if err := town.Nudge(t.Context(), "mayor", "dispatch", "unknown"); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if got := ft.Sent("hq-mayor"); len(got) != 1 {
		t.Fatalf("sent = %q, want the nudge delivered (DND fails open)", got)
	}
}

func TestTownResolvesRigAddresses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target, session, rig string
		live                       []string
	}{
		{"short address prefers crew", "gastown/max", "gt-crew-max", "gastown", []string{"gt-crew-max", "gt-max"}},
		{"short address falls back to polecat", "gastown/toast", "gt-toast", "gastown", []string{"gt-toast"}},
		{"explicit crew", "gastown/crew/max", "gt-crew-max", "gastown", []string{"gt-crew-max"}},
		{"explicit polecat", "gastown/polecats/max", "gt-max", "gastown", []string{"gt-crew-max", "gt-max"}},
		{"raw session name", "gt-toast", "gt-toast", "", []string{"gt-toast"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ft := newDeliveryTmux(t, tc.live...)
			ft.SetIdle(tc.session, true)
			town, l := testTown(t, ft)

			if err := town.Nudge(t.Context(), tc.target, "hi", "mayor"); err != nil {
				t.Fatalf("Nudge(%q): %v", tc.target, err)
			}
			if got := ft.Sent(tc.session); len(got) != 1 {
				t.Fatalf("sent to %s = %q, want the nudge", tc.session, got)
			}
			if got, want := l.entries(), "mayor "+tc.rig+" "+tc.target+" hi"; len(got) != 1 || got[0] != want {
				t.Errorf("logged = %q, want [%q]", got, want)
			}
		})
	}
}

func TestTownRefusesUndeliverableTargets(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"gt-gone", "gastown/gone", "mayor/x", "deacon/dogs", "channel:workers", "/x"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			ft := newDeliveryTmux(t)
			town, l := testTown(t, ft)
			if err := town.Nudge(t.Context(), target, "hi", "mayor"); err == nil {
				t.Fatalf("Nudge(%q) succeeded, want an error", target)
			}
			if n := nudge.QueueLen(town.Delivery.TownRoot, "gt-gone"); n != 0 {
				t.Errorf("queued %d, want nothing", n)
			}
			if got := l.entries(); len(got) != 0 {
				t.Errorf("logged = %q, want nothing", got)
			}
		})
	}
}

// TestDeliveryStopsWhenContextEndsWhileWaitingForIdle is the in-process form
// of killing `gt nudge` on its deadline mid wait-idle: the wait is cut to the
// deadline however long the idle timeout is, and nothing is queued or typed
// afterwards.
func TestDeliveryStopsWhenContextEndsWhileWaitingForIdle(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy
	town := t.TempDir()
	d := testDelivery(ft, town, &pollerLog{}, &bytes.Buffer{})
	d.WaitIdleTimeout = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := d.Deliver(ctx, deliveryTarget, "m", "mayor")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Deliver = %v, want context.DeadlineExceeded", err)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 0 {
		t.Errorf("queue = %d, want nothing queued after the deadline", n)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Errorf("sent = %q, want nothing", got)
	}
}

// TestDeliveryStopsWhenContextEndsWhileWatching: a deadline inside the
// post-queue watch ends it and reports the deadline, leaving the nudge queued
// for the poller, as a killed `gt nudge` did.
func TestDeliveryStopsWhenContextEndsWhileWatching(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy
	town := t.TempDir()
	d := testDelivery(ft, town, &pollerLog{}, &bytes.Buffer{})
	d.WatchTimeout = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	err := d.Deliver(ctx, deliveryTarget, "m", "mayor")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Deliver = %v, want context.DeadlineExceeded", err)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 1 {
		t.Errorf("queue = %d, want the nudge left queued", n)
	}
}

func TestDeliveryRefusesDoneContext(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := testDelivery(ft, t.TempDir(), &pollerLog{}, &bytes.Buffer{}).Deliver(ctx, deliveryTarget, "m", "mayor"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Deliver = %v, want context.Canceled", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Errorf("sent = %q, want nothing", got)
	}
}

func TestSender(t *testing.T) {
	t.Parallel()
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	for _, tc := range []struct {
		cwd  string
		env  map[string]string
		want string
	}{
		{"/town", nil, "unknown"},
		{"/town/mayor", nil, "mayor"},
		{"/town/gastown/crew/max/sub", nil, "gastown/crew/max"},
		{"/town/gastown/polecats/toast", map[string]string{"GT_ROLE": "polecat"}, "gastown/toast"},
		{"/town", map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "max"}, "gastown/crew/max"},
	} {
		if got := Sender(tc.cwd, "/town", env(tc.env)); got != tc.want {
			t.Errorf("Sender(%q, %v) = %q, want %q", tc.cwd, tc.env, got, tc.want)
		}
	}
}
