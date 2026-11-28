package hostload

import (
	"strconv"
	"strings"
	"time"
)

// CPURecord is one reading of the Mac's CPU: the idle share and memory from
// top and each consumer's cumulative CPU seconds from ps, nil or absent when
// its read failed (gt-04wzw4).
type CPURecord struct {
	IdlePct         *float64
	UsedBytes       *int64
	CompressorBytes *int64
	Seconds         map[string]float64
}

// CPULine renders record as now's RFC3339 time, then idle, used and
// compressor memory, then the CPU seconds of sbx, control, docker and
// scanner; a nil or absent reading prints '-', never 0.
func CPULine(now time.Time, r CPURecord) string {
	parts := []string{
		now.Format(time.RFC3339),
		"idle=" + pct(r.IdlePct),
		"mem_used=" + byteText(r.UsedBytes),
		"mem_compressor=" + byteText(r.CompressorBytes),
	}
	for _, name := range []string{"sbx", "control", "docker", "scanner"} {
		text := "-"
		if sec, ok := r.Seconds[name]; ok {
			text = strconv.FormatFloat(sec, 'f', 2, 64)
		}
		parts = append(parts, name+"_cpu_s="+text)
	}
	return strings.Join(parts, " ")
}

// vmPaths are the file path each VM's process holds, in match order, so a
// process two VMs could claim is named by the first (gt-04wzw4).
var vmPaths = []struct{ name, path string }{
	{"sbx", "/.colima/_lima/colima-gt-sbx/"},
	{"control", "/.colima/_lima/colima-gt-control/"},
	{"docker", "/com.docker.docker/Data/vms/"},
}

// VMName names the VM whose files lsofText holds open: sbx, control or
// docker, or "" when it holds none of their paths (gt-04wzw4).
func VMName(lsofText string) string {
	for _, vm := range vmPaths {
		if strings.Contains(lsofText, vm.path) {
			return vm.name
		}
	}
	return ""
}

func pct(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func byteText(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}
