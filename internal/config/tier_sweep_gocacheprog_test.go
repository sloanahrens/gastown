package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGoCacheProgConfig holds go_cache_prog_max_bytes to its refusals: a
// positive cap on the Kubernetes backend loads, zero and an omitted key are
// off, and a negative value or a positive one on the Docker backend is refused
// naming the key (gt-7gezj1).
func TestGoCacheProgConfig(t *testing.T) {
	t.Parallel()
	const k8s = `"image":"i","jobs":4,"output_root":"/out","context":"gt-control","backend":"kubernetes","memory":8,"cpus":8`
	const docker = `"image":"i","jobs":4,"output_root":"/out","context":"c","backend":"docker","memory":8,"cpus":8`
	for _, tc := range []struct {
		name, block string
		want        int
		wantErr     string
	}{
		{name: "positive on kubernetes loads", block: `{` + k8s + `,"go_cache_prog_max_bytes":1073741824}`, want: 1073741824},
		{name: "omitted is off", block: `{` + k8s + `}`},
		{name: "zero is off", block: `{` + k8s + `,"go_cache_prog_max_bytes":0}`},
		{name: "negative", block: `{` + k8s + `,"go_cache_prog_max_bytes":-1}`, wantErr: "runner.go_cache_prog_max_bytes"},
		{name: "positive on the docker backend", block: `{` + docker + `,"go_cache_prog_max_bytes":1024}`, wantErr: "runner.go_cache_prog_max_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			err := json.Unmarshal([]byte(`{"tier_sweep":{"runner":`+tc.block+`}}`), &p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("the runner block does not decode: %v", err)
			}
			if r := p.TierSweep.Runner; r == nil || r.GoCacheProgMaxBytes != tc.want {
				t.Fatalf("runner = %+v, want GoCacheProgMaxBytes %d", r, tc.want)
			}
		})
	}
}
