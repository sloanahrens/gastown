package keyrotation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/keystore"
)

const (
	ws   = "ws"
	name = "api-key"
)

// checkerFunc adapts a function to Checker.
type checkerFunc func(ctx context.Context, workspace, name string, rev keystore.Revision) error

func (f checkerFunc) Check(ctx context.Context, workspace, name string, rev keystore.Revision) error {
	return f(ctx, workspace, name, rev)
}

func accept(context.Context, string, string, keystore.Revision) error { return nil }

// counter is a SeatCounter that reports a fixed number of running seats.
type counter struct{ running int }

func (c *counter) Running(context.Context, string, string, keystore.Revision) (int, error) {
	return c.running, nil
}

// failingDisable wraps a store so Disable fails for the revisions in failOn and
// counts the Disable calls it sees.
type failingDisable struct {
	keystore.Store
	failOn   map[keystore.Revision]bool
	disables int
}

func (s *failingDisable) Disable(ctx context.Context, workspace, name string, rev keystore.Revision) error {
	s.disables++
	if s.failOn[rev] {
		return errors.New("store unavailable")
	}
	return s.Store.Disable(ctx, workspace, name, rev)
}

func mustPut(t *testing.T, store keystore.Store, value string) keystore.Revision {
	t.Helper()
	rev, err := store.Put(context.Background(), ws, name, []byte(value))
	if err != nil {
		t.Fatalf("Put(%q): %v", value, err)
	}
	return rev
}

func read(t *testing.T, store keystore.Store) string {
	t.Helper()
	secret, err := store.Get(context.Background(), ws, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return string(secret.Bytes())
}

func revisions(t *testing.T, store keystore.Store) []keystore.RevisionInfo {
	t.Helper()
	infos, err := store.Revisions(context.Background(), ws, name)
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	return infos
}

func exists(t *testing.T, store keystore.Store, rev keystore.Revision) bool {
	t.Helper()
	for _, info := range revisions(t, store) {
		if info.Revision == rev {
			return true
		}
	}
	return false
}

func TestFailedCheckKeepsOldKey(t *testing.T) {
	t.Parallel()
	store := keystore.NewFake()
	mustPut(t, store, "old-value")
	refuse := checkerFunc(func(context.Context, string, string, keystore.Revision) error {
		return errors.New("provider rejected the key brand-new-value")
	})

	out, err := Rotate(context.Background(), store, refuse, ws, name, []byte("brand-new-value"))
	if err != nil {
		t.Fatalf("Rotate: unexpected error %v", err)
	}
	if out.Rotated || out.OwnerNotice == "" {
		t.Errorf("Outcome = %+v, want Rotated false with an OwnerNotice", out)
	}
	if got := read(t, store); got != "old-value" {
		t.Errorf("new seats read %q after a failed check, want the old key", got)
	}
	if strings.Contains(out.OwnerNotice, "brand-new-value") {
		t.Errorf("OwnerNotice carries the key: %q", out.OwnerNotice)
	}
	if !strings.Contains(out.OwnerNotice, "*errors.errorString") {
		t.Errorf("OwnerNotice %q does not name the checker error's type", out.OwnerNotice)
	}
}

func TestPassedCheckDisablesOldForNewSeats(t *testing.T) {
	t.Parallel()
	store := keystore.NewFake()
	old := mustPut(t, store, "old-value")

	out, err := Rotate(context.Background(), store, checkerFunc(accept), ws, name, []byte("new-value"))
	if err != nil || !out.Rotated {
		t.Fatalf("Rotate = %+v, %v; want Rotated true", out, err)
	}
	if got := read(t, store); got != "new-value" {
		t.Errorf("new seats read %q, want the new key", got)
	}
	if !exists(t, store, old) {
		t.Errorf("old revision %d was destroyed at rotation, but seats started on it still hold it", old)
	}
}

func TestRotateDisablesOnlyTheEnabledOlderRevisions(t *testing.T) {
	t.Parallel()
	inner := keystore.NewFake()
	store := &failingDisable{Store: inner}
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := Rotate(context.Background(), store, checkerFunc(accept), ws, name, []byte(v)); err != nil {
			t.Fatalf("Rotate(%s): %v", v, err)
		}
	}
	store.disables = 0

	if _, err := Rotate(context.Background(), store, checkerFunc(accept), ws, name, []byte("v4")); err != nil {
		t.Fatalf("Rotate(v4): %v", err)
	}
	if store.disables != 1 {
		t.Errorf("a rotation after three earlier ones made %d Disable calls, want 1 (only the one enabled older revision)", store.disables)
	}
}

