package feed

import (
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAddEventConcurrentWithView verifies that addEvent and View can run
// concurrently without data races. Run with -race to detect issues.
func TestAddEventConcurrentWithView(t *testing.T) {
	m := NewModel(nil)
	m.mu.Lock()
	m.width = 80
	m.height = 40
	m.mu.Unlock()

	var wg sync.WaitGroup

	// Writer goroutine: add events rapidly
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.addEvent(Event{
				Time:    time.Now(),
				Type:    "update",
				Actor:   "gastown/crew/test",
				Target:  "gt-xyz",
				Message: "test event",
				Rig:     "gastown",
				Role:    "crew",
			})
		}
	}()

	// Reader goroutine: call View() concurrently (the public API)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = m.View()
		}
	}()

	wg.Wait()
}

// TestSetEventChannelConcurrentWithListen verifies that SetEventChannel
// can be called concurrently with listenForEvents without data races.
func TestSetEventChannelConcurrentWithListen(t *testing.T) {
	m := NewModel(nil)

	var wg sync.WaitGroup

	// Writer goroutine: swap event channels
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			ch := make(chan Event, 1)
			m.SetEventChannel(ch)
		}
	}()

	// Reader goroutine: call listenForEvents (reads eventChan)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = m.listenForEvents()
		}
	}()

	wg.Wait()
}

// TestSetTownRootConcurrentWithFetch verifies that SetTownRoot can be called
// concurrently with fetchConvoys without data races.
func TestSetTownRootConcurrentWithFetch(t *testing.T) {
	m := NewModel(nil)

	var wg sync.WaitGroup

	// Writer goroutine: update town root
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.SetTownRoot("/tmp/test")
		}
	}()

	// Reader goroutine: call fetchConvoys (reads townRoot)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = m.fetchConvoys()
		}
	}()

	wg.Wait()
}

// TestMultipleWritersConcurrent verifies that multiple goroutines adding
// events concurrently don't cause data races on the events slice or rigs map.
func TestMultipleWritersConcurrent(t *testing.T) {
	m := NewModel(nil)
	m.mu.Lock()
	m.width = 80
	m.height = 40
	m.mu.Unlock()

	var wg sync.WaitGroup

	// Multiple writer goroutines
	for g := 0; g < 5; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				m.addEvent(Event{
					Time:    time.Now(),
					Type:    "create",
					Actor:   "gastown/crew/test",
					Target:  "gt-test",
					Message: "concurrent event",
					Rig:     "gastown",
					Role:    "crew",
				})
			}
		}(g)
	}

	// Concurrent reader
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = m.View()
		}
	}()

	wg.Wait()

	// Verify events were added (some may be deduplicated)
	if len(m.events) == 0 {
		t.Error("expected events to be added")
	}
}

// TestAddEventLocked verifies the locked mutation logic directly.
func TestAddEventLocked(t *testing.T) {
	m := NewModel(nil)

	tests := []struct {
		name        string
		event       Event
		wantUpdate  bool
		wantEvents  int // expected event count after this event
	}{
		{
			name: "normal event adds to feed",
			event: Event{
				Time:    time.Now(),
				Type:    "create",
				Actor:   "gastown/crew/joe",
				Target:  "gt-abc",
				Message: "created issue",
				Rig:     "gastown",
				Role:    "crew",
			},
			wantUpdate: true,
			wantEvents: 1,
		},
		{
			name: "update with empty target filtered out",
			event: Event{
				Time: time.Now(),
				Type: "update",
			},
			wantUpdate: false,
			wantEvents: 1, // unchanged from previous
		},
		{
			name: "rig info populates agent tree",
			event: Event{
				Time:    time.Now(),
				Type:    "create",
				Actor:   "beads/crew/wolf",
				Target:  "gt-def",
				Message: "wolf event",
				Rig:     "beads",
				Role:    "crew",
			},
			wantUpdate: true,
			wantEvents: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m.mu.Lock()
			got := m.addEventLocked(tc.event)
			m.mu.Unlock()

			if got != tc.wantUpdate {
				t.Errorf("addEventLocked() = %v, want %v", got, tc.wantUpdate)
			}
			if len(m.events) != tc.wantEvents {
				t.Errorf("len(events) = %d, want %d", len(m.events), tc.wantEvents)
			}
		})
	}

	// Verify agent tree was populated
	if _, ok := m.rigs["gastown"]; !ok {
		t.Error("expected gastown rig in tree")
	}
	if _, ok := m.rigs["beads"]; !ok {
		t.Error("expected beads rig in tree")
	}
}

