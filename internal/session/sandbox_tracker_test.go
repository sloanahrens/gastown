package session

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/seattoken"
)

// openshellSeatTokenValue is the seat token the tests stage. It is distinctive
// so a test can prove the token reaches the archive's file and nothing else.
const openshellSeatTokenValue = "trk_seat-token-appears-once"

// openshellTrackerTownJSON is a town whose tracker listens on 7600, so a test
// proves the port comes from the town's config and not from the default.
const openshellTrackerTownJSON = `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","tracker":{"port":7600}}`

// openshellRoute writes the town's route table with one route for the test
// rig's store, on backend ("" leaves the Dolt default).
func openshellRoute(t *testing.T, town, backend string) {
	t.Helper()
	route := `{"path":"` + openshellTestRig + `/mayor/rig","prefix":"gt-"`
	if backend != "" {
		route += `,"backend":"` + backend + `"`
	}
	writeOpenshellFile(t, filepath.Join(town, ".beads", "routes.jsonl"), route+"}\n")
}

// openshellSeatToken writes the test polecat's seat token file, 0600, and
// returns its path.
func openshellSeatToken(t *testing.T, town string) string {
	t.Helper()
	path := filepath.Join(town, ".runtime", "seats", openshellTestRig, "polecats."+openshellTestPolecat+".token")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(openshellSeatTokenValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// openshellDoltContract is the whole contract of a rig on Dolt, every key and
// value: a Dolt rig's contract must stay byte for byte what it was before the
// tracker joined it.
func openshellDoltContract() map[string]string {
	return map[string]string{
		"GT_TOWN_ROOT":           "/sandbox/gt",
		"GT_ROOT":                "/sandbox/gt",
		"GT_RIG":                 openshellTestRig,
		"GT_POLECAT":             openshellTestPolecat,
		"GT_ROLE":                openshellTestRig + "/polecats/" + openshellTestPolecat,
		"BD_ACTOR":               openshellTestRig + "/polecats/" + openshellTestPolecat,
		"GT_SESSION_BACKEND":     BackendOpenShell,
		"GT_TMUX_SOCKET":         "gt",
		"BEADS_DOLT_SERVER_MODE": "1",
		"BEADS_DOLT_SERVER_HOST": defaultControlPlaneHost,
		"BEADS_DOLT_SERVER_PORT": "3307",
		"BD_DISABLE_METRICS":     "1",
		"DO_NOT_TRACK":           "1",
		"DISABLE_AUTOUPDATER":    "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
}

// openshellContractOnTracker is the Dolt contract plus the three tracker
// variables, so the table below states exactly what the tracker adds. host is
// the control-plane host the profile names, which is also the Dolt host.
func openshellContractOnTracker(url, host string) map[string]string {
	env := openshellDoltContract()
	env["BEADS_DOLT_SERVER_HOST"] = host
	env["TRACKER_URL"] = url
	env["TRACKER_ALLOW_HTTP_HOST"] = host
	env["TRACKER_TOKEN_FILE"] = "/sandbox/.tracker/token"
	return env
}

// A tracker-routed rig's create carries TRACKER_URL on the control-plane host
// and the town's tracker port, that host as the one plain http may reach, and
// the token file's sandbox path. A rig on Dolt, or one the route table does not
// name, gets the contract it had before, and no token is asked for.
func TestOpenShellContractCarriesTheTrackerForATrackerRoutedRig(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		backend      string // "" writes no route table at all
		trackerTown  bool
		controlPlane string
		want         map[string]string
	}{
		{
			name:    "a rig the route table names with the Dolt backend",
			backend: beads.BackendDolt,
			want:    openshellDoltContract(),
		},
		{
			name: "a rig the route table does not name",
			want: openshellDoltContract(),
		},
		{
			name:        "a tracker rig takes the town's tracker port",
			backend:     beads.BackendTracker,
			trackerTown: true,
			want:        openshellContractOnTracker("http://host.docker.internal:7600", "host.docker.internal"),
		},
		{
			name:         "a tracker rig reaches the profile's control-plane host",
			backend:      beads.BackendTracker,
			controlPlane: "control-plane.openshell.svc.cluster.local",
			want:         openshellContractOnTracker("http://control-plane.openshell.svc.cluster.local:7433", "control-plane.openshell.svc.cluster.local"),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			if c.backend != "" {
				openshellRoute(t, town, c.backend)
			}
			if c.trackerTown {
				writeOpenshellFile(t, filepath.Join(town, "mayor", "town.json"), openshellTrackerTownJSON)
			}
			p := openshellTestProfile
			p.ControlPlaneHost = c.controlPlane
			b, _ := openshellTestBackend(town, p, &sandbox.Fake{})
			tokenPath := openshellSeatToken(t, town)
			b.seatToken = func(context.Context, intent.Seat) (string, error) {
				if c.backend != beads.BackendTracker {
					t.Errorf("a Dolt rig asked for a seat token; its sandbox carries none")
				}
				return tokenPath, nil
			}

			spec := openshellSpawn(town, work)
			tracker, err := b.trackerFor(context.Background(), spec)
			if err != nil {
				t.Fatalf("trackerFor: %v", err)
			}
			cs, err := b.createSpec(spec, tracker)
			if err != nil {
				t.Fatalf("createSpec: %v", err)
			}
			if !reflect.DeepEqual(cs.Env, c.want) {
				t.Fatalf("contract:\n got %q\nwant %q", cs.Env, c.want)
			}
		})
	}
}

// A route table that cannot be read, or one naming a backend that is neither
// Dolt nor the tracker, is an error: a tracker rig must never be given a Dolt
// contract because its route could not be understood.
func TestOpenShellTrackerForRefusesWhatItCannotResolve(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		prepare func(t *testing.T, town string)
		// seatToken is what the backend's seam answers; nil means no case here
		// may reach it.
		seatToken func(t *testing.T, town string) (string, error)
		noSeam    bool // leave the backend's token seam unset
		wantErr   string
	}{
		{
			name: "a route naming an unknown backend",
			prepare: func(t *testing.T, town string) {
				openshellRoute(t, town, "postgres")
			},
			wantErr: `"postgres"`,
		},
		{
			name: "a tracker rig with no token seam installed",
			prepare: func(t *testing.T, town string) {
				openshellRoute(t, town, beads.BackendTracker)
			},
			noSeam:  true,
			wantErr: "no issuer is installed",
		},
		{
			name: "a route table that cannot be read",
			prepare: func(t *testing.T, town string) {
				if err := os.MkdirAll(filepath.Join(town, ".beads", "routes.jsonl"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "reading the town routes",
		},
		{
			name: "a town whose tracker port does not parse",
			prepare: func(t *testing.T, town string) {
				openshellRoute(t, town, beads.BackendTracker)
				writeOpenshellFile(t, filepath.Join(town, "mayor", "town.json"), `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","tracker":{"port":"seven"}}`)
			},
			wantErr: "resolving the tracker endpoint",
		},
		{
			name: "a seat token file that cannot be read",
			prepare: func(t *testing.T, town string) {
				openshellRoute(t, town, beads.BackendTracker)
			},
			seatToken: func(t *testing.T, town string) (string, error) {
				return filepath.Join(town, ".runtime", "seats", "missing.token"), nil
			},
			wantErr: "the seat's tracker token",
		},
		{
			name: "a seat token file that holds nothing",
			prepare: func(t *testing.T, town string) {
				openshellRoute(t, town, beads.BackendTracker)
			},
			seatToken: func(t *testing.T, town string) (string, error) {
				path := openshellSeatToken(t, town)
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return path, nil
			},
			wantErr: "holds no token",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			c.prepare(t, town)
			b, _ := openshellTestBackend(town, openshellTestProfile, &sandbox.Fake{})
			switch {
			case c.noSeam:
			case c.seatToken != nil:
				b.seatToken = func(context.Context, intent.Seat) (string, error) { return c.seatToken(t, town) }
			default:
				b.seatToken = func(context.Context, intent.Seat) (string, error) {
					t.Error("a seat token was asked for before the route could be resolved")
					return "", errors.New("not reached")
				}
			}

			_, err := b.trackerFor(context.Background(), openshellSpawn(town, work))
			if err == nil {
				t.Fatal("trackerFor = nil error; the seat would be given a contract without its tracker")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("trackerFor error = %v, want it to name %q", err, c.wantErr)
			}
		})
	}
}

// The production seat-token seam refuses a town with no admin credential before
// it asks the tracker for anything, and names the credential rather than a
// value.
func TestHostSeatTokenRefusesATownWithNoAdminCredential(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	_, err := hostSeatToken(town)(context.Background(), intent.Seat{Rig: openshellTestRig, Role: "polecats", Name: openshellTestPolecat})
	if err == nil {
		t.Fatal("hostSeatToken produced a token file with no admin credential")
	}
	if !strings.Contains(err.Error(), "TRACKER_ADMIN_TOKEN") {
		t.Errorf("hostSeatToken error = %v, want it to name the credential", err)
	}
}

// A tracker-routed seat's token reaches the sandbox as one 0600 file in the
// runtime archive, and no other entry, no environment value the agent runs
// with and no value the gateway's create process reads carries it.
func TestOpenShellPrepareStagesTheSeatTokenAsOneFile(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	openshellRoute(t, town, beads.BackendTracker)
	tokenPath := openshellSeatToken(t, town)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	b.seatToken = func(_ context.Context, seat intent.Seat) (string, error) {
		if want := openshellTestRig + "/polecats/" + openshellTestPolecat; seat.String() != want {
			t.Errorf("seat = %q, want the polecat's identity %q", seat, want)
		}
		return tokenPath, nil
	}
	// The staging directory is removed when Prepare returns, so the archive is
	// read from disk at the upload that names it.
	base := fake.Runner()
	var uploaded []byte
	b.cli = &sandbox.CLI{Gateway: openshellTestProfile.Gateway, Run: func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		for _, a := range argv {
			if filepath.Base(a) != runtimeArchiveName || strings.HasPrefix(a, sandboxHome+"/") {
				continue
			}
			data, err := os.ReadFile(a)
			if err != nil {
				t.Errorf("reading the uploaded runtime archive: %v", err)
			}
			uploaded = data
		}
		return base(ctx, argv, stdin, env)
	}}

	prepared, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if uploaded == nil {
		t.Fatal("Prepare uploaded no runtime archive")
	}
	entries := untarRuntime(t, uploaded)
	token, ok := entries[runtimeTrackerToken]
	if !ok {
		t.Fatalf("runtime archive entries = %v, want %s", entries, runtimeTrackerToken)
	}
	if token.mode != 0o600 {
		t.Errorf("%s mode = %o, want 600", runtimeTrackerToken, token.mode)
	}
	if got := strings.TrimSpace(token.body); got != openshellSeatTokenValue {
		t.Errorf("%s = %q, want the seat's token", runtimeTrackerToken, got)
	}
	if dir, ok := entries[".tracker/"]; !ok || dir.mode != 0o700 {
		t.Errorf("archive directory entry = %+v (present %v), want .tracker/ at mode 700", dir, ok)
	}
	for name, entry := range entries {
		if name != runtimeTrackerToken && strings.Contains(entry.body, openshellSeatTokenValue) {
			t.Errorf("archive entry %s carries the seat token; only %s may", name, runtimeTrackerToken)
		}
	}
	for k, v := range prepared.Env {
		if strings.Contains(v, openshellSeatTokenValue) {
			t.Errorf("the agent's environment carries the seat token in %s", k)
		}
	}
	if prepared.Env["TRACKER_TOKEN_FILE"] != sandboxTrackerTokenFile || prepared.Env["TRACKER_URL"] == "" {
		t.Errorf("Prepared.Env tracker values = %q, want the token file %s and the URL", prepared.Env, sandboxTrackerTokenFile)
	}
	for _, row := range fake.Envs() {
		for _, kv := range row {
			if strings.Contains(kv, openshellSeatTokenValue) {
				t.Errorf("the gateway's create environment carries the seat token in %q", kv)
			}
		}
	}
}

// failingIssuer is an issuer that answers with an error while still returning a
// token, the worst shape for a leak: nothing of that token may reach Prepare's
// error.
type failingIssuer struct {
	token string
	err   error
}

func (f failingIssuer) Issue(context.Context, string) (string, error) { return f.token, f.err }

// An issuer failure fails Prepare before anything is pushed or created, names
// what failed, and leaves no token in the error and no file behind.
func TestOpenShellPrepareFailsWhenTheSeatTokenCannotBeIssued(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	openshellRoute(t, town, beads.BackendTracker)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)
	b.seatToken = func(ctx context.Context, seat intent.Seat) (string, error) {
		return seattoken.New(town).Ensure(ctx, seat, failingIssuer{token: openshellSeatTokenValue, err: errors.New("tracker: 403 forbidden")})
	}

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil {
		t.Fatal("Prepare succeeded with no seat token; the sandbox would start on a tracker it cannot read")
	}
	if strings.Contains(err.Error(), openshellSeatTokenValue) {
		t.Errorf("Prepare error carries the seat token: %v", err)
	}
	if !strings.Contains(err.Error(), "403 forbidden") {
		t.Errorf("Prepare error = %v, want the issuer's failure", err)
	}
	if len(*pushes) != 0 || len(fake.Calls()) != 0 {
		t.Errorf("pushes = %d, sandbox calls = %d; a failed issue must touch nothing", len(*pushes), len(fake.Calls()))
	}
	tokenFile := filepath.Join(town, ".runtime", "seats", openshellTestRig, "polecats."+openshellTestPolecat+".token")
	if _, statErr := os.Stat(tokenFile); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a failed issue left %s behind: %v", tokenFile, statErr)
	}
}

// A Dolt rig's Prepare asks for no seat token at all, so a town with no tracker
// credential still spawns its sandboxes.
func TestOpenShellPrepareAsksForNoTokenOnADoltRig(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	b.seatToken = func(context.Context, intent.Seat) (string, error) {
		t.Error("a Dolt rig asked for a seat token; its sandbox carries none")
		return "", errors.New("not reached")
	}

	prepared, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, k := range []string{"TRACKER_URL", "TRACKER_ALLOW_HTTP_HOST", "TRACKER_TOKEN_FILE"} {
		if _, ok := prepared.Env[k]; ok {
			t.Errorf("a Dolt rig's environment carries %s", k)
		}
	}
}
