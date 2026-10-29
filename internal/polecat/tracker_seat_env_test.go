package polecat

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/seattoken"
)

// These tests call addTrackerSeatEnv with an explicit town directory: the
// function reads the town's files only (route table, town.json, the seat token
// file) and writes the variables into the map it is handed, so they set no
// environment, point no process-wide routing at a town and run in parallel. The
// one launch-level assertion, that launch calls it before the session is made,
// is in TestLaunchHostSessionEnvKeepsHostTownRoot.

const trackerSeatToken = "trk_seat-token-value"

// fakeSeatIssuer records the actors it was asked for and answers with token
// and err together, so an issuer that fails after minting cannot have its
// token written or printed.
type fakeSeatIssuer struct {
	token  string
	err    error
	actors []string
}

func (f *fakeSeatIssuer) Issue(_ context.Context, actor string) (string, error) {
	f.actors = append(f.actors, actor)
	return f.token, f.err
}

// waitingSeatIssuer answers only once its context ends, so a test can show
// the launch bounds the issue it waits on.
type waitingSeatIssuer struct{}

// It also carries a bound of its own, so a launch that stops bounding the issue
// fails this test with a clear error instead of hanging it.
func (*waitingSeatIssuer) Issue(ctx context.Context, _ string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, waitingSeatIssuerBackstop)
	defer cancel()
	<-bounded.Done()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("the issuer was never given a bounded context: %w", bounded.Err())
}

// waitingSeatIssuerBackstop is how long waitingSeatIssuer waits for a context
// that never ends before it fails the test's launch.
const waitingSeatIssuerBackstop = 30 * time.Second

// trackerSeatManager is a session manager for the rig gastown that mints seat
// tokens through issuer.
func trackerSeatManager(issuer seattoken.Issuer) *SessionManager {
	return &SessionManager{rig: &rig.Rig{Name: "gastown"}, seatTokenIssuer: issuer}
}

// rigRoute is one route line of the test town's table.
func rigRoute(path, prefix, backend string) string {
	return fmt.Sprintf(`{"path":%q,"prefix":%q,"backend":%q}`, path, prefix, backend)
}

// writeSeatRoutes gives the town the route lines it is passed, one per line.
func writeSeatRoutes(t *testing.T, town string, lines ...string) {
	t.Helper()
	dir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, beads.RoutesFileName), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRigRoute gives the town one route for the rig gastown, on backend.
func writeRigRoute(t *testing.T, town, backend string) {
	t.Helper()
	writeSeatRoutes(t, town, rigRoute("gastown/mayor/rig", "gt-", backend))
}

// trackerSeatTownDir is a temp town whose route table puts gastown on backend.
func trackerSeatTownDir(t *testing.T, backend string) string {
	t.Helper()
	town := t.TempDir()
	writeRigRoute(t, town, backend)
	return town
}

// A seat whose rig is on the tracker carries the endpoint and the path to its
// token file, and the token itself reaches neither the environment nor argv.
// A seat whose rig is on Dolt is handed no tracker name and no token: its
// environment is byte for byte what it was.
func TestAddTrackerSeatEnv(t *testing.T) {
	t.Parallel()
	issuer := &fakeSeatIssuer{token: trackerSeatToken}
	m := trackerSeatManager(issuer)
	before := map[string]string{"GT_ROLE": "polecat", "BD_ACTOR": "gastown/polecats/box"}

	dolt := maps.Clone(before)
	if err := m.addTrackerSeatEnv(dolt, trackerSeatTownDir(t, beads.BackendDolt), "box"); err != nil {
		t.Fatalf("a Dolt rig: %v", err)
	}
	if !maps.Equal(dolt, before) {
		t.Errorf("a Dolt rig changed the seat environment: %v, want %v", dolt, before)
	}
	if len(issuer.actors) != 0 {
		t.Errorf("a Dolt rig asked for a token for %v", issuer.actors)
	}

	town := trackerSeatTownDir(t, beads.BackendTracker)
	tracker := maps.Clone(before)
	if err := m.addTrackerSeatEnv(tracker, town, "box"); err != nil {
		t.Fatalf("a tracker rig: %v", err)
	}
	if got, want := tracker[envTrackerURL], "http://127.0.0.1:7433"; got != want {
		t.Errorf("TRACKER_URL = %q, want %q (the default the town's config resolves to)", got, want)
	}
	wantPath := filepath.Join(town, ".runtime", "seats", "gastown", "polecats.box.token")
	if got := tracker[envTrackerTokenFile]; got != wantPath {
		t.Errorf("TRACKER_TOKEN_FILE = %q, want %q", got, wantPath)
	}
	if got := stringOfFile(t, wantPath); got != trackerSeatToken {
		t.Errorf("token file holds %q, want %q", got, trackerSeatToken)
	}
	for name, value := range tracker {
		if strings.Contains(value, trackerSeatToken) {
			t.Errorf("the seat environment's %s carries the token itself", name)
		}
	}
	if len(tracker) != len(before)+2 {
		t.Errorf("tracker env has %d names, want %d: the %d it was handed plus the two tracker names",
			len(tracker), len(before)+2, len(before))
	}
	for name, value := range before {
		if got := tracker[name]; got != value {
			t.Errorf("a tracker rig changed %s: %q, want %q", name, got, value)
		}
	}
	if len(issuer.actors) != 1 || issuer.actors[0] != "gastown/polecats/box" {
		t.Errorf("issuer asked for %v, want one token for gastown/polecats/box", issuer.actors)
	}
}

