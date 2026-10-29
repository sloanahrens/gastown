package crew

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/rig"
)

// fakeSeatIssuer answers a crew seat's token request with a fixed token and
// error, and records the actors it was asked for.
type fakeSeatIssuer struct {
	token   string
	err     error
	actors  []string
	bounded bool
}

// Issue records the actor and whether the request's context has a deadline.
func (f *fakeSeatIssuer) Issue(ctx context.Context, actor string) (string, error) {
	f.actors = append(f.actors, actor)
	_, f.bounded = ctx.Deadline()
	return f.token, f.err
}

const (
	trackerRouteGastown = `{"path":"gastown/mayor/rig","prefix":"gt-","backend":"tracker"}`
	doltRouteGastown    = `{"path":"gastown/mayor/rig","prefix":"gt-"}`
	trackerRouteOther   = `{"path":"other/mayor/rig","prefix":"ot-","backend":"tracker"}`
	doltRouteOther      = `{"path":"other/mayor/rig","prefix":"ot-"}`
	// routesIsDirectory stands in for a route table that exists but cannot be
	// read: a directory where routes.jsonl should be.
	routesIsDirectory = "\x00directory"
)

// writeCrewTown lays out a town with the given route table and town.json. An
// empty routes string writes no route table; routesIsDirectory makes it a
// directory; an empty townJSON writes no town.json.
func writeCrewTown(t *testing.T, routes, townJSON string) string {
	t.Helper()
	root := t.TempDir()
	beadsDir := filepath.Join(root, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	switch {
	case routes == routesIsDirectory:
		if err := os.MkdirAll(routesPath, 0o755); err != nil {
			t.Fatal(err)
		}
	case routes != "":
		if err := os.WriteFile(routesPath, []byte(routes+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if townJSON != "" {
		if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(townJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// crewManagerFor returns a manager for the gastown rig that mints tokens with
// issuer, so no admin credential is read.
func crewManagerFor(issuer *fakeSeatIssuer) *Manager {
	return &Manager{rig: &rig.Rig{Name: "gastown"}, trackerIssuer: issuer}
}

func TestTrackerEnvByRouteBackend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		routes      string
		townJSON    string
		wantTracker bool
		wantErr     bool
		wantMinted  int
	}{
		{"tracker rig gets the URL and its token file", trackerRouteGastown, "", true, false, 1},
		{"Dolt rig environment is unchanged", doltRouteGastown, "", false, false, 0},
		{"town with no route table is on Dolt", "", "", false, false, 0},
		{"rig with no route in a Dolt-only town is on Dolt", doltRouteOther, "", false, false, 0},
		{"rig with no route in a town with tracker rigs refuses", trackerRouteOther, "", false, true, 0},
		{"unreadable route table refuses", routesIsDirectory, "", false, true, 0},
		{"broken town config refuses before minting a token", trackerRouteGastown, "{", false, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeCrewTown(t, tc.routes, tc.townJSON)
			issuer := &fakeSeatIssuer{token: "trk_seat-token-0001"}
			env, err := crewManagerFor(issuer).trackerEnv(root, "dave")

			if (err != nil) != tc.wantErr {
				t.Fatalf("trackerEnv error = %v, want error %v", err, tc.wantErr)
			}
			if len(issuer.actors) != tc.wantMinted {
				t.Errorf("tokens minted = %d, want %d", len(issuer.actors), tc.wantMinted)
			}
			if !tc.wantTracker {
				if len(env) != 0 {
					t.Errorf("environment = %v, want no tracker variables", env)
				}
				return
			}
			if got := env["TRACKER_URL"]; got != "http://127.0.0.1:7433" {
				t.Errorf("TRACKER_URL = %q, want the default tracker endpoint", got)
			}
			path := env["TRACKER_TOKEN_FILE"]
			if !strings.HasPrefix(path, root) {
				t.Fatalf("TRACKER_TOKEN_FILE = %q, want a file under the town", path)
			}
			held, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading the token file: %v", err)
			}
			if got := strings.TrimSpace(string(held)); got != issuer.token {
				t.Errorf("token file holds %q, want the issued token", got)
			}
			if len(issuer.actors) != 1 || issuer.actors[0] != "gastown/crew/dave" {
				t.Errorf("issuer asked for %v, want the crew seat gastown/crew/dave", issuer.actors)
			}
			if !issuer.bounded {
				t.Error("token request has no deadline, want a bounded context")
			}
		})
	}
}

// A crew name becomes a file name under the town's seat directory. A name that
// is not a crew name refuses before any token is minted or any directory made,
// even when the caller did not check it.
func TestTrackerEnvRefusesUnsafeCrewName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "dave.x", "dave-x", "dave x"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := writeCrewTown(t, trackerRouteGastown, "")
			issuer := &fakeSeatIssuer{token: "trk_seat-token-0003"}
			env, err := crewManagerFor(issuer).trackerEnv(root, name)
			if err == nil {
				t.Fatalf("trackerEnv(%q) = %v, want a refusal", name, env)
			}
			if env != nil {
				t.Errorf("environment = %v after a refused name, want none", env)
			}
			if len(issuer.actors) != 0 {
				t.Errorf("tokens minted = %d, want none", len(issuer.actors))
			}
			if _, err := os.Stat(filepath.Join(root, ".runtime")); !os.IsNotExist(err) {
				t.Errorf("runtime directory exists after a refused name (stat error %v), want none", err)
			}
		})
	}
}

