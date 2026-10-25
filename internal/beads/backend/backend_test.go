package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/trackerclient"
	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/tracker/client"
)

func TestMain(m *testing.M) { os.Exit(testutil.HermeticMain(m)) }

// town writes a route table and returns the town root.
func town(t *testing.T, routes ...beads.Route) string {
	t.Helper()
	root := t.TempDir()
	if err := beads.WriteRoutes(filepath.Join(root, ".beads"), routes); err != nil {
		t.Fatal(err)
	}
	return root
}

var good = Config{Tracker: client.Config{BaseURL: "http://127.0.0.1:7433", Token: "trk_x"}}

// Unit tier: a temp directory and constructors; no bd, no network.
func TestARigWithNoBackendOrNoRouteIsOnDolt(t *testing.T) {
	t.Parallel()
	root := town(t, beads.Route{Prefix: "gt-", Path: "gastown/mayor/rig"}, beads.Route{Prefix: "al-", Path: "agentlab/mayor/rig", Backend: "dolt"})
	for name, dir := range map[string]string{
		"a route with no backend":   filepath.Join(root, "gastown/mayor/rig/.beads"),
		"a route naming dolt":       filepath.Join(root, "agentlab/mayor/rig/.beads"),
		"a directory with no route": filepath.Join(root, "elsewhere/.beads"),
	} {
		st, err := Open(root, dir, good)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if _, ok := st.(*beads.Beads); !ok {
			t.Errorf("%s: opened %T, want *beads.Beads", name, st)
		}
	}
}

func TestARigOnTheTrackerOpensTheTrackerClientForItsPrefix(t *testing.T) {
	t.Parallel()
	root := town(t, beads.Route{Prefix: "al-", Path: "agentlab/mayor/rig", Backend: beads.BackendTracker}, beads.Route{Prefix: "hq-", Path: ".", Backend: beads.BackendTracker})
	for name, dir := range map[string]string{"a rig": filepath.Join(root, "agentlab/mayor/rig/.beads"), "the town beads (path .)": filepath.Join(root, ".beads")} {
		st, err := Open(root, dir, good)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		tc, ok := st.(*trackerclient.Client)
		if !ok {
			t.Errorf("%s: opened %T, want *trackerclient.Client", name, st)
			continue
		}
		if v, err := tc.ConfigGet("issue_prefix"); err != nil || (name == "a rig" && v != "al") || (name != "a rig" && v != "hq") {
			t.Errorf("%s: issue_prefix = %q, %v; want the route's prefix without its dash", name, v, err)
		}
	}
}

func TestATrackerRigNeverFallsBackToDoltWhenTheTrackerIsNotConfigured(t *testing.T) {
	t.Parallel()
	root := town(t, beads.Route{Prefix: "al-", Path: "agentlab/mayor/rig", Backend: beads.BackendTracker})
	dir := filepath.Join(root, "agentlab/mayor/rig/.beads")
	for name, tc := range map[string]struct {
		cfg  Config
		want string
	}{
		"no url":                   {Config{Tracker: client.Config{Token: "trk_x"}}, "TRACKER_URL"},
		"no token":                 {Config{Tracker: client.Config{BaseURL: "http://127.0.0.1:7433"}}, "TRACKER_TOKEN"},
		"a url the client refuses": {Config{Tracker: client.Config{BaseURL: "http://example.com", Token: "trk_secret"}}, "al-"},
	} {
		st, err := Open(root, dir, tc.cfg)
		if err == nil || st != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Open = %v, %v; want an error mentioning %q and no store", name, st, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "trk_secret") {
			t.Errorf("%s: the error leaks the token: %v", name, err)
		}
	}
}

func TestAnUnknownBackendIsAnErrorNotADefault(t *testing.T) {
	t.Parallel()
	root := town(t, beads.Route{Prefix: "al-", Path: "agentlab/mayor/rig", Backend: "postgres"})
	st, err := Open(root, filepath.Join(root, "agentlab/mayor/rig/.beads"), good)
	if err == nil || st != nil || !strings.Contains(err.Error(), `"postgres"`) {
		t.Errorf("Open = %v, %v; want an error naming the backend", st, err)
	}
}

func TestTheBackendSurvivesAWriteOfTheRouteTable(t *testing.T) {
	t.Parallel()
	root := town(t, beads.Route{Prefix: "al-", Path: "agentlab/mayor/rig", Backend: beads.BackendTracker})
	if err := beads.AppendRoute(root, beads.Route{Prefix: "zz-", Path: "zz/mayor/rig"}); err != nil {
		t.Fatal(err)
	}
	routes, err := beads.LoadRoutes(filepath.Join(root, ".beads"))
	if err != nil || len(routes) != 2 || routes[0].BackendName() != beads.BackendTracker || routes[1].BackendName() != beads.BackendDolt {
		t.Errorf("routes after an append = %+v, %v; want the backend kept for al- and defaulted for zz-", routes, err)
	}
}

func TestFromEnvReadsTheTrackerSettings(t *testing.T) {
	t.Parallel()
	cfg := FromEnv(func(k string) string {
		return map[string]string{"TRACKER_URL": "http://127.0.0.1:7433", "TRACKER_TOKEN": "trk_x"}[k]
	})
	if cfg.Tracker.BaseURL != "http://127.0.0.1:7433" || cfg.Tracker.Token != "trk_x" {
		t.Errorf("FromEnv = %+v", cfg)
	}
}
