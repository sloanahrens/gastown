package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltpause"
)

// A paused server is neither started, probed nor restarted; once the pause
// lapses the manager starts it again (gt-8z769.2).
func TestEnsureRunning_PausedServerIsLeftAlone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)
	m := newTestManager(t)
	m.nowFn = func() time.Time { return now }
	var mu sync.Mutex
	var logs []string
	m.logger = func(format string, v ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	}
	starts, probes := 0, 0
	m.startFn = func() error { starts++; return nil }
	m.healthCheckFn = func() error { probes++; return nil }
	if err := doltpause.Write(m.townRoot, doltpause.Marker{
		Actor: "deacon", Reason: "weekly gc", Until: now.Add(time.Hour), Since: now,
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := m.EnsureRunning(); err != nil {
			t.Fatalf("EnsureRunning while paused: %v", err)
		}
	}
	if starts != 0 || probes != 0 {
		t.Fatalf("paused: starts=%d probes=%d, want 0 and 0", starts, probes)
	}
	pauseLogs := 0
	for _, l := range logs {
		if strings.Contains(l, "not starting, probing or restarting") {
			pauseLogs++
		}
	}
	if pauseLogs != 1 {
		t.Errorf("pause logged %d times over 3 ticks, want once: %q", pauseLogs, logs)
	}

	now = now.Add(2 * time.Hour)
	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning after the pause lapsed: %v", err)
	}
	if starts != 1 {
		t.Errorf("after the pause lapsed: starts=%d, want 1", starts)
	}
}
