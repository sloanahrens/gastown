package tiersweep

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestSweepCgroupLine: report writes the cgroup sample a package result carries
// on its line, and writes no cgroup key for one that carries none (gt-0rm5n0).
func TestSweepCgroupLine(t *testing.T) {
	t.Parallel()
	spent := &CgroupSample{CPUUS: 900, ThrottledUS: 80, CPUStallUS: 7, IOStallUS: 3, MemStallUS: 2}
	for _, tc := range []struct {
		name   string
		cgroup *CgroupSample
	}{{"carrying a cgroup", spent}, {"carrying none", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logDir := t.TempDir()
			stderr := &bytes.Buffer{}
			r := &Runner{LogDir: logDir, Stderr: stderr, Timings: OpenTimings(filepath.Join(logDir, TimingsName), ProcessHost, stderr)}
			r.report(TierIntegration, &task{name: "./internal/p", result: packageResult{ran: true, start: time.Now(), log: filepath.Join(logDir, "log"), cgroup: tc.cgroup}})
			line := strings.TrimSuffix(string(r.Timings.Bytes()), "\n")
			var tm Timing
			if err := json.Unmarshal([]byte(line), &tm); err != nil {
				t.Fatalf("unmarshal %q: %v", line, err)
			}
			if got := strings.Contains(line, `"cgroup"`); got != (tc.cgroup != nil) || !reflect.DeepEqual(tm.Cgroup, tc.cgroup) {
				t.Fatalf("line %q: cgroup = %+v, want key %t and sample %+v", line, tm.Cgroup, tc.cgroup != nil, tc.cgroup)
			}
		})
	}
}
