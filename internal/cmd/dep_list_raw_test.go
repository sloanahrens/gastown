package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/backend"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// The raw dependency query authenticates with the town's configured Dolt
// password; a DSN that drops it made a password-protected server answer access
// denied, and depListRawIDs fell back to the slower bd path (gt-napz3).
func TestDepListRawDSNCarriesPassword(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := `{"type":"town-settings","version":1,"operational":{"dolt":{"password":"secret"}}}`
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	dsn := depListRawDSN(depListRawUserInfo(beadsDir), "127.0.0.1", 3307, "hq")
	if want := "root:secret@tcp(127.0.0.1:3307)/hq"; !strings.HasPrefix(dsn, want) {
		t.Errorf("depListRawDSN = %q, want prefix %q", dsn, want)
	}
}

// A dir outside any town has no Dolt config to read, so the raw query connects
// as the default user with no password (gt-sktin).
func TestDepListRawUserInfoOutsideTownIsDefaultUser(t *testing.T) {
	t.Parallel()
	if got := depListRawUserInfo(t.TempDir()); got != doltserver.DefaultUser {
		t.Errorf("depListRawUserInfo outside a town = %q, want %q", got, doltserver.DefaultUser)
	}
}

// A town whose Dolt config sets a user and no password connects as the bare
// user. The user is not the default one, so the result shows the town's config
// was read rather than the default user returned (gt-sktin).
func TestDepListRawUserInfoWithoutPassword(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"type":"town-settings","version":1,"operational":{"dolt":{"user":"admin"}}}`
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := depListRawUserInfo(townRoot), "admin"; got != want {
		t.Errorf("depListRawUserInfo without a password = %q, want %q", got, want)
	}
}

// trackerTown is a town whose route table puts the tr- rig on the tracker and
// the gt- rig on Dolt. It returns the town root and the tracker rig's directory.
func trackerTown(t *testing.T) (townRoot, trackerRig string) {
	t.Helper()
	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	routes := []beads.Route{
		{Prefix: "tr-", Path: "trk/mayor/rig", Backend: beads.BackendTracker},
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
	}
	if err := beads.WriteRoutes(filepath.Join(townRoot, ".beads"), routes); err != nil {
		t.Fatal(err)
	}
	trackerRig = filepath.Join(townRoot, "trk", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(trackerRig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	return townRoot, trackerRig
}

// handlerTransport answers requests in-process, so the tracker client never
// leaves the test.
type handlerTransport http.HandlerFunc

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	http.HandlerFunc(h).ServeHTTP(rec, r)
	return rec.Result(), nil
}

// trackerFixture serves tr-x's edges: it depends on tr-a (blocks), tr-b
// (tracks), tr-a again, and external:tr:tr-c (blocks); tr-w (blocks) and tr-v
// (tracks) depend on it. The dependencies route filters by ?type as the real
// service does.
func trackerFixture(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/issues/tr-x/dependencies":
		all := []api.IssueDep{
			{ID: "tr-a", DependencyType: "blocks"},
			{ID: "tr-b", DependencyType: "tracks"},
			{ID: "tr-a", DependencyType: "blocks"},
			{ID: "external:tr:tr-c", DependencyType: "blocks"},
		}
		want := r.URL.Query().Get("type")
		out := []api.IssueDep{}
		for _, d := range all {
			if want == "" || d.DependencyType == want {
				out = append(out, d)
			}
		}
		_ = json.NewEncoder(w).Encode(api.DependenciesResponse{Dependencies: out})
	case "/v1/issues/tr-x":
		_ = json.NewEncoder(w).Encode(api.Issue{ID: "tr-x", Dependents: []api.IssueDep{
			{ID: "tr-w", DependencyType: "blocks"},
			{ID: "tr-v", DependencyType: "tracks"},
		}})
	default:
		http.NotFound(w, r)
	}
}

// failIfDolt is the Dolt read for a case that must never reach it: calling it
// fails the test.
func failIfDolt(t *testing.T) rawDepDolt {
	return func(string, string, string, string) ([]string, error) {
		t.Error("the query reached the Dolt read for a rig the seam must answer or refuse")
		return nil, errors.New("the Dolt read was reached")
	}
}

func trackerConfig(handler http.HandlerFunc) backend.Config {
	return backend.Config{Tracker: client.Config{
		BaseURL: "http://127.0.0.1:7433", Token: "t", SchemaLevel: api.SchemaLevel, MaxRetries: -1,
		HTTP: &http.Client{Transport: handlerTransport(handler)},
	}}
}

// On a tracker rig the query answers from the seam: dependencies for "down",
// dependents for "up", filtered by the dependency type, deduplicated and
// unwrapped the way the raw path unwraps them.
func TestDepListRawIDsOnATrackerRigAnswersFromTheSeam(t *testing.T) {
	t.Parallel()
	_, rig := trackerTown(t)
	cases := []struct {
		name, direction, depType string
		want                     []string
	}{
		{"down, any type", "down", "", []string{"tr-a", "tr-b", "tr-c"}},
		{"down, blocks only", "down", "blocks", []string{"tr-a", "tr-c"}},
		{"up, any type", "up", "", []string{"tr-w", "tr-v"}},
		{"up, blocks only", "up", "blocks", []string{"tr-w"}},
	}
	for _, tc := range cases {
		got, err := depListRawIDsWith(rig, "tr-x", tc.direction, tc.depType, trackerConfig(trackerFixture), workspace.Find, failIfDolt(t))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A tracker rig never opens a Dolt connection, even when its metadata names a
// Dolt server. The opener is the one place the raw path dials Dolt: it counts
// each attempt and refuses it, so nothing is dialed in the unit tier.
func TestDepListRawIDsOnATrackerRigNeverOpensDolt(t *testing.T) {
	t.Parallel()
	_, rig := trackerTown(t)
	writeDoltRuntime(t, filepath.Join(rig, ".beads"))
	opens := 0
	dolt := func(dir, issueID, direction, depType string) ([]string, error) {
		return depListRawIDsViaDoltWith(dir, issueID, direction, depType, countingOpener(&opens))
	}

	got, err := depListRawIDsWith(rig, "tr-x", "down", "", trackerConfig(trackerFixture), workspace.Find, dolt)
	if opens != 0 {
		t.Errorf("the tracker rig opened %d Dolt connection(s), want none", opens)
	}
	if err != nil {
		t.Fatalf("depListRawIDsWith: %v", err)
	}
	if want := []string{"tr-a", "tr-b", "tr-c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("depListRawIDsWith = %v, want %v", got, want)
	}
}

// The zero above means something only if the counter is wired: a Dolt read
// reaches the opener exactly once.
func TestDepListRawIDsViaDoltOpensOneConnection(t *testing.T) {
	t.Parallel()
	townRoot, _ := trackerTown(t)
	gtRig := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(gtRig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoltRuntime(t, filepath.Join(gtRig, ".beads"))
	opens := 0
	if _, err := depListRawIDsViaDoltWith(gtRig, "gt-x", "down", "", countingOpener(&opens)); err == nil {
		t.Fatal("depListRawIDsViaDoltWith with a refused connection = nil error, want the refusal")
	}
	if opens != 1 {
		t.Errorf("the Dolt read opened %d connection(s), want 1", opens)
	}
}

// countingOpener refuses every connection and counts the attempts.
func countingOpener(opens *int) doltOpener {
	return func(context.Context, string, string) (*beadsql.DB, error) {
		*opens++
		return nil, errors.New("connection refused in the unit tier")
	}
}

// writeDoltRuntime gives beadsDir the Dolt server metadata the raw path reads its
// host, port and database from, so a Dolt read would get as far as the opener.
func writeDoltRuntime(t *testing.T, beadsDir string) {
	t.Helper()
	meta := `{"backend":"dolt","dolt_mode":"server","database":"trk","dolt_server_host":"127.0.0.1","dolt_server_port":3307}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

