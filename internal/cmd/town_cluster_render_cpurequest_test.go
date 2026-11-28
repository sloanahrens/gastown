package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDaemonCPURequestFlagRenders: a request is written as the daemon's requests.cpu
// under the limit; with no flag the request equals the limit.
func TestDaemonCPURequestFlagRenders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, request, want string }{
		{"a named request is written under the limit", "1", "1"},
		{"no request keeps requests equal to the limit", "", "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			f.cpu, f.cpuRequest = "3", tc.request
			var out bytes.Buffer
			if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
				t.Fatalf("render: %v", err)
			}
			res := renderedDeployment(t, f.out, "-deployment-daemon.yaml").Spec.Template.Spec.Containers[0].Resources
			if res == nil || res.Requests["cpu"] != tc.want || res.Limits["cpu"] != "3" {
				t.Errorf("the daemon's resources are %+v, want requests.cpu %s and limits.cpu 3", res, tc.want)
			}
		})
	}
}

// TestDaemonCPURequestFlagRecordReplay: the record holds the request under cpu_request
// and replays it; a flag named on the command line wins; a record with no such key
// replays an empty request.
func TestDaemonCPURequestFlagRecordReplay(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	recorded := renderFlagsFixture()
	recorded.cpuRequest = "1"
	if err := saveRenderFlags(path, recorded); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil || keys["cpu_request"] != "1" {
		t.Errorf("the record's cpu_request = %v (decode %v), want 1", keys["cpu_request"], err)
	}
	loaded, ok, err := loadRenderFlags(path)
	if err != nil || !ok {
		t.Fatalf("loadRenderFlags = (ok %v, err %v), want the record", ok, err)
	}
	none := func(string) bool { return false }
	if replay, err := (clusterRenderFlags{}).withRecorded(loaded, none); err != nil || replay.cpuRequest != "1" {
		t.Errorf("the replayed request = %q (err %v), want the recorded 1", replay.cpuRequest, err)
	}
	named := func(flag string) bool { return flag == "cpu-request" }
	if wins, err := (clusterRenderFlags{cpuRequest: "2"}).withRecorded(loaded, named); err != nil || wins.cpuRequest != "2" {
		t.Errorf("the named request = %q (err %v), want the command line's 2 over the record", wins.cpuRequest, err)
	}
	if err := os.WriteFile(path, []byte(`{"dir": "/repo/gastown", "cpu": "3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	oldLoaded, ok, err := loadRenderFlags(path)
	if err != nil || !ok {
		t.Fatalf("loadRenderFlags on the old record = (ok %v, err %v)", ok, err)
	}
	if replay, err := (clusterRenderFlags{}).withRecorded(oldLoaded, none); err != nil || replay.cpuRequest != "" {
		t.Errorf("the old record replays request %q (err %v), want empty", replay.cpuRequest, err)
	}
}
