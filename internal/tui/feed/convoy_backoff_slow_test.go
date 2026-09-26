package feed

import (
	"testing"
	"time"
)

// The slow half of the poll-interval backoff (gt-1rfb). gt-nzt0 tested the
// error half; the branch the interval was introduced for — a fetch that
// succeeds but outruns the current interval, the shape of a town-wide bd
// famine (gt-05vk) — was left uncovered, so nothing held the two halves apart.
// These tests assert the distinction the code draws: slow is not failed.

// A slow success must back off even though it returned usable data, and unlike
// a failed fetch it must still adopt what it read.
func TestConvoyPollBackoff_DoublesOnSlowFetch(t *testing.T) {
	m := NewModel(nil)
	slow := &ConvoyState{InProgress: []Convoy{{ID: "hq-conv1"}}, LastUpdate: time.Now()}

	m.Update(convoyUpdateMsg{
		state:   slow,
		elapsed: 3 * baseConvoyPollInterval, // kept up with nothing
	})

	m.mu.RLock()
	gotInterval := m.convoyPollInterval
	gotState := m.convoyState
	m.mu.RUnlock()

	if want := baseConvoyPollInterval * 2; gotInterval != want {
		t.Errorf("convoyPollInterval = %v after a slow fetch, want %v (a fetch slower than the interval must back off)", gotInterval, want)
	}
	if gotState != slow {
		t.Errorf("convoyState = %+v, want the slow fetch's state adopted — slow is not failed, only err preserves the last good snapshot", gotState)
	}
}

// Sustained slowness settles at the ceiling. The elapsed value exceeds
// maxConvoyPollInterval, so it counts as slow however far the interval has
// already backed off and the loop cannot accidentally stop tripping the branch
// and reset instead.
func TestConvoyPollBackoff_ClampsAtMaxOnSlowFetches(t *testing.T) {
	m := NewModel(nil)

	for i := 0; i < 8; i++ {
		m.Update(convoyUpdateMsg{
			state:   &ConvoyState{},
			elapsed: maxConvoyPollInterval + time.Minute,
		})
	}

	m.mu.RLock()
	got := m.convoyPollInterval
	m.mu.RUnlock()

	if got != maxConvoyPollInterval {
		t.Errorf("convoyPollInterval = %v after sustained slow fetches, want it clamped at %v", got, maxConvoyPollInterval)
	}
}

// Recovery is judged against the interval currently in force, not the base: a
// fetch that fits inside the backed-off rate proves the contention has eased,
// and only then does the interval return to the base rate.
func TestConvoyPollBackoff_ResetsWhenFetchFitsBackedOffInterval(t *testing.T) {
	m := NewModel(nil)
	m.mu.Lock()
	m.convoyPollInterval = 8 * baseConvoyPollInterval
	m.mu.Unlock()

	m.Update(convoyUpdateMsg{
		state:   &ConvoyState{},
		elapsed: 15 * time.Second, // over the 10s base, well under the 80s in force
	})

	m.mu.RLock()
	got := m.convoyPollInterval
	m.mu.RUnlock()

	if got != baseConvoyPollInterval {
		t.Errorf("convoyPollInterval = %v, want %v: a fetch that fits the current interval means recovery, even when it would have been slow at the base rate", got, baseConvoyPollInterval)
	}
}
