package tiersweep

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestSweepCgroupSpan: end reports the growth between the two samples, a nil
// sample reports nothing, and a failed sample reports its error. A Timing with
// no Cgroup carries no cgroup key, and one with a Cgroup carries its counters.
func TestSweepCgroupSpan(t *testing.T) {
	t.Parallel()
	start := CgroupSample{CPUUS: 100, ThrottledUS: 10, CPUStallUS: 5, IOStallUS: 6, MemStallUS: 7}
	last := CgroupSample{CPUUS: 400, ThrottledUS: 30, CPUStallUS: 15, IOStallUS: 16, MemStallUS: 7}
	diff := CgroupSample{CPUUS: 300, ThrottledUS: 20, CPUStallUS: 10, IOStallUS: 10}
	sampleErr := errors.New("cpu.stat: no usage_usec line")
	tests := []struct {
		name     string
		samples  []CgroupSample
		errs     []error
		noSample bool
		want     *CgroupSample
		wantErr  error
		calls    int
	}{
		{"difference", []CgroupSample{start, last}, []error{nil, nil}, false, &diff, nil, 2},
		{"nil sample", nil, nil, true, nil, nil, 0},
		{"first sample fails", []CgroupSample{{}, {}}, []error{sampleErr, sampleErr}, false, nil, sampleErr, 1},
		{"second sample fails", []CgroupSample{start, {}}, []error{nil, sampleErr}, false, nil, sampleErr, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			var sample func() (CgroupSample, error)
			if !tc.noSample {
				sample = func() (CgroupSample, error) {
					if calls >= len(tc.samples) {
						return CgroupSample{}, errors.New("sample called past the case's table")
					}
					calls++
					return tc.samples[calls-1], tc.errs[calls-1]
				}
			}
			got, err := measureCgroup(sample)()
			switch {
			case !errors.Is(err, tc.wantErr), tc.want == nil && got != nil, tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("end() = %v, %v; want %+v and %v", got, err, tc.want, tc.wantErr)
			}
			if calls != tc.calls {
				t.Errorf("sample called %d times, want %d", calls, tc.calls)
			}
		})
	}
	t.Run("timing cgroup key", func(t *testing.T) {
		t.Parallel()
		if b, err := json.Marshal(Timing{}); err != nil || strings.Contains(string(b), "cgroup") {
			t.Fatalf("Timing with no Cgroup = %s, %v; want no cgroup key", b, err)
		}
		s := CgroupSample{CPUUS: 5}
		if b, err := json.Marshal(Timing{Cgroup: &s}); err != nil || !strings.Contains(string(b), `"cgroup":{"cpu_us":5`) {
			t.Fatalf("Timing with Cgroup = %s, %v; want its counters under a cgroup key", b, err)
		}
	})
}
