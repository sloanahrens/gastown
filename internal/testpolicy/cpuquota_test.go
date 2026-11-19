package testpolicy

import (
	"errors"
	"io/fs"
	"testing"
)

// fakeRead serves the named cgroup files, reports the rest as missing, and
// fails a path in unreadable the way an unreadable file does.
func fakeRead(files map[string]string, unreadable ...string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		for _, p := range unreadable {
			if p == path {
				return nil, errors.New("permission denied")
			}
		}
		s, ok := files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(s), nil
	}
}

type quotaCase struct {
	name       string
	files      map[string]string
	unreadable []string
	env        map[string]string
	numCPU     int
	want       int
}

func runQuotaCases(t *testing.T, tests []quotaCase) {
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read := fakeRead(tt.files, tt.unreadable...)
			if got := CPUQuota(read, func(k string) string { return tt.env[k] }, tt.numCPU); got != tt.want {
				t.Errorf("CPUQuota() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestCPUQuota checks which source CPUQuota reports: the cgroup's quota
// before the runner's variable, and the variable before NumCPU.
func TestCPUQuota(t *testing.T) {
	t.Parallel()
	runQuotaCases(t, []quotaCase{
		{"cpu.max quota over period", map[string]string{cpuMaxPath: "600000 100000"}, nil, nil, 16, 6},
		{"cpu.max fractional quota rounds up", map[string]string{cpuMaxPath: "150000 100000"}, nil, nil, 16, 2},
		{"cpu.max beats the variable", map[string]string{cpuMaxPath: "400000 100000"}, nil, map[string]string{CPUQuotaEnv: "6"}, 16, 4},
		{"variable when cpu.max is max", map[string]string{cpuMaxPath: "max 100000"}, nil, map[string]string{CPUQuotaEnv: "6"}, 16, 6},
		{"numCPU when neither source limits", nil, nil, nil, 8, 8},
	})
}

// TestCPUQuotaFallsThrough checks that a source with no usable limit falls to
// the next one — the v1 files when cpu.max is absent, then the variable, then
// numCPU — and never to zero.
func TestCPUQuotaFallsThrough(t *testing.T) {
	t.Parallel()
	runQuotaCases(t, []quotaCase{
		{"unparseable variable", nil, nil, map[string]string{CPUQuotaEnv: "six"}, 8, 8},
		{"zero variable", nil, nil, map[string]string{CPUQuotaEnv: "0"}, 8, 8},
		{"negative variable", nil, nil, map[string]string{CPUQuotaEnv: "-3"}, 8, 8},
		{"unreadable cpu.max", nil, []string{cpuMaxPath}, nil, 8, 8},
		{"v1 quota when cpu.max is absent", map[string]string{cpuQuotaPath: "250000", cpuPeriodPath: "100000"}, nil, nil, 8, 3},
		{"v1 unlimited quota", map[string]string{cpuQuotaPath: "-1", cpuPeriodPath: "100000"}, nil, nil, 8, 8},
		{"v1 period unreadable", map[string]string{cpuQuotaPath: "250000"}, nil, nil, 8, 8},
	})
}
