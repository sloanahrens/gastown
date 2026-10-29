package polecat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/seattoken"
)

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

// trackerSeatTown is a temp town whose polecat worktree is already on a
// session branch, so a launch keeps that branch: two launches of one seat
// then hand the session the same environment apart from what a test varies.
func trackerSeatTown(t *testing.T) (town, rigPath, workDir string, w *world) {
	t.Helper()
	town, rigPath, workDir, w = sandboxLaunchTown(t)
	if err := w.repo(workDir).CheckoutNewBranch("polecat/box/gt-seed@seed", "main"); err != nil {
		t.Fatal(err)
	}
	return town, rigPath, workDir, w
}

// writeRigRoute gives the town one route for the rig, on backend.
func writeRigRoute(t *testing.T, town, backend string) {
	t.Helper()
	dir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	route := fmt.Sprintf(`{"path":"gastown/mayor/rig","prefix":"gt-","backend":%q}`+"\n", backend)
	if err := os.WriteFile(filepath.Join(dir, beads.RoutesFileName), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}
}

// launchCapturingEnv launches the polecat named box in the town and returns
// the environment it handed the new session. The capturing tmux refuses the
// session, so the launch stops at session creation with its env recorded.
func launchCapturingEnv(t *testing.T, m *SessionManager, workDir string) (map[string]string, error) {
	t.Helper()
	var got map[string]string
	m.tmux = envCapturingTmux{newFakeSessionTmux(), &got}
	return got, m.launch("box", m.SessionName("box"), SessionStartOptions{WorkDir: workDir})
}

func seatEnvSessionManager(rigPath string, w *world, issuer seattoken.Issuer) *SessionManager {
	return &SessionManager{
		rig:             &rig.Rig{Name: "gastown", Path: rigPath},
		gits:            w.opener(),
		seatTokenIssuer: issuer,
	}
}

// A seat whose rig is on the tracker carries the endpoint and the path to its
// token file, and the token itself reaches neither the environment nor argv.
// A seat whose rig is on Dolt carries neither name: its environment is byte
// for byte what it was.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchTrackerSeatEnv(t *testing.T) {
	town, rigPath, workDir, w := trackerSeatTown(t)
	const token = "trk_seat-token-value"
	issuer := &fakeSeatIssuer{token: token}
	m := seatEnvSessionManager(rigPath, w, issuer)

	cases := []struct {
		name    string
		backend string
	}{
		{"a Dolt rig", beads.BackendDolt},
		{"a tracker rig", beads.BackendTracker},
	}
	envs := make(map[string]map[string]string, len(cases))
	for _, tc := range cases {
		writeRigRoute(t, town, tc.backend)
		env, err := launchCapturingEnv(t, m, workDir)
		if err == nil || !strings.Contains(err.Error(), "captured") {
			t.Fatalf("%s: launch error = %v, want the capturing tmux's refusal", tc.name, err)
		}
		envs[tc.name] = env
	}
	dolt, tracker := envs["a Dolt rig"], envs["a tracker rig"]

	for _, name := range []string{envTrackerURL, envTrackerTokenFile} {
		if got, ok := dolt[name]; ok {
			t.Errorf("a Dolt rig's seat has %s = %q; a Dolt rig gets no tracker environment", name, got)
		}
	}
	if got, want := tracker[envTrackerURL], "http://127.0.0.1:7433"; got != want {
		t.Errorf("TRACKER_URL = %q, want %q (the default the town's config resolves to)", got, want)
	}
	wantPath := filepath.Join(town, ".runtime", "seats", "gastown", "polecats.box.token")
	if got := tracker[envTrackerTokenFile]; got != wantPath {
		t.Errorf("TRACKER_TOKEN_FILE = %q, want %q", got, wantPath)
	}
	if got := stringOfFile(t, wantPath); got != token {
		t.Errorf("token file holds %q, want %q", got, token)
	}
	for name, value := range tracker {
		if strings.Contains(value, token) {
			t.Errorf("the seat environment's %s carries the token itself", name)
		}
	}
	if len(tracker) != len(dolt)+2 {
		t.Errorf("tracker env has %d names, want %d: Dolt's %d plus the two tracker names",
			len(tracker), len(dolt)+2, len(dolt))
	}
	for name, value := range dolt {
		if got := tracker[name]; got != value {
			t.Errorf("a tracker rig changed %s: %q, want %q (a Dolt rig's value)", name, got, value)
		}
	}
}

// A restart reuses the seat's token: the issuer is asked once across two
// launches, and both sessions are given the same file.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchTrackerSeatEnvReusesTheTokenOnRestart(t *testing.T) {
	town, rigPath, workDir, w := trackerSeatTown(t)
	writeRigRoute(t, town, beads.BackendTracker)
	issuer := &fakeSeatIssuer{token: "trk_seat-token-value"}
	m := seatEnvSessionManager(rigPath, w, issuer)

	var paths []string
	for i := range 2 {
		env, err := launchCapturingEnv(t, m, workDir)
		if err == nil || !strings.Contains(err.Error(), "captured") {
			t.Fatalf("launch %d: error = %v, want the capturing tmux's refusal", i+1, err)
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

// A seat that cannot be given a token does not launch: the refusal names the
// rig and seat, and never the token.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchTrackerSeatEnvRefusals(t *testing.T) {
	const token = "trk_seat-token-value"
	cases := []struct {
		name    string
		prepare func(t *testing.T, town string) seattoken.Issuer
		want    string
	}{
		{
			name: "no host credential to build an issuer",
			prepare: func(t *testing.T, _ string) seattoken.Issuer {
				// A nil issuer makes the launch build the host issuer from
				// the town's admin credential, which this town does not have.
				return nil
			},
			want: "TRACKER_ADMIN_TOKEN",
		},
		{
			name: "an issuer that fails after minting",
			prepare: func(t *testing.T, _ string) seattoken.Issuer {
				return &fakeSeatIssuer{token: token, err: errors.New("the tracker is down")}
			},
			want: "the tracker is down",
		},
		{
			name: "a route table that cannot be read",
			prepare: func(t *testing.T, town string) seattoken.Issuer {
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
				return &fakeSeatIssuer{token: token}
			},
			want: "reading the route table",
		},
		{
			name: "a town config that does not resolve the endpoint",
			prepare: func(t *testing.T, town string) seattoken.Issuer {
				t.Helper()
				dir := filepath.Join(town, "mayor")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "town.json"), []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
				return &fakeSeatIssuer{token: token}
			},
			want: "town.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			town, rigPath, workDir, w := trackerSeatTown(t)
			writeRigRoute(t, town, beads.BackendTracker)
			issuer := tc.prepare(t, town)
			m := seatEnvSessionManager(rigPath, w, issuer)

			env, err := launchCapturingEnv(t, m, workDir)
			if err == nil {
				t.Fatal("launch succeeded; a seat with no tracker token must not start")
			}
			if !strings.Contains(err.Error(), "gastown/polecats/box") {
				t.Errorf("launch error = %v, want it to name the seat gastown/polecats/box", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("launch error = %v, want it to name %q", err, tc.want)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("launch error carries the token: %v", err)
			}
			if env != nil {
				t.Errorf("the launch created a session (env captured) after failing to get a token")
			}
		})
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
