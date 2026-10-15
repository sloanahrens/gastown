package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/nudge/deliver"
	"github.com/steveyegge/gastown/internal/tmux"
)

// nudgeTmux is a tmux server holding idle sessions. A wait on it blocks until
// release is closed when hang is set, like a tmux call that never returns.
type nudgeTmux struct {
	mu       sync.Mutex
	sessions map[string]bool
	sent     map[string][]string
	hang     bool
	busy     bool // a wait for idle times out, as on a seat that is mid-turn
	release  chan struct{}
}

func newNudgeTmux(sessions ...string) *nudgeTmux {
	f := &nudgeTmux{sessions: map[string]bool{}, sent: map[string][]string{}, release: make(chan struct{})}
	for _, s := range sessions {
		f.sessions[s] = true
	}
	return f
}

func (f *nudgeTmux) HasSession(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[name], nil
}

func (f *nudgeTmux) ListSessions() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for name := range f.sessions {
		names = append(names, name)
	}
	return names, nil
}

func (f *nudgeTmux) IsBusy(string) bool { return false }

func (f *nudgeTmux) WaitForIdle(session string, _ time.Duration) error {
	if f.hang {
		<-f.release
	}
	if f.busy {
		return tmux.ErrIdleTimeout
	}
	if ok, _ := f.HasSession(session); !ok {
		return tmux.ErrSessionNotFound
	}
	return nil
}

func (f *nudgeTmux) NudgeSessionWithOpts(session, message string, _ tmux.NudgeOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[session] = append(f.sent[session], message)
	return nil
}

func (f *nudgeTmux) WaitForInputConsumed(string, time.Duration) (tmux.InputConsumption, error) {
	return tmux.InputConsumptionStartedTurn, nil
}

func (f *nudgeTmux) SessionAgentPreset(string, string) (string, *config.AgentPresetInfo, bool) {
	return "", nil, false
}

func (f *nudgeTmux) sentTo(session string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[session]...)
}

