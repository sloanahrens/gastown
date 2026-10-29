package seattoken

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/tracker/client"
)

// fakeRevoker records the actors it was asked to revoke, and fails as set.
type fakeRevoker struct {
	err    error
	actors []string
}

func (f *fakeRevoker) Revoke(_ context.Context, actor string) error {
	f.actors = append(f.actors, actor)
	return f.err
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
	seat := testSeat()
	path, err := store.Ensure(context.Background(), seat, &fakeIssuer{token: seatToken})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// A directory that holds a file cannot be removed by os.Remove, so the
	// token path is no longer a file the delete can take away.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	revoker := &fakeRevoker{}

	err = store.Revoke(context.Background(), seat, revoker)
	if err == nil {
		t.Fatal("Revoke = nil, want the token file it could not remove")
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

// failingRoundTrip answers every request with status, off the network.
type failingRoundTrip int

func (f failingRoundTrip) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: int(f),
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}, nil
}

// The tracker's refusal of a revoke is reported with the tracker's answer, so
// the warning a removal prints says why the token may still be live.
func TestTrackerRevokerReportsARefusal(t *testing.T) {
	t.Parallel()
	tc, err := client.New(client.Config{
		BaseURL: "http://127.0.0.1:7433",
		Token:   "admin-token-value",
		HTTP:    &http.Client{Transport: failingRoundTrip(http.StatusInternalServerError)},
	})
	if err != nil {
		t.Fatalf("tracker client: %v", err)
	}

	err = NewTrackerRevoker(tc).Revoke(context.Background(), "gastown/crew/max")
	if err == nil || !strings.Contains(err.Error(), "tracker answered 500") {
		t.Fatalf("Revoke = %v, want the tracker's 500 reported", err)
	}
}

// A tracker rig whose town has no admin credential fails the revoke, naming
// the seat rather than reading as a seat with nothing to revoke.
func TestRevokeSeatReportsAMissingAdminCredential(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRouteTable(t, town, trackerRoute)

	err := RevokeSeat(context.Background(), town, testSeat(), nil)
	if err == nil || !strings.Contains(err.Error(), "gastown/polecats/cheedo") {
		t.Fatalf("RevokeSeat = %v, want a failure naming the seat", err)
	}
}

// A revoke through a nil revoker is refused, and the token file is left for
// the caller to retry.
func TestStoreRevokeNeedsARevoker(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	path, err := store.Ensure(context.Background(), testSeat(), &fakeIssuer{token: seatToken})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := store.Revoke(context.Background(), testSeat(), nil); err == nil {
		t.Fatal("Revoke with no revoker = nil, want a refusal")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("token file stat = %v, want it kept for the retry", err)
	}
}

// A seat that names a file outside the runtime directory is refused before
// anything is revoked.
func TestStoreRevokeRefusesASeatOutsideTheRuntime(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	seat := intent.Seat{Rig: "../etc", Role: "polecats", Name: "cheedo"}
	revoker := &fakeRevoker{}

	if err := store.Revoke(context.Background(), seat, revoker); err == nil {
		t.Fatal("Revoke accepted a seat that climbs out of the runtime directory")
	}
	if len(revoker.actors) != 0 {
		t.Errorf("revoked %v, want nothing for a refused seat", revoker.actors)
	}
}
