package feed

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// installFailingBd writes a fake `bd` that fails every invocation, the way bd
// behaves when Dolt is contended or refusing connections: it exits non-zero
// almost immediately, with no output.
func installFailingBd(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd script uses /bin/sh, not available on Windows")
	}
	installFakeBdScript(t, "#!/bin/sh\necho 'bd: connection refused' >&2\nexit 1\n")
}

// installEmptyBd writes a fake `bd` that succeeds with an empty result set.
func installEmptyBd(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd script uses /bin/sh, not available on Windows")
	}
	installFakeBdScript(t, "#!/bin/sh\nprintf '[]'\nexit 0\n")
}

func installFakeBdScript(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newTempTownRoot returns a town root with the .beads dir bd runs in.
func newTempTownRoot(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	return townRoot
}

// TestFetchConvoys_SurfacesFetchFailure is the regression test for gt-nzt0.
// FetchConvoys is declared (state, error), but it used to swallow the listing
// failure and return a nil error with an empty state — so the feed read a
// failing bd as a healthy fetch that happened to return nothing, and the
// poll-interval backoff (which keys on that error) never fired.
func TestFetchConvoys_SurfacesFetchFailure(t *testing.T) {
	installFailingBd(t)

	state, err := FetchConvoys(newTempTownRoot(t))
	if err == nil {
		t.Fatal("FetchConvoys() error = nil, want the bd listing failure surfaced — the poll backoff keys on it")
	}
	if state == nil {
		t.Fatal("FetchConvoys() state = nil, want partial state returned alongside the error")
	}
}

// TestFetchConvoys_CleanFetchIsNoError is the mirror image: an empty town is
// not a failure, so a successful fetch must not report an error. Without this,
// "surface the error" could be satisfied by always erroring, which would pin
// the poll interval at its maximum forever.
func TestFetchConvoys_CleanFetchIsNoError(t *testing.T) {
	installEmptyBd(t)

	state, err := FetchConvoys(newTempTownRoot(t))
	if err != nil {
		t.Fatalf("FetchConvoys() error = %v, want nil for a successful fetch of an empty town", err)
	}
	if state == nil {
		t.Fatal("FetchConvoys() state = nil, want non-nil state for a successful fetch")
	}
}

// TestConvoyPollBackoff_DoublesOnFetchError covers the model half of gt-nzt0:
// a failed fetch must double the poll interval even though it failed fast, and
// must not overwrite the last good snapshot with the empty state it returns.
func TestConvoyPollBackoff_DoublesOnFetchError(t *testing.T) {
	m := NewModel(nil)
	lastGood := &ConvoyState{InProgress: []Convoy{{ID: "hq-conv1"}}, LastUpdate: time.Now()}
	m.mu.Lock()
	m.convoyState = lastGood
	m.mu.Unlock()

	m.Update(convoyUpdateMsg{
		state:   &ConvoyState{},   // a failed fetch comes back empty...
		elapsed: time.Millisecond, // ...and fails fast, so elapsed is no signal at all
		err:     errors.New("bd: connection refused"),
	})

	m.mu.RLock()
	gotInterval := m.convoyPollInterval
	gotState := m.convoyState
	m.mu.RUnlock()

	if want := baseConvoyPollInterval * 2; gotInterval != want {
		t.Errorf("convoyPollInterval = %v after a failed fetch, want %v (backoff must fire on error)", gotInterval, want)
	}
	if gotState != lastGood {
		t.Errorf("convoyState = %+v, want the last good snapshot preserved, not the failed fetch's empty state", gotState)
	}
}

// TestConvoyPollBackoff_ClampsAtMax verifies repeated failures settle at the
// ceiling instead of growing without bound.
func TestConvoyPollBackoff_ClampsAtMax(t *testing.T) {
	m := NewModel(nil)
	m.mu.Lock()
	m.convoyPollInterval = maxConvoyPollInterval
	m.mu.Unlock()

	for i := 0; i < 3; i++ {
		m.Update(convoyUpdateMsg{
			state:   &ConvoyState{},
			elapsed: time.Millisecond,
			err:     errors.New("bd: connection refused"),
		})
	}

	m.mu.RLock()
	got := m.convoyPollInterval
	m.mu.RUnlock()

	if got != maxConvoyPollInterval {
		t.Errorf("convoyPollInterval = %v after repeated failures, want it clamped at %v", got, maxConvoyPollInterval)
	}
}

// TestConvoyPollBackoff_ResetsOnCleanFastFetch verifies recovery: once a fetch
// succeeds quickly again, the interval returns to the base rate and the fresh
// state is adopted.
func TestConvoyPollBackoff_ResetsOnCleanFastFetch(t *testing.T) {
	m := NewModel(nil)
	m.mu.Lock()
	m.convoyPollInterval = maxConvoyPollInterval
	m.mu.Unlock()

	fresh := &ConvoyState{InProgress: []Convoy{{ID: "hq-conv1"}}, LastUpdate: time.Now()}
	m.Update(convoyUpdateMsg{state: fresh, elapsed: 50 * time.Millisecond})

	m.mu.RLock()
	gotInterval := m.convoyPollInterval
	gotState := m.convoyState
	m.mu.RUnlock()

	if gotInterval != baseConvoyPollInterval {
		t.Errorf("convoyPollInterval = %v after a clean fast fetch, want %v", gotInterval, baseConvoyPollInterval)
	}
	if gotState != fresh {
		t.Errorf("convoyState = %+v, want the freshly fetched state adopted", gotState)
	}
}