// nudgeTown is a scratch town root (mayor/town.json) with a gastown crew dir.
func nudgeTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"mayor", "gastown/crew/max"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(`{"type":"town"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// sent records the town root and sender of every nudge a TownNudger made.
type sent struct {
	mu        sync.Mutex
	townRoots []string
	senders   []string
}

// townNudger is a TownNudger over ft whose deliveries record into s, with
// no DND and millisecond timings.
func townNudger(dir string, env []string, ft *nudgeTmux, s *sent) *TownNudger {
	return &TownNudger{
		Dir: dir,
		Env: func() []string { return env },
		town: func(townRoot string) *deliver.Town {
			s.mu.Lock()
			s.townRoots = append(s.townRoots, townRoot)
			s.mu.Unlock()
			d := deliver.New(ft, townRoot)
			d.WaitIdleTimeout, d.WatchTimeout, d.PollInterval, d.ProbeWindow = time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond
			d.StartPoller = func(string, string) (int, error) { return 0, nil }
			return &deliver.Town{
				Delivery:          d,
				NotificationLevel: func(string, string) (string, error) { return "", nil },
				Log: func(_, sender, _, _, _ string) {
					s.mu.Lock()
					defer s.mu.Unlock()
					s.senders = append(s.senders, sender)
				},
			}
		},
	}
}

func TestTownNudgerAttributesTheNudgeByDirAndEnv(t *testing.T) {
	t.Parallel()
	root := nudgeTown(t)
	for _, tc := range []struct {
		name string
		dir  string
		env  []string
		want string
	}{
		{"the daemon at the town root", root, []string{"BD_ACTOR=daemon"}, "unknown"},
		{"a crew dir", filepath.Join(root, "gastown/crew/max"), nil, "gastown/crew/max"},
		{"GT_ROLE, last assignment wins", root, []string{"GT_ROLE=gastown/polecats/alpha", "GT_ROLE=gastown/crew/max"}, "gastown/crew/max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ft, s := newNudgeTmux("gt-crew-max"), &sent{}
			if err := townNudger(tc.dir, tc.env, ft, s).Nudge(t.Context(), "gt-crew-max", "dispatch"); err != nil {
				t.Fatalf("Nudge: %v", err)
			}
			if len(s.townRoots) != 1 || s.townRoots[0] != root {
				t.Errorf("town roots = %v, want [%s]", s.townRoots, root)
			}
			if len(s.senders) != 1 || s.senders[0] != tc.want {
				t.Errorf("senders = %v, want [%s]", s.senders, tc.want)
			}
			if got := ft.sentTo("gt-crew-max"); len(got) != 1 || !strings.Contains(got[0], "[from "+tc.want+"] dispatch") {
				t.Errorf("sent = %q, want the nudge from %s", got, tc.want)
			}
		})
	}
}

func TestTownNudgerReportsAMissingSession(t *testing.T) {
	t.Parallel()
	err := townNudger(nudgeTown(t), nil, newNudgeTmux(), &sent{}).Nudge(t.Context(), "gt-gone", "m")
	if err == nil || !strings.Contains(err.Error(), `gt nudge: session "gt-gone" not found`) {
		t.Fatalf("Nudge = %v, want the missing session named", err)
	}
	if errors.Is(err, ErrInvalid) {
		t.Fatalf("Nudge = %v: a well-formed nudge must not read as invalid", err)
	}
}

// TestTownNudgerReturnsAtTheDeadlineWhenTmuxHangs is kill-on-timeout in
// process: a tmux call that never returns cannot hold the caller past its
// deadline, and the deadline is reported as such.
func TestTownNudgerReturnsAtTheDeadlineWhenTmuxHangs(t *testing.T) {
	t.Parallel()
	ft := newNudgeTmux("gt-crew-max")
	ft.hang = true
	defer close(ft.release)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := townNudger(nudgeTown(t), nil, ft, &sent{}).Nudge(ctx, "gt-crew-max", "m")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Nudge = %v, want context.DeadlineExceeded", err)
	}
	if got := ft.sentTo("gt-crew-max"); len(got) != 0 {
		t.Errorf("sent = %q, want nothing", got)
	}
}

// TestTownNudgerQueueOptionDeliversToABusySeat is the overrun guard's nudge
// (gt-t61ft). The default mode queues the nudge for a seat that is mid-turn
// and then watches the seat for WatchTimeout, so a caller whose deadline is
// shorter is told the deadline passed although the nudge is queued. Queue
// mode writes the queue and returns without waiting on the seat.
func TestTownNudgerQueueOptionDeliversToABusySeat(t *testing.T) {
	t.Parallel()
	const session = "gt-crew-max"
	// deadline is the caller's, and it must be a bound on what the nudge waits
	// for, never on how fast a loaded host writes the queue: the gate runs this
	// beside the whole parallel suite, where a 300 ms deadline fired before the
	// queue write (load average 30). The default mode blocks on the watch until
	// the deadline, so its deadline is the test's duration and stays short;
	// queue mode must return long before the one-minute watch, so its deadline
	// is generous and still far under it: a queue mode that waited on the seat
	// would fail the call with DeadlineExceeded after 20 s.
	cases := []struct {
		name     string
		opts     []NudgeOption
		deadline time.Duration
		wantErr  error
	}{
		{"default waits out the watch", nil, 2 * time.Second, context.DeadlineExceeded},
		{"queue option returns at once", []NudgeOption{Queue()}, 20 * time.Second, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := nudgeTown(t)
			ft := newNudgeTmux(session)
			ft.busy = true
			n := townNudger(root, nil, ft, &sent{})
			build := n.town
			n.town = func(townRoot string) *deliver.Town {
				town := build(townRoot)
				town.Delivery.WatchTimeout = time.Minute // the real watch outlasts the caller's deadline
				return town
			}
			ctx, cancel := context.WithTimeout(t.Context(), tc.deadline)
			defer cancel()

			err := n.Nudge(ctx, session, "split proposal", tc.opts...)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("Nudge = %v, want %v", err, tc.wantErr)
			}
			if got := nudge.QueueLen(root, session); got != 1 {
				t.Errorf("queue holds %d nudges, want the one just sent", got)
			}
			if got := ft.sentTo(session); len(got) != 0 {
				t.Errorf("typed into a busy seat: %q", got)
			}
		})
	}
}

func TestTownNudgerRefusesBeforeDelivering(t *testing.T) {
	t.Parallel()
	n := &TownNudger{town: func(string) *deliver.Town {
		t.Error("delivery started for a refused nudge")
		return nil
	}}
	if err := n.Nudge(t.Context(), " ", "m"); !errors.Is(err, ErrInvalid) {
		t.Errorf("blank target: %v, want ErrInvalid", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := n.Nudge(ctx, "gt-crew-max", "m"); !errors.Is(err, context.Canceled) {
		t.Errorf("done context: %v, want context.Canceled", err)
	}
}

func TestCLIHandsNudgesToItsNudger(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	ft, s := newNudgeTmux("gt-crew-max"), &sent{}
	cli := newTestCLI(r)
	cli.Nudger = townNudger(nudgeTown(t), nil, ft, s)

	if err := cli.Nudge(t.Context(), "gt-crew-max", "MERGED: mr-1"); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("ran %+v, want no gt", r.calls)
	}
	if got := ft.sentTo("gt-crew-max"); len(got) != 1 {
		t.Fatalf("sent = %q, want the nudge delivered in-process", got)
	}
	if err := cli.Nudge(t.Context(), "gt-crew-max", " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank message: %v, want ErrInvalid", err)
	}
}