// unreadableRoutes replaces the route table with a directory: it opens, but it
// cannot be read as a file.
func unreadableRoutes(t *testing.T, townRoot string) {
	t.Helper()
	routes := filepath.Join(townRoot, ".beads", beads.RoutesFileName)
	if err := os.Remove(routes); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(routes, 0o755); err != nil {
		t.Fatal(err)
	}
}

// A tracker rig whose tracker is not configured is refused. It is not read
// through Dolt or bd sql instead.
func TestDepListRawIDsOnATrackerRigWithNoTrackerConfigRefuses(t *testing.T) {
	t.Parallel()
	_, rig := trackerTown(t)
	_, err := depListRawIDsWith(rig, "tr-x", "down", "", backend.Config{}, workspace.Find, failIfDolt(t))
	if err == nil {
		t.Fatal("depListRawIDsWith on an unconfigured tracker rig = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "TRACKER_URL") {
		t.Errorf("error = %v, want it to name the missing TRACKER_URL", err)
	}
}

// When the town or the route table cannot be read, the seam cannot tell a
// tracker rig from a Dolt rig, so it refuses: a Dolt read on a guess could
// answer a tracker rig from a store it no longer writes to. This holds for a
// Dolt rig too, as the rest of gt already refuses such a rig: backend.Open fails
// on the same unreadable table. The Dolt read is never reached.
func TestDepListRawIDsRefusesWhenTheSeamCannotReadTheTownOrRoutes(t *testing.T) {
	t.Parallel()
	broken := func(string) (string, error) { return "", errors.New("the working directory is gone") }
	cases := []struct {
		name    string
		tracker bool
		setup   func(t *testing.T, townRoot string)
		find    townRootFinder
	}{
		{"a Dolt rig whose town cannot be found", false, func(*testing.T, string) {}, broken},
		{"a Dolt rig whose route table cannot be read", false, unreadableRoutes, workspace.Find},
		{"a tracker rig whose route table cannot be read", true, unreadableRoutes, workspace.Find},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot, trackerRig := trackerTown(t)
			tc.setup(t, townRoot)
			rig := filepath.Join(townRoot, "gastown", "mayor", "rig")
			if tc.tracker {
				rig = trackerRig
			}
			if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			ids, err := depListRawIDsWith(rig, "gt-x", "down", "", backend.Config{}, tc.find, failIfDolt(t))
			if err == nil || errors.Is(err, errNotOnTracker) {
				t.Fatalf("depListRawIDsWith = %v, %v; want a refusal", ids, err)
			}
		})
	}
}

