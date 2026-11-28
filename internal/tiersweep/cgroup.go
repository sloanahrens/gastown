package tiersweep

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupSample is one read of the container's cgroup counters: CPU time used and throttled and the user and system split of that use, the time tasks spent stalled on CPU, IO and memory, in microseconds, and the machine's process count.
type CgroupSample struct {
	CPUUS       int64 `json:"cpu_us"`
	UserUS      int64 `json:"user_us"`
	SystemUS    int64 `json:"system_us"`
	ThrottledUS int64 `json:"throttled_us"`
	CPUStallUS  int64 `json:"cpu_stall_us"`
	IOStallUS   int64 `json:"io_stall_us"`
	MemStallUS  int64 `json:"memory_stall_us"`
	// NodeProcs is the processes line of /proc/stat: every process and thread the whole machine has started, not this container's alone.
	NodeProcs int64 `json:"node_procs"`
}

// Since returns each counter's growth from start to s.
func (s CgroupSample) Since(start CgroupSample) CgroupSample {
	return CgroupSample{
		CPUUS:       s.CPUUS - start.CPUUS,
		UserUS:      s.UserUS - start.UserUS,
		SystemUS:    s.SystemUS - start.SystemUS,
		ThrottledUS: s.ThrottledUS - start.ThrottledUS,
		CPUStallUS:  s.CPUStallUS - start.CPUStallUS,
		IOStallUS:   s.IOStallUS - start.IOStallUS,
		MemStallUS:  s.MemStallUS - start.MemStallUS,
		NodeProcs:   s.NodeProcs - start.NodeProcs,
	}
}

// procStatFile is the machine's process counters, which are not under the container's cgroup directory.
const procStatFile = "/proc/stat"

// ReadCgroup reads the container's cpu.stat usage, user, system and throttle counters, the total of the some line in its cpu, io and memory pressure files under dir, and the machine-wide processes line of /proc/stat. A file that cannot be read, a missing key or a value that is not a number is an error naming the file, and no partial sample is returned.
func ReadCgroup(readFile func(string) ([]byte, error), dir string) (CgroupSample, error) {
	names := []string{"cpu.stat", "cpu.pressure", "io.pressure", "memory.pressure"}
	files := make(map[string]string, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := readFile(path)
		if err != nil {
			return CgroupSample{}, fmt.Errorf("%s: %w", path, err)
		}
		files[name] = string(data)
	}
	procStat, err := readFile(procStatFile)
	if err != nil {
		return CgroupSample{}, fmt.Errorf("%s: %w", procStatFile, err)
	}
	var s CgroupSample
	for i, dst := range []*int64{&s.CPUStallUS, &s.IOStallUS, &s.MemStallUS} {
		total, err := cgroupSomeTotal(files[names[i+1]], names[i+1])
		if err != nil {
			return CgroupSample{}, err
		}
		*dst = total
	}
	if s.CPUUS, err = cgroupKey(files["cpu.stat"], "usage_usec"); err != nil {
		return CgroupSample{}, err
	}
	if s.UserUS, err = cgroupKey(files["cpu.stat"], "user_usec"); err != nil {
		return CgroupSample{}, err
	}
	if s.SystemUS, err = cgroupKey(files["cpu.stat"], "system_usec"); err != nil {
		return CgroupSample{}, err
	}
	if s.ThrottledUS, err = cgroupKey(files["cpu.stat"], "throttled_usec"); err != nil {
		return CgroupSample{}, err
	}
	if s.NodeProcs, err = cgroupCount(string(procStat), procStatFile, "processes", "processes"); err != nil {
		return CgroupSample{}, err
	}
	return s, nil
}

// cgroupKey reads cpu.stat's key line as microseconds; a missing key and a value that is not a count are errors naming the file.
func cgroupKey(text, key string) (int64, error) {
	return cgroupCount(text, "cpu.stat", key, "microseconds")
}

// cgroupCount reads a stat file's key line as a non-negative integer counting unit; a missing key and a value that is not a number are errors naming the file.
func cgroupCount(text, file, key, unit string) (int64, error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != key {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s: %s %s is not a count of %s", file, key, fields[1], unit)
		}
		return n, nil
	}
	return 0, fmt.Errorf("%s: no %s line", file, key)
}

// cgroupSomeTotal reads a pressure file's some line total as microseconds; a line without one and a total that is not a count are errors naming the file.
func cgroupSomeTotal(text, file string) (int64, error) {
	total := ""
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if value, ok := strings.CutPrefix(field, "total="); ok {
				total = value
			}
		}
	}
	usec, err := strconv.ParseInt(total, 10, 64)
	if err != nil || usec < 0 {
		return 0, fmt.Errorf("%s: no some line with a numeric total", file)
	}
	return usec, nil
}
