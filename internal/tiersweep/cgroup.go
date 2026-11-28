package tiersweep

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// CgroupSample is one read of the container's cgroup counters, in microseconds: CPU time used and throttled, and the time tasks spent stalled on CPU, IO and memory.
type CgroupSample struct {
	CPUUS       int64 `json:"cpu_us"`
	ThrottledUS int64 `json:"throttled_us"`
	CPUStallUS  int64 `json:"cpu_stall_us"`
	IOStallUS   int64 `json:"io_stall_us"`
	MemStallUS  int64 `json:"memory_stall_us"`
}

// Since returns each counter's growth from start to s.
func (s CgroupSample) Since(start CgroupSample) CgroupSample {
	return CgroupSample{s.CPUUS - start.CPUUS, s.ThrottledUS - start.ThrottledUS, s.CPUStallUS - start.CPUStallUS, s.IOStallUS - start.IOStallUS, s.MemStallUS - start.MemStallUS}
}

// ReadCgroup reads the container's cpu.stat usage and throttle counters and the total of the some line in its cpu, io and memory pressure files under dir. A file that cannot be read, a missing key or a value that is not a number is an error naming the file, and no partial sample is returned.
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
	var s CgroupSample
	for i, dst := range []*int64{&s.CPUStallUS, &s.IOStallUS, &s.MemStallUS} {
		total, err := cgroupSomeTotal(files[names[i+1]], names[i+1])
		if err != nil {
			return CgroupSample{}, err
		}
		*dst = total
	}
	var err error
	if s.CPUUS, err = cgroupKey(files["cpu.stat"], "cpu.stat", "usage_usec"); err != nil {
		return CgroupSample{}, err
	}
	if s.ThrottledUS, err = cgroupKey(files["cpu.stat"], "cpu.stat", "throttled_usec"); err != nil {
		return CgroupSample{}, err
	}
	return s, nil
}

// cgroupKey reads a stat file's key line as microseconds; a missing key and a value that is not a count are errors naming the file.
func cgroupKey(text, file, key string) (int64, error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != key {
			continue
		}
		usec, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || usec < 0 {
			return 0, fmt.Errorf("%s: %s %s is not a count of microseconds", file, key, fields[1])
		}
		return usec, nil
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
