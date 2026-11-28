package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTierSweepCPUMillis: runner_cpu_request_m and dind_cpu_request_m ask the
// Kubernetes scheduler for fractions of a core, each 0 for the whole share or
// 100 to the share it splits, and not alongside runner_cpu_request (gt-svprjl).
func TestTierSweepCPUMillis(t *testing.T) {
	t.Parallel()
	const k8s = `"image":"i","jobs":4,"output_root":"/out","context":"gt-control","backend":"kubernetes","memory":8`
	for _, tc := range []struct {
		name, block       string
		wantRun, wantDind int
		wantErr           string
	}{
		{name: "both keys decode", block: `{` + k8s + `,"cpus":9,"dind":true,"dind_cpus":3,"runner_cpu_request_m":500,"dind_cpu_request_m":500}`, wantRun: 500, wantDind: 500},
		{name: "zero is the whole share", block: `{` + k8s + `,"cpus":8}`},
		{name: "runner below 100", block: `{` + k8s + `,"cpus":8,"runner_cpu_request_m":50}`, wantErr: "runner.runner_cpu_request_m"},
		{name: "dind below 100", block: `{` + k8s + `,"cpus":8,"dind":true,"dind_cpu_request_m":99}`, wantErr: "runner.dind_cpu_request_m"},
		{name: "runner on the docker backend", block: `{"image":"i","jobs":4,"output_root":"/out","context":"c","backend":"docker","memory":8,"cpus":8,"runner_cpu_request_m":500}`, wantErr: "runner.runner_cpu_request_m"},
		{name: "dind on the docker backend", block: `{"image":"i","jobs":4,"output_root":"/out","context":"c","backend":"docker","memory":8,"cpus":8,"dind":true,"dind_cpu_request_m":500}`, wantErr: "runner.dind_cpu_request_m"},
		{name: "dind with dind off", block: `{` + k8s + `,"cpus":8,"dind_cpu_request_m":500}`, wantErr: "runner.dind_cpu_request_m"},
		{name: "dind above its share", block: `{` + k8s + `,"cpus":9,"dind":true,"dind_cpus":3,"dind_cpu_request_m":3001}`, wantErr: "runner.dind_cpu_request_m"},
		{name: "runner above its share", block: `{` + k8s + `,"cpus":8,"runner_cpu_request_m":8001}`, wantErr: "runner.runner_cpu_request_m"},
		{name: "runner above its share with dind", block: `{` + k8s + `,"cpus":8,"dind":true,"runner_cpu_request_m":6001}`, wantErr: "runner.runner_cpu_request_m"},
		{name: "both runner keys", block: `{` + k8s + `,"cpus":8,"runner_cpu_request":4,"runner_cpu_request_m":4000}`, wantErr: "runner.runner_cpu_request_m"},
	} {
		var p PatrolsConfig
		err := json.Unmarshal([]byte(`{"tier_sweep":{"runner":`+tc.block+`}}`), &p)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s: error = %v, want it to name %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: a runner block with millicore requests does not decode: %v", tc.name, err)
		}
		if r := p.TierSweep.Runner; r == nil || r.RunnerCPURequestMillis != tc.wantRun || r.DindCPURequestMillis != tc.wantDind {
			t.Fatalf("%s: runner = %+v, want requests %d and %d millicores", tc.name, r, tc.wantRun, tc.wantDind)
		}
	}
}
