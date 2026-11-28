package tiersweep

import (
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeCounters is the container's cgroup: the first read sees start, reads after the command see last, and read failAt fails.
type fakeCounters struct {
	start, last   CgroupSample
	failAt, reads int
	ran           bool
}

func (f *fakeCounters) sample() (CgroupSample, error) {
	f.reads++
	switch {
	case f.reads == f.failAt:
		return CgroupSample{}, errors.New("cpu.stat: no usage_usec line")
	case f.ran:
		return f.last, nil
	}
	return f.start, nil
}

// TestSweepCgroupRunner: a timing line carries the difference of the two reads its command was bracketed by; a nil or failing sampler writes no cgroup key and the package still passes (gt-d9eetx).
func TestSweepCgroupRunner(t *testing.T) {
	t.Parallel()
	start := CgroupSample{CPUUS: 100, ThrottledUS: 10, CPUStallUS: 5, IOStallUS: 6, MemStallUS: 7}
	last := CgroupSample{CPUUS: 400, ThrottledUS: 30, CPUStallUS: 15, IOStallUS: 16, MemStallUS: 7}
	spent := CgroupSample{CPUUS: 300, ThrottledUS: 20, CPUStallUS: 10, IOStallUS: 10}
	for _, tc := range []struct {
		name      string
		nilSample bool
		failAt    int
		want      *CgroupSample
	}{
		{"difference", false, 0, &spent},
		{"nil sample", true, 0, nil},
		{"first read fails", false, 1, nil},
		{"second read fails", false, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeCounters{start: start, last: last, failAt: tc.failAt}
			logDir := t.TempDir()
			exec := func(string, []string, []string, io.Writer) int { fake.ran = true; return 0 }
			r := &Runner{Exec: exec, LogDir: logDir, Timeout: "7m", Stderr: io.Discard,
				Timings: OpenTimings(filepath.Join(logDir, TimingsName), ProcessHost, io.Discard)}
			if !tc.nilSample {
				r.Sample = fake.sample
			}
			if got := r.Tier(TierIntegration, ".", nil, nil, []string{"./internal/p"}); got.Passed != 1 || got.Failed != 0 {
				t.Errorf("Tier() = %+v, want one pass", got)
			}
			line := strings.TrimSuffix(string(r.Timings.Bytes()), "\n")
			var tm Timing
			if err := json.Unmarshal([]byte(line), &tm); err != nil {
				t.Fatalf("unmarshal %q: %v", line, err)
			}
			if got := strings.Contains(line, `"cgroup"`); got != (tc.want != nil) || !reflect.DeepEqual(tm.Cgroup, tc.want) {
				t.Errorf("line %q: cgroup = %+v, want %+v", line, tm.Cgroup, tc.want)
			}
		})
	}
}
