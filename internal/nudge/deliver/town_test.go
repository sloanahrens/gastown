package deliver

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
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

// testTown is a Town over ft in a scratch town whose one rig, gastown, has
// prefix gt, and whose one nudge channel, workers, is gastown's polecats and
// the mayor.
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
		RigExists:         func(_, rig string) bool { return rig == "gastown" },
		Channels: func(string) (map[string][]string, error) {
			return map[string][]string{"workers": {"gastown/polecats/*", "mayor"}, "empty": nil}, nil
		},
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
	for _, target := range []string{"gt-gone", "gastown/gone", "nowhere/polecats/gone", "mayor/x", "deacon/dogs", "channel:nosuch", "channel:empty", "/x"} {
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

// TestDeliveryStopsWhenContextEndsWhileWatching: a context that ends inside
// the post-queue watch ends it and is reported, leaving the nudge queued for
// the poller, as a killed `gt nudge` did.
func TestDeliveryStopsWhenContextEndsWhileWatching(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ft.onWait = func(n int) {
		if n == 2 { // the watcher's first poll; the first wait was wait-idle's
			cancel()
		}
	}
	town := t.TempDir()
	d := testDelivery(ft, town, &pollerLog{}, &bytes.Buffer{})
	d.WatchTimeout = time.Hour

	err := d.Deliver(ctx, deliveryTarget, "m", "mayor")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Deliver = %v, want context.Canceled", err)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 1 {
		t.Errorf("queue = %d, want the nudge left queued", n)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Errorf("sent = %q, want nothing", got)
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

func TestTownRefusesPolecatOfUnknownRig(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, "gt-toast")
	ft.SetIdle("gt-toast", true)
	town, _ := testTown(t, ft)
	// "beads" has no prefix, so its polecat would resolve to gastown's gt-toast.
	err := town.Nudge(t.Context(), "beads/toast", "hi", "mayor")
	if err == nil || !strings.Contains(err.Error(), "rig 'beads' not found") {
		t.Fatalf("Nudge(beads/toast) = %v, want rig not found", err)
	}
	if got := ft.Sent("gt-toast"); len(got) != 0 {
		t.Errorf("sent = %q, want nothing", got)
	}
}

func TestTownNudgesChannelMembers(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, "hq-mayor", "gt-alpha", "gt-beta", "gt-crew-max")
	for _, s := range []string{"hq-mayor", "gt-alpha", "gt-beta", "gt-crew-max"} {
		ft.SetIdle(s, true)
	}
	town, l := testTown(t, ft)
	l.levels["gt-beta"] = beads.NotifyMuted
	var reported []string
	town.Member = func(m ChannelMember) { reported = append(reported, m.Session+":"+m.DND) }

	if err := town.Nudge(t.Context(), "channel:workers", "standup", "mayor"); err != nil {
		t.Fatalf("Nudge(channel:workers): %v", err)
	}
	if want := []string{"gt-alpha:", "gt-beta:" + beads.NotifyMuted, "hq-mayor:"}; strings.Join(reported, ",") != strings.Join(want, ",") {
		t.Errorf("members = %q, want %q", reported, want)
	}
	for session, n := range map[string]int{"gt-alpha": 1, "hq-mayor": 1, "gt-beta": 0, "gt-crew-max": 0} {
		if got := ft.Sent(session); len(got) != n {
			t.Errorf("sent to %s = %q, want %d nudge(s)", session, got, n)
		}
	}
	if got := l.entries(); len(got) != 1 || got[0] != "mayor  channel:workers standup" {
		t.Errorf("logged = %q, want one channel entry", got)
	}
}

func TestTownChannelReportsFailedMembers(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, "gt-alpha")
	ft.SetIdle("gt-alpha", true)
	town, _ := testTown(t, ft)

	results, err := town.NudgeChannel(t.Context(), "workers", "standup", "mayor")
	if err == nil {
		t.Fatal("NudgeChannel succeeded, want the failed delivery reported")
	}
	// hq-mayor is not running, so the mayor pattern names a dead session.
	if len(results) != 2 || results[0].Session != "gt-alpha" || results[0].Err != nil || results[1].Session != "hq-mayor" || results[1].Err == nil {
		t.Errorf("results = %+v, want gt-alpha then a failed hq-mayor", results)
	}
}

