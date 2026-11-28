package tiersweep

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// cgroupFiles serves a container's four cgroup files and the machine's /proc/stat; a full pressure line's total (999) differs from its some line's, and user_usec (700) from system_usec (300), so reading the wrong line fails.
func cgroupFiles(override, text string) func(string) ([]byte, error) {
	files := map[string]string{
		"cpu.stat":        "usage_usec 1000\nuser_usec 700\nsystem_usec 300\nthrottled_usec 40\n",
		"cpu.pressure":    "some avg10=1.00 total=111\nfull avg10=9.00 total=999\n",
		"io.pressure":     "some avg10=1.00 total=222\nfull avg10=9.00 total=999\n",
		"memory.pressure": "some avg10=1.00 total=333\nfull avg10=9.00 total=999\n",
		"stat":            "cpu 1 2 3 4\nprocs_running 2\nprocesses 5000\n",
	}
	if override != "" && text == "" {
		delete(files, override)
	} else if override != "" {
		files[override] = text
	}
	return func(path string) ([]byte, error) {
		body, ok := files[filepath.Base(path)]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(body), nil
	}
}

func TestSweepCgroupRead(t *testing.T) {
	t.Parallel()
	t.Run("values and errors", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name  string
			files func(string) ([]byte, error)
			want  CgroupSample
			file  string // the file the error names; empty means success
			cause error
		}{
			{"all eight counters", cgroupFiles("", ""), CgroupSample{CPUUS: 1000, UserUS: 700, SystemUS: 300, ThrottledUS: 40, CPUStallUS: 111, IOStallUS: 222, MemStallUS: 333, NodeProcs: 5000}, "", nil},
			{"unreadable file", cgroupFiles("cpu.stat", ""), CgroupSample{}, "cpu.stat", fs.ErrNotExist},
			{"missing usage_usec", cgroupFiles("cpu.stat", "throttled_usec 40\n"), CgroupSample{}, "cpu.stat", nil},
			{"missing user_usec", cgroupFiles("cpu.stat", "usage_usec 1000\nsystem_usec 300\nthrottled_usec 40\n"), CgroupSample{}, "cpu.stat", nil},
			{"missing system_usec", cgroupFiles("cpu.stat", "usage_usec 1000\nuser_usec 700\nthrottled_usec 40\n"), CgroupSample{}, "cpu.stat", nil},
			{"missing throttled_usec", cgroupFiles("cpu.stat", "usage_usec 1000\nuser_usec 700\nsystem_usec 300\n"), CgroupSample{}, "cpu.stat", nil},
			{"usage_usec not a number", cgroupFiles("cpu.stat", "usage_usec soon\nthrottled_usec 40\n"), CgroupSample{}, "cpu.stat", nil},
			{"missing some line", cgroupFiles("io.pressure", "full avg10=9.00 total=999\n"), CgroupSample{}, "io.pressure", nil},
			{"some total not a number", cgroupFiles("memory.pressure", "some total=soon\n"), CgroupSample{}, "memory.pressure", nil},
			{"unreadable proc stat", cgroupFiles("stat", ""), CgroupSample{}, "/proc/stat", fs.ErrNotExist},
			{"missing processes", cgroupFiles("stat", "cpu 1 2 3 4\nprocs_running 2\n"), CgroupSample{}, "/proc/stat", nil},
			{"processes not a number", cgroupFiles("stat", "processes soon\n"), CgroupSample{}, "/proc/stat", nil},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got, err := ReadCgroup(tc.files, "container")
				if tc.file == "" {
					if err != nil || got != tc.want {
						t.Fatalf("ReadCgroup = %+v, %v; want %+v", got, err, tc.want)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.file) || got != (CgroupSample{}) || (tc.cause != nil && !errors.Is(err, tc.cause)) {
					t.Fatalf("ReadCgroup = %+v, %v; want an error naming %s, wrapping its cause, and no sample", got, err, tc.file)
				}
			})
		}
	})
	t.Run("since", func(t *testing.T) {
		t.Parallel()
		start := CgroupSample{CPUUS: 100, UserUS: 60, SystemUS: 40, ThrottledUS: 10, CPUStallUS: 5, IOStallUS: 6, MemStallUS: 7, NodeProcs: 500}
		end := CgroupSample{CPUUS: 400, UserUS: 250, SystemUS: 150, ThrottledUS: 30, CPUStallUS: 15, IOStallUS: 16, MemStallUS: 7, NodeProcs: 900}
		if got, want := end.Since(start), (CgroupSample{CPUUS: 300, UserUS: 190, SystemUS: 110, ThrottledUS: 20, CPUStallUS: 10, IOStallUS: 10, NodeProcs: 400}); got != want {
			t.Errorf("Since = %+v, want %+v", got, want)
		}
	})
}
