package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// TestMemoryFootprintLine renders the status line's Memory: value: the
// footprint and peak when vmmap answered, and "unavailable" when it did not —
// never an error (gt-v1zyv).
func TestMemoryFootprintLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		metrics *doltserver.HealthMetrics
		want    string
	}{
		{
			name:    "footprint and peak",
			metrics: &doltserver.HealthMetrics{FootprintBytes: 1825361101, FootprintPeakBytes: 10952166605},
			want:    "1.7 GB (peak 10.2 GB)",
		},
		{
			name:    "footprint without a peak",
			metrics: &doltserver.HealthMetrics{FootprintBytes: 1825361101},
			want:    "1.7 GB",
		},
		{
			name:    "nothing read",
			metrics: &doltserver.HealthMetrics{},
			want:    "unavailable",
		},
		{
			name: "no metrics",
			want: "unavailable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := memoryFootprintLine(tc.metrics); got != tc.want {
				t.Errorf("memoryFootprintLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrintResourceMetrics shows the status block carries the Memory: line
// beside Disk usage:, both when the footprint was read and when it was not.
func TestPrintResourceMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		metrics *doltserver.HealthMetrics
		want    []string
	}{
		{
			name: "footprint beside disk usage",
			metrics: &doltserver.HealthMetrics{
				DiskUsageHuman:     "3.4 GB",
				FootprintBytes:     1825361101,
				FootprintPeakBytes: 10952166605,
			},
			want: []string{"Disk usage:    3.4 GB\n    Memory:        1.7 GB (peak 10.2 GB)\n"},
		},
		{
			name:    "unavailable footprint",
			metrics: &doltserver.HealthMetrics{DiskUsageHuman: "3.4 GB"},
			want:    []string{"Disk usage:    3.4 GB\n    Memory:        unavailable\n"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			printResourceMetrics(&out, tc.metrics)
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("status output does not carry %q:\n%s", want, out.String())
				}
			}
		})
	}
}
