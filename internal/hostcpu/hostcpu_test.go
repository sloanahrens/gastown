package hostcpu

import "testing"

// The probe bodies below are the shape each top prints, cut down to the
// lines ParseIdle reads: two samples, the second the reading of now.
const (
	darwinTwoSamples = `Processes: 800 total, 4 running, 24 cores
CPU usage: 12.04% user, 24.61% sys, 63.35% idle
Processes: 800 total, 4 running, 24 cores
CPU usage: 23.14% user, 28.21% sys, 48.64% idle
`
	linuxTwoSamples = `top - 17:35:36 up 3 days,  2:14,  1 user,  load average: 1.20, 3.40, 5.60
%Cpu(s):  3.1 us,  9.4 sy,  0.0 ni, 87.5 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st
top - 17:35:37 up 3 days,  2:14,  1 user,  load average: 1.30, 3.40, 5.60
%Cpu(s):  6.2 us, 12.5 sy,  0.0 ni, 81.2 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st
`
)

func TestParseIdle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sample   string
		want     float64
		wantRead bool
	}{
		{"macOS keeps the second sample", darwinTwoSamples, 48.64, true},
		{"Linux keeps the second sample", linuxTwoSamples, 81.2, true},
		// The first sample is the cumulative reading since boot: 63% idle
		// over the host's uptime says nothing about this moment, so a probe
		// that returned only it has no reading to give.
		{"one sample is not a reading", "CPU usage: 12.04% user, 24.61% sys, 63.35% idle\n", 0, false},
		{"no CPU line at all", "Processes: 800 total, 4 running, 24 cores\n", 0, false},
		{"empty probe output", "", 0, false},
		{"idle share missing", darwinTwoSamples[:len(darwinTwoSamples)-1] + "CPU usage: 23.14% user, 28.21% sys\n", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseIdle(tt.sample)
			if ok != tt.wantRead || (ok && got != tt.want) {
				t.Errorf("ParseIdle(%q) = %v, %v; want %v, %v", tt.sample, got, ok, tt.want, tt.wantRead)
			}
		})
	}
}

// A Linux process row can carry the text ParseIdle looks for; the CPU line
// is the one that starts with it.
func TestParseIdleIgnoresProcessRows(t *testing.T) {
	t.Parallel()
	sample := linuxTwoSamples +
		" 1234 root      20   0  100m  10m   5m S  0.3  0.0   0:01.00 cpus(s)-helper cpu usage\n"
	got, ok := ParseIdle(sample)
	if !ok || got != 81.2 {
		t.Errorf("ParseIdle with a process row after the samples = %v, %v; want 81.2, true", got, ok)
	}
}