// A missing issue on the tracker is an error from the seam, not an empty answer.
func TestDepListRawIDsOnATrackerRigMissingIssueIsAnError(t *testing.T) {
	t.Parallel()
	_, rig := trackerTown(t)
	_, err := depListRawIDsWith(rig, "tr-missing", "down", "", trackerConfig(trackerFixture), workspace.Find, failIfDolt(t))
	if err == nil || errors.Is(err, errNotOnTracker) {
		t.Fatalf("depListRawIDsWith for a missing issue = %v, want an error", err)
	}
}

// A Dolt rig, whether its route names no backend or it has no route at all, and
// a directory outside any town, are not handled by the tracker seam, so they
// keep the raw path.
func TestDepListRawIDsOnADoltRigIsNotHandledByTheSeam(t *testing.T) {
	t.Parallel()
	townRoot, _ := trackerTown(t)
	gtRig := filepath.Join(townRoot, "gastown", "mayor", "rig")
	elsewhere := filepath.Join(townRoot, "elsewhere")
	for name, dir := range map[string]string{"a route with no backend": gtRig, "a directory with no route": elsewhere, "a directory outside any town": t.TempDir()} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		ids, err := depListRawIDsOnTracker(dir, "gt-x", "down", "", backend.Config{}, workspace.Find)
		if !errors.Is(err, errNotOnTracker) || ids != nil {
			t.Errorf("%s: depListRawIDsOnTracker = %v, %v; want errNotOnTracker", name, ids, err)
		}
	}
}