func TestRetirementFailureAfterAPassedCheckReportsTheLiveKey(t *testing.T) {
	t.Parallel()
	inner := keystore.NewFake()
	old := mustPut(t, inner, "old-value")
	store := &failingDisable{Store: inner, failOn: map[keystore.Revision]bool{old: true}}

	out, err := Rotate(context.Background(), store, checkerFunc(accept), ws, name, []byte("new-value"))
	if err == nil {
		t.Fatal("Rotate returned no error though the old revision stayed enabled")
	}
	if !out.Rotated || out.Revision == 0 {
		t.Errorf("Outcome = %+v, want Rotated true with the live revision, so the console reports the state", out)
	}
}

func TestFailedRollbackSaysTheUncheckedRevisionMayBeReadable(t *testing.T) {
	t.Parallel()
	inner := keystore.NewFake()
	mustPut(t, inner, "old-value")
	store := &failingDisable{Store: inner, failOn: map[keystore.Revision]bool{2: true}}
	refuse := checkerFunc(func(context.Context, string, string, keystore.Revision) error { return errors.New("no") })

	out, err := Rotate(context.Background(), store, refuse, ws, name, []byte("new-value"))
	if err == nil || out.Rotated || out.Revision != 2 {
		t.Errorf("Rotate = %+v, %v; want Rotated false, revision 2 and an error", out, err)
	}
}

func TestOldRevisionDestroyedOnlyWhenLastSeatEnds(t *testing.T) {
	t.Parallel()
	store := keystore.NewFake()
	old := mustPut(t, store, "old-value")
	if _, err := Rotate(context.Background(), store, checkerFunc(accept), ws, name, []byte("new-value")); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	seats := &counter{running: 1}

	err := SeatEnded(context.Background(), store, seats, ws, name, old)
	if !errors.Is(err, ErrSeatsRunning) || !exists(t, store, old) {
		t.Fatalf("SeatEnded with one seat running = %v, revision present %v; want ErrSeatsRunning and the revision kept", err, exists(t, store, old))
	}
	seats.running = 0
	if err := SeatEnded(context.Background(), store, seats, ws, name, old); err != nil || exists(t, store, old) {
		t.Fatalf("SeatEnded with no seat running = %v, revision present %v; want nil and the revision destroyed", err, exists(t, store, old))
	}
	if err := SeatEnded(context.Background(), store, seats, ws, name, old); err != nil {
		t.Errorf("a second SeatEnded for a destroyed revision = %v, want nil", err)
	}
}

func TestSeatEndedRefusesTheLiveRevision(t *testing.T) {
	t.Parallel()
	store := keystore.NewFake()
	live := mustPut(t, store, "live-value")

	err := SeatEnded(context.Background(), store, &counter{}, ws, name, live)
	if !errors.Is(err, ErrRevisionLive) {
		t.Errorf("SeatEnded on the live revision = %v, want ErrRevisionLive", err)
	}
	if got := read(t, store); got != "live-value" {
		t.Errorf("the live key reads %q after the refused SeatEnded", got)
	}
}

// The seat-end process shares nothing with the rotating process: it gets only
// the store and a counter. The retired state is the store's, so it still works.
func TestSeatEndedWorksFromAProcessThatNeverRotated(t *testing.T) {
	t.Parallel()
	store := keystore.NewFake()
	old := mustPut(t, store, "old-value")
	func() { // the rotating process; everything local to it goes out of scope here
		if _, err := Rotate(context.Background(), store, checkerFunc(accept), ws, name, []byte("new-value")); err != nil {
			t.Fatalf("Rotate: %v", err)
		}
	}()

	if err := SeatEnded(context.Background(), store, &counter{}, ws, name, old); err != nil || exists(t, store, old) {
		t.Errorf("SeatEnded from a fresh caller = %v, revision present %v; want the old revision destroyed", err, exists(t, store, old))
	}
}