// TestAddEventLocked_AgentTreeIgnoresOutOfOrderEvents reproduces gt-0qu: the
// agent tree's "last activity" for an agent must never regress to an older
// event just because that event happened to arrive later. Sources are
// polling/subprocess based and don't guarantee strict chronological arrival
// (e.g. a delayed source flushing a backlog after a faster source's live
// event already landed), so addEventLocked must compare timestamps instead
// of unconditionally overwriting agent.LastEvent/LastUpdate.
func TestAddEventLocked_AgentTreeIgnoresOutOfOrderEvents(t *testing.T) {
	m := NewModel(nil)

	now := time.Now()
	newer := Event{
		Time: now, Type: "create", Actor: "gastown/crew/joe",
		Target: "gt-new", Message: "newer event", Rig: "gastown", Role: "crew",
	}
	older := Event{
		Time: now.Add(-time.Hour), Type: "create", Actor: "gastown/crew/joe",
		Target: "gt-old", Message: "stale backlog event", Rig: "gastown", Role: "crew",
	}

	m.mu.Lock()
	m.addEventLocked(newer)
	m.addEventLocked(older) // arrives second but is chronologically older
	agent := m.rigs["gastown"].Agents["gastown/crew/joe"]
	m.mu.Unlock()

	if agent.LastUpdate.Before(now) {
		t.Errorf("agent tree regressed to a stale event: LastUpdate = %v, want >= %v", agent.LastUpdate, now)
	}
	if agent.LastEvent.Message != "newer event" {
		t.Errorf("agent tree shows stale LastEvent %q, want %q", agent.LastEvent.Message, "newer event")
	}
}

// TestEventsHistoryLimit verifies that the events slice doesn't grow beyond
// maxEventHistory.
func TestEventsHistoryLimit(t *testing.T) {
	m := NewModel(nil)

	// Add more than maxEventHistory events
	for i := 0; i < maxEventHistory+100; i++ {
		m.mu.Lock()
		m.addEventLocked(Event{
			Time:    time.Now().Add(time.Duration(i) * time.Millisecond),
			Type:    "create",
			Actor:   "gastown/crew/test",
			Target:  "gt-test",
			Message: "event",
			Rig:     "gastown",
			Role:    "crew",
		})
		m.mu.Unlock()
	}

	if len(m.events) > maxEventHistory {
		t.Errorf("events exceeded maxEventHistory: got %d, max %d",
			len(m.events), maxEventHistory)
	}
}

// TestViewConcurrentWithWindowResize verifies that View and WindowSizeMsg
// updates can run concurrently without data races on width/height.
func TestViewConcurrentWithWindowResize(t *testing.T) {
	m := NewModel(nil)
	m.mu.Lock()
	m.width = 80
	m.height = 40
	m.mu.Unlock()

	var wg sync.WaitGroup

	// Writer goroutine: send WindowSizeMsg updates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.Update(tea.WindowSizeMsg{Width: 80 + i, Height: 40 + i})
		}
	}()

	// Reader goroutine: call View() concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = m.View()
		}
	}()

	wg.Wait()
}

// TestViewConcurrentWithKeyHandling verifies that View and handleKey
// focus/help toggles can run concurrently without data races.
func TestViewConcurrentWithKeyHandling(t *testing.T) {
	m := NewModel(nil)
	m.mu.Lock()
	m.width = 80
	m.height = 40
	m.mu.Unlock()

	var wg sync.WaitGroup

	// Writer goroutine: toggle help and cycle panels
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.Update(tea.KeyMsg{Type: tea.KeyTab})
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
		}
	}()

	// Reader goroutine: call View() concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = m.View()
		}
	}()

	wg.Wait()
}