func TestChannelSessions(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("bd", "beads")
	live := []string{"bd-gamma", "gt-beta", "gt-crew-max", "gt-alpha", "gt-crew-jack", "hq-mayor", "plaintext"}
	for _, tc := range []struct {
		patterns []string
		want     string
	}{
		{[]string{"mayor"}, "hq-mayor"},
		{[]string{"gastown/polecats/*"}, "gt-alpha gt-beta"},
		{[]string{"gastown/polecats/alpha"}, "gt-alpha"},
		{[]string{"gastown/crew/*"}, "gt-crew-jack gt-crew-max"},
		{[]string{"gastown/crew/max"}, "gt-crew-max"},
		{[]string{"gastown/alpha"}, "gt-alpha"},
		{[]string{"*/polecats/*"}, "bd-gamma gt-alpha gt-beta"},
		{[]string{"gastown/*"}, ""},
		{[]string{"nonexistent/polecats/*"}, ""},
		{[]string{"invalid"}, ""},
		{[]string{"gastown/crew/max", "*/crew/*", "mayor"}, "gt-crew-max gt-crew-jack hq-mayor"},
	} {
		if got := strings.Join(ChannelSessions(reg, tc.patterns, live), " "); got != tc.want {
			t.Errorf("ChannelSessions(%q) = %q, want %q", tc.patterns, got, tc.want)
		}
	}
}

func TestSessionAddress(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	for session, want := range map[string]string{
		"hq-mayor":    "mayor",
		"gt-witness":  "gastown/witness",
		"gt-crew-max": "gastown/crew/max",
		"gt-alpha":    "gastown/alpha",
		"plaintext":   "",
		"gt-":         "",
	} {
		if got := SessionAddress(reg, session); got != want {
			t.Errorf("SessionAddress(%q) = %q, want %q", session, got, want)
		}
	}
}

// TestSender pins who a nudge is attributed to, by the caller's working
// directory (relative to the town root) and identity environment; gt nudge
// and the daemon's in-process Notifier both attribute with Sender.
func TestSender(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cwd  string
		env  map[string]string
		want string
	}{
		{"town root, no identity (the daemon)", ".", nil, "unknown"},
		{"mayor dir", "mayor", nil, "mayor"},
		{"crew dir", "gastown/crew/max", nil, "gastown/crew/max"},
		{"crew subdir", "gastown/crew/max/sub", nil, "gastown/crew/max"},
		{"polecat dir", "gastown/polecats/toast", nil, "gastown/toast"},
		{"rig root", "gastown", nil, "unknown"},
		{"retired deacon dir", "deacon", nil, "unknown"},
		{"GT_ROLE mayor anywhere", ".", map[string]string{"GT_ROLE": "mayor"}, "mayor"},
		{"GT_ROLE crew", ".", map[string]string{"GT_ROLE": "gastown/crew/max"}, "gastown/crew/max"},
		{"GT_ROLE crew with GT_RIG and GT_CREW", ".", map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "max"}, "gastown/crew/max"},
		{"GT_ROLE polecat filled from cwd", "gastown/polecats/toast", map[string]string{"GT_ROLE": "polecat"}, "gastown/toast"},
		{"GT_ROLE beats cwd", "gastown/crew/max", map[string]string{"GT_ROLE": "mayor"}, "mayor"},
		{"GT_ROLE unknown simple role", ".", map[string]string{"GT_ROLE": "overseer"}, "overseer"},
		{"GT_ROLE retired witness", ".", map[string]string{"GT_ROLE": "gastown/witness"}, "gastown/witness"},
	} {
		getenv := func(k string) string { return tc.env[k] }
		if got := Sender(filepath.Join("/town", tc.cwd), "/town", getenv); got != tc.want {
			t.Errorf("%s: Sender = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := Sender("/elsewhere", "", func(string) string { return "mayor" }); got != "unknown" {
		t.Errorf("Sender outside a town = %q, want unknown", got)
	}
}