// A restart reuses the seat's token: the issuer is asked once across two
// launches, and both sessions are given the same file.
func TestAddTrackerSeatEnvReusesTheTokenOnRestart(t *testing.T) {
	t.Parallel()
	town := trackerSeatTownDir(t, beads.BackendTracker)
	issuer := &fakeSeatIssuer{token: trackerSeatToken}
	m := trackerSeatManager(issuer)

	var paths []string
	for i := range 2 {
		env := map[string]string{}
		if err := m.addTrackerSeatEnv(env, town, "box"); err != nil {
			t.Fatalf("launch %d: %v", i+1, err)
		}
		paths = append(paths, env[envTrackerTokenFile])
	}
	if len(issuer.actors) != 1 || issuer.actors[0] != "gastown/polecats/box" {
		t.Fatalf("issuer asked for %v, want one token for gastown/polecats/box", issuer.actors)
	}
	if paths[0] == "" || paths[0] != paths[1] {
		t.Errorf("the two launches named the token files %v, want one reused file", paths)
	}
}

// A seat that cannot be given a token is an error that names the rig and seat
// and never the token, and it leaves the environment it was handed untouched:
// a launch that failed here must not have started a session with half the
// tracker names.
func TestAddTrackerSeatEnvRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		issuer  seattoken.Issuer
		host    func(root string) (seattoken.Issuer, error)
		prepare func(t *testing.T, town string)
		want    string
	}{
		{
			name: "no host credential to build an issuer",
			host: func(string) (seattoken.Issuer, error) {
				return nil, errors.New("TRACKER_ADMIN_TOKEN is not set in the town's daemon env")
			},
			want: "TRACKER_ADMIN_TOKEN",
		},
		{
			name:   "an issuer that fails after minting",
			issuer: &fakeSeatIssuer{token: trackerSeatToken, err: errors.New("the tracker is down")},
			want:   "the tracker is down",
		},
		{
			name:   "a route table that cannot be read",
			issuer: &fakeSeatIssuer{token: trackerSeatToken},
			prepare: func(t *testing.T, town string) {
				t.Helper()
				// A directory where the route table should be: opening it
				// succeeds and reading it fails, as an unreadable file does.
				path := filepath.Join(town, ".beads", beads.RoutesFileName)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "reading the route table",
		},
		{
			name:   "routes that name the rig and disagree",
			issuer: &fakeSeatIssuer{token: trackerSeatToken},
			prepare: func(t *testing.T, town string) {
				t.Helper()
				writeSeatRoutes(t, town,
					rigRoute("gastown/mayor/rig", "gt-", beads.BackendDolt),
					rigRoute("gastown/polecats/box", "gt-box-", beads.BackendTracker))
			},
			want: "name both",
		},
		{
			name:   "a backend the route table does not define",
			issuer: &fakeSeatIssuer{token: trackerSeatToken},
			prepare: func(t *testing.T, town string) {
				t.Helper()
				writeRigRoute(t, town, "postgres")
			},
			want: "which is neither",
		},
		{
			name:   "a town config that does not resolve the endpoint",
			issuer: &fakeSeatIssuer{token: trackerSeatToken},
			prepare: func(t *testing.T, town string) {
				t.Helper()
				dir := filepath.Join(town, "mayor")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "town.json"), []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "town.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := trackerSeatTownDir(t, beads.BackendTracker)
			if tc.prepare != nil {
				tc.prepare(t, town)
			}
			m := trackerSeatManager(tc.issuer)
			m.hostSeatIssuer = tc.host
			env := map[string]string{}

			err := m.addTrackerSeatEnv(env, town, "box")
			if err == nil {
				t.Fatal("addTrackerSeatEnv succeeded; a seat with no tracker token must not start")
			}
			if !strings.Contains(err.Error(), "gastown/polecats/box") {
				t.Errorf("error = %v, want it to name the seat gastown/polecats/box", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if strings.Contains(err.Error(), trackerSeatToken) {
				t.Errorf("error carries the token: %v", err)
			}
			if len(env) != 0 {
				t.Errorf("a refused seat was handed environment %v", env)
			}
		})
	}
}

// A restart finds the seat's token file and needs no admin credential: the host
// issuer is built only when a token is issued, so a town whose credential is
// gone (rotated, or the daemon env unreadable) still restarts a seat that has
// its token.
func TestAddTrackerSeatEnvRestartNeedsNoAdminCredential(t *testing.T) {
	t.Parallel()
	town := trackerSeatTownDir(t, beads.BackendTracker)
	first := map[string]string{}
	if err := trackerSeatManager(&fakeSeatIssuer{token: trackerSeatToken}).addTrackerSeatEnv(first, town, "box"); err != nil {
		t.Fatalf("first launch: %v", err)
	}

	restart := trackerSeatManager(nil)
	built := 0
	restart.hostSeatIssuer = func(string) (seattoken.Issuer, error) {
		built++
		return nil, errors.New("TRACKER_ADMIN_TOKEN is not set in the town's daemon env")
	}
	again := map[string]string{}
	if err := restart.addTrackerSeatEnv(again, town, "box"); err != nil {
		t.Fatalf("a restart with the token file in place failed: %v", err)
	}
	if built != 0 {
		t.Errorf("the host issuer was built %d time(s) for a seat that already had its token", built)
	}
	if again[envTrackerTokenFile] == "" || again[envTrackerTokenFile] != first[envTrackerTokenFile] {
		t.Errorf("restart named the token file %q, want the first launch's %q", again[envTrackerTokenFile], first[envTrackerTokenFile])
	}
}

// A seat name that is not one safe path segment is refused before the issuer is
// asked or a file is written: the name becomes part of a file name under the
// town's runtime directory.
func TestAddTrackerSeatEnvRefusesAnUnsafeSeatName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../escape", "a/b", ".hidden", "has space", ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			town := trackerSeatTownDir(t, beads.BackendTracker)
			issuer := &fakeSeatIssuer{token: trackerSeatToken}
			env := map[string]string{}

			err := trackerSeatManager(issuer).addTrackerSeatEnv(env, town, name)
			if err == nil {
				t.Fatalf("addTrackerSeatEnv(%q) succeeded", name)
			}
			if len(issuer.actors) != 0 || len(env) != 0 {
				t.Errorf("a refused seat name reached the issuer (%v) or the environment (%v)", issuer.actors, env)
			}
			if _, statErr := os.Stat(filepath.Join(town, ".runtime")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused seat name wrote under the town's runtime directory (stat: %v)", statErr)
			}
		})
	}
}

// A token issue that never answers fails at the launch's own bound: the issuer
// is handed a bounded context, so a tracker that accepted the connection and
// then stalled cannot hold the launch open. The bound is one nanosecond, so
// the context is expired when the issuer first reads it and the test waits on
// nothing.
func TestAddTrackerSeatEnvBoundsTheIssue(t *testing.T) {
	t.Parallel()
	town := trackerSeatTownDir(t, beads.BackendTracker)
	m := trackerSeatManager(&waitingSeatIssuer{})
	m.seatTokenTimeout = time.Nanosecond
	env := map[string]string{}

	err := m.addTrackerSeatEnv(env, town, "box")
	if err == nil {
		t.Fatal("addTrackerSeatEnv succeeded; a token issue that never answers must not")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the token issue's bound to have expired", err)
	}
	if !strings.Contains(err.Error(), "gastown/polecats/box") {
		t.Errorf("error = %v, want it to name the seat gastown/polecats/box", err)
	}
	if len(env) != 0 {
		t.Errorf("a seat whose token issue timed out was handed environment %v", env)
	}
}

// stringOfFile returns the whitespace-trimmed contents of the file at path.
func stringOfFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}
