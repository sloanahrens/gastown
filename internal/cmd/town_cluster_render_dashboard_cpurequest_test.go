package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDashboardCPURequestFlagRenders: a request is the dashboard's requests.cpu with
// no limit; with no flag the container renders no resources.
func TestDashboardCPURequestFlagRenders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ request, want string }{{"2", "2"}, {"", ""}} {
		f := clusterRenderFixture(t)
		f.dashboard, f.dashboardCPURequest = true, tc.request
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		res := renderedDeployment(t, f.out, "-deployment-dashboard.yaml").Spec.Template.Spec.Containers[0].Resources
		if (res == nil) != (tc.want == "") || res != nil && (res.Requests["cpu"] != tc.want || len(res.Limits) != 0) {
			t.Errorf("the dashboard's resources are %+v, want requests.cpu %q and no limit", res, tc.want)
		}
	}
}

// TestDashboardCPURequestFlagRecordReplay: the record holds the request under
// dashboard_cpu_request and replays it; a named flag wins; an old record replays empty.
func TestDashboardCPURequestFlagRecordReplay(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	recorded := renderFlagsFixture()
	recorded.dashboardCPURequest = "2"
	if err := saveRenderFlags(path, recorded); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil || keys["dashboard_cpu_request"] != "2" {
		t.Errorf("the record's dashboard_cpu_request = %v (decode %v), want 2", keys["dashboard_cpu_request"], err)
	}
	none := func(string) bool { return false }
	named := func(flag string) bool { return flag == "dashboard-cpu-request" }
	replayed := func(f clusterRenderFlags, named func(string) bool) string {
		loaded, ok, err := loadRenderFlags(path)
		if err != nil || !ok {
			t.Fatalf("loadRenderFlags = (ok %v, err %v), want the record", ok, err)
		}
		got, err := f.withRecorded(loaded, named)
		if err != nil {
			t.Fatalf("withRecorded: %v", err)
		}
		return got.dashboardCPURequest
	}
	if got := replayed(clusterRenderFlags{}, none); got != "2" {
		t.Errorf("the replayed request = %q, want the recorded 2", got)
	}
	if got := replayed(clusterRenderFlags{dashboardCPURequest: "100m"}, named); got != "100m" {
		t.Errorf("the named request = %q, want the command line's 100m over the record", got)
	}
	if err := os.WriteFile(path, []byte(`{"dir": "/repo/gastown", "cpu": "3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := replayed(clusterRenderFlags{}, none); got != "" {
		t.Errorf("an old record replays request %q, want empty", got)
	}
}