// A second start of the same crew member keeps the token file it was given
// and mints nothing new.
func TestTrackerEnvSecondStartReusesTokenFile(t *testing.T) {
	t.Parallel()
	root := writeCrewTown(t, trackerRouteGastown, "")
	issuer := &fakeSeatIssuer{token: "trk_seat-token-0002"}
	m := crewManagerFor(issuer)

	first, err := m.trackerEnv(root, "dave")
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := m.trackerEnv(root, "dave")
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if len(issuer.actors) != 1 {
		t.Errorf("tokens minted across two starts = %d, want 1", len(issuer.actors))
	}
	if first["TRACKER_TOKEN_FILE"] != second["TRACKER_TOKEN_FILE"] {
		t.Errorf("token file changed between starts: %q then %q", first["TRACKER_TOKEN_FILE"], second["TRACKER_TOKEN_FILE"])
	}
}

// An issuer failure fails the start with an error that names the cause and
// never carries the token, even when the issuer returns one beside its error.
func TestTrackerEnvIssuerFailureHoldsNoToken(t *testing.T) {
	t.Parallel()
	root := writeCrewTown(t, trackerRouteGastown, "")
	const leaked = "trk_must-not-appear-in-errors"
	issuer := &fakeSeatIssuer{token: leaked, err: errors.New("tracker: 503 unavailable")}

	env, err := crewManagerFor(issuer).trackerEnv(root, "dave")
	if err == nil {
		t.Fatalf("trackerEnv = %v, want the issuer failure", env)
	}
	if env != nil {
		t.Errorf("environment = %v after a failed issue, want none", env)
	}
	if strings.Contains(err.Error(), leaked) {
		t.Errorf("error %q carries the token", err)
	}
	if !strings.Contains(err.Error(), "tracker: 503 unavailable") {
		t.Errorf("error %q does not name the cause", err)
	}
}

// Start on a tracker-backed rig whose token the tracker refuses fails before
// any session is touched, and its error carries the cause and no token. The
// refusal happens before tmux is reached, so the test runs no session.
func TestStartFailsWhenTrackerRefusesToken(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	root := writeCrewTown(t, trackerRouteGastown, "")
	rigPath := filepath.Join(root, "gastown")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "bare-repo.git")
	f.InitBare(t, bare)
	r := &rig.Rig{Name: "gastown", Path: rigPath, GitURL: bare}
	const leaked = "trk_must-not-appear-in-errors"
	issuer := &fakeSeatIssuer{token: leaked, err: errors.New("tracker: 503 unavailable")}
	m := fakeManager(t, f, r)
	m.trackerIssuer = issuer

	err := m.Start("dave", StartOptions{ClaudeConfigDir: t.TempDir()})
	if err == nil {
		t.Fatal("Start = nil, want the tracker refusal")
	}
	if strings.Contains(err.Error(), leaked) {
		t.Errorf("Start error %q carries the token", err)
	}
	if !strings.Contains(err.Error(), "tracker: 503 unavailable") {
		t.Errorf("Start error %q does not name the cause", err)
	}
	if len(issuer.actors) != 1 {
		t.Errorf("tokens requested = %d, want 1", len(issuer.actors))
	}
}

// A tracker-backed rig with no injected issuer and no admin credential in its
// town refuses before it mints a token: no token file is written. The town's
// daemon.env sets the credential to nothing, so the refusal comes from the
// town's own file and no value is read from the process environment.
func TestTrackerEnvWithoutAdminCredentialRefuses(t *testing.T) {
	t.Parallel()
	root := writeCrewTown(t, trackerRouteGastown, "")
	if err := os.MkdirAll(filepath.Join(root, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings", "daemon.env"), []byte("TRACKER_ADMIN_TOKEN=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := (&Manager{rig: &rig.Rig{Name: "gastown"}}).trackerEnv(root, "dave")
	if err == nil || !strings.Contains(err.Error(), "tracker token issuer") {
		t.Fatalf("trackerEnv error = %v, want the host issuer to be refused", err)
	}
	if env != nil {
		t.Errorf("environment = %v after a refused issuer, want none", env)
	}
	tokens, err := filepath.Glob(filepath.Join(root, ".runtime", "seats", "gastown", "*.token"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 0 {
		t.Errorf("token files written = %v, want none", tokens)
	}
}
