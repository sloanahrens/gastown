package hostload

import (
	"testing"
	"time"
)

// TestHostCPULine renders a full record, an empty one and a zero reading
// whole, so '-' marks only a value that was never read.
func TestHostCPULine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 16, 24, 0, 0, zone)
	zero := 0.0
	z := int64(0)
	idle, used, comp := 6.68, int64(126)<<30, int64(40)<<30
	for _, c := range []struct {
		rec  CPURecord
		want string
	}{
		{CPURecord{&idle, &used, &comp, map[string]float64{"sbx": 1234.5, "docker": 0, "scanner": 62074.27}},
			"2026-11-20T16:24:00-06:00 idle=6.68 mem_used=135291469824 mem_compressor=42949672960 sbx_cpu_s=1234.50 control_cpu_s=- docker_cpu_s=0.00 scanner_cpu_s=62074.27"},
		{CPURecord{},
			"2026-11-20T16:24:00-06:00 idle=- mem_used=- mem_compressor=- sbx_cpu_s=- control_cpu_s=- docker_cpu_s=- scanner_cpu_s=-"},
		{CPURecord{&zero, &z, &z, map[string]float64{"sbx": 0, "control": 0, "docker": 0, "scanner": 0}},
			"2026-11-20T16:24:00-06:00 idle=0.00 mem_used=0 mem_compressor=0 sbx_cpu_s=0.00 control_cpu_s=0.00 docker_cpu_s=0.00 scanner_cpu_s=0.00"},
	} {
		if got := CPULine(now, c.rec); got != c.want {
			t.Errorf("CPULine(%+v) = %q, want %q", c.rec, got, c.want)
		}
	}
}

// TestHostCPULineVMName names each VM path lsof shows and leaves any other
// text unnamed.
func TestHostCPULineVMName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ text, want string }{
		{"... /Users/me/.colima/_lima/colima-gt-sbx/disk", "sbx"},
		{"... /Users/me/.colima/_lima/colima-gt-control/serial.log", "control"},
		{"... /Users/me/Library/Containers/com.docker.docker/Data/vms/0/data/Docker.raw", "docker"},
		{"... /usr/lib/dyld", ""},
		{"", ""},
	} {
		if got := VMName(c.text); got != c.want {
			t.Errorf("VMName(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
