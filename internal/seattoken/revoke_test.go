package seattoken

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/intent"
)

// fakeRevoker records the actors it was asked to revoke, and fails as set.
type fakeRevoker struct {
	err    error
	actors []string
}

func (f *fakeRevoker) Revoke(_ context.Context, actor string) (int, error) {
	f.actors = append(f.actors, actor)
	if f.err != nil {
		return 0, f.err
	}
	return 1, nil
}

// writeRouteTable writes the town's route table holding one line, which the
// cases spell as the route they need.
func writeRouteTable(t *testing.T, townRoot, line string) {
	t.Helper()
	dir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "routes.jsonl"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// trackerRoute is a route line putting gastown on the town tracker.
const trackerRoute = `{"prefix":"gt-","path":"gastown/mayor/rig","backend":"tracker"}`

// doltRoute is a route line putting gastown on Dolt.
const doltRoute = `{"prefix":"gt-","path":"gastown/mayor/rig"}`

// A seat on a tracker-backed rig loses its token file and its actor's tokens.
func TestRevokeSeatRevokesATrackerSeat(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	store := New(town)
	path, err := store.Ensure(context.Background(), testSeat(), &fakeIssuer{token: seatToken})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	writeRouteTable(t, town, trackerRoute)
	revoker := &fakeRevoker{}

	if err := RevokeSeat(context.Background(), town, testSeat(), revoker); err != nil {
		t.Fatalf("RevokeSeat: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file stat = %v, want it gone", err)
	}
	if want := []string{"gastown/polecats/cheedo"}; !slices.Equal(revoker.actors, want) {
		t.Errorf("revoked %v, want %v", revoker.actors, want)
	}
}

// A seat on a Dolt rig has no tracker token: nothing is revoked, and the
// revoker is not even needed.
func TestRevokeSeatLeavesADoltRigAlone(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		routes string // "" writes no route table
	}{
		{"the route names Dolt", doltRoute},
		{"the table names no rig", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tt.routes != "" {
				writeRouteTable(t, town, tt.routes)
			}

			if err := RevokeSeat(context.Background(), town, testSeat(), nil); err != nil {
				t.Fatalf("RevokeSeat = %v, want nil", err)
			}
		})
	}
}

// A route table that cannot be read is an error, never "this rig has no
// tracker token": the token would stay live.
func TestRevokeSeatRefusesAnUnreadableRouteTable(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, ".beads", "routes.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RevokeSeat(context.Background(), town, testSeat(), &fakeRevoker{}); err == nil {
		t.Fatal("RevokeSeat read an unreadable route table as a rig with nothing to revoke")
	}
}

// A second revoke of the same seat ends nothing and is still a success.
func TestStoreRevokeIsIdempotent(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	revoker := &fakeRevoker{}
	if _, err := store.Ensure(context.Background(), testSeat(), &fakeIssuer{token: seatToken}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for i := range 2 {
		if err := store.Revoke(context.Background(), testSeat(), revoker); err != nil {
			t.Fatalf("Revoke %d: %v", i+1, err)
		}
	}
	if want := []string{"gastown/polecats/cheedo", "gastown/polecats/cheedo"}; !slices.Equal(revoker.actors, want) {
		t.Errorf("revoked %v, want %v", revoker.actors, want)
	}
}

// A token file that cannot be removed still ends the actor's tokens, and the
// file's failure is reported with them.
func TestStoreRevokeRevokesWhenTheFileCannotBeRemoved(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	seat := intent.Seat{Rig: strings.Repeat("r", 300), Role: "polecats", Name: "cheedo"}
	if err := os.MkdirAll(store.seatDirs(seat)[1], 0o700); err != nil {
		t.Fatal(err)
	}
	revoker := &fakeRevoker{}

	err := store.Revoke(context.Background(), seat, revoker)
	if err == nil {
		t.Fatal("Revoke = nil, want the token file it could not stat")
	}
	if want := []string{seat.String()}; !slices.Equal(revoker.actors, want) {
		t.Errorf("revoked %v, want %v", revoker.actors, want)
	}
}

// A revoker failure is reported, and the token file is gone all the same: a
// seat recreated under the same name must not be handed the token whose revoke
// failed.
func TestStoreRevokeRemovesTheFileWhenTheRevokeFails(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path, err := store.Ensure(context.Background(), testSeat(), &fakeIssuer{token: seatToken})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	revokeErr := errors.New("the tracker is down")

	err = store.Revoke(context.Background(), testSeat(), &fakeRevoker{err: revokeErr})
	if !errors.Is(err, revokeErr) {
		t.Fatalf("Revoke = %v, want it to wrap %q", err, revokeErr)
	}
	if !strings.Contains(err.Error(), "gastown/polecats/cheedo") {
		t.Errorf("Revoke error = %q, want it to name the seat", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("token file stat = %v, want it gone", statErr)
	}
}
