package tiersweep

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestSweepCgroupReported: a cgroup sampling failure reaches stderr once per run and never changes a pass; a working sampler writes no cgroup line (gt-p24ou5).
func TestSweepCgroupReported(t *testing.T) {
	t.Parallel()
	failing := func() (CgroupSample, error) { return CgroupSample{}, errors.New("cpu.stat: no usage_usec line") }
	working := func() (CgroupSample, error) { return CgroupSample{CPUUS: 1}, nil }
	for _, tc := range []struct {
		name   string
		sample func() (CgroupSample, error)
		lines  int
	}{
		{"failing sample prints once", failing, 1},
		{"working sample prints nothing", working, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			exec := func(string, []string, []string, io.Writer) int { return 0 }
			r := &Runner{Exec: exec, LogDir: t.TempDir(), Timeout: "7m", Stderr: &stderr, Sample: tc.sample}
			got := r.Tier(TierIntegration, ".", nil, nil, []string{"./internal/a", "./internal/b"})
			if got.Passed != 2 || got.Failed != 0 {
				t.Errorf("Tier() = %+v, want both packages passed", got)
			}
			if n := strings.Count(stderr.String(), "timings: cgroup:"); n != tc.lines {
				t.Errorf("cgroup lines = %d, want %d; stderr:\n%s", n, tc.lines, stderr.String())
			}
		})
	}
}
