package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// at is the fixed reading time; UTC keeps the day file's name and the line's
// stamp in step.
var at = time.Date(2026, 11, 20, 16, 24, 0, 0, time.UTC)

// sampler answers the sampler's reads from its fields; fail names the one step
// whose read returns an error instead, so one test drives one refusal.
type sampler struct {
	load, ncpu, top, ps string
	fail                string
}

func (s *sampler) read(_ time.Duration, name string, args ...string) (string, error) {
	step, text := name, ""
	switch name {
	case "sysctl":
		if args[1] == "hw.ncpu" {
			step, text = "hw.ncpu", s.ncpu
		} else {
			step, text = "vm.loadavg", s.load
		}
	case "top":
		text = s.top
	case "ps":
		text = s.ps
	}
	if step == s.fail {
		return "", errors.New("read failed")
	}
	return text, nil
}

// full answers every read a run makes with the samples; fail names the one step whose read fails instead (gt-phge44).
func full(fail string) *sampler {
	return &sampler{
		load: "{ 21.53 22.70 21.14 }\n",
		ncpu: "24\n",
		top:  topReadSample,
		ps:   procsSample,
		fail: fail,
	}
}

// TestSysctlRefusesACommandThatCannotStart covers the exec error return: a
// path that cannot exist fails before any process starts (gt-emjwp9).
func TestSysctlRefusesACommandThatCannotStart(t *testing.T) {
	t.Parallel()
	if out, err := sysctl(5*time.Second, "/nonexistent-gt-hostload-sysctl", "-n", "vm.loadavg"); err == nil || out != "" {
		t.Errorf("sysctl = %q, %v: want an error and no output", out, err)
	}
}

// TestHostLoadRunWritesBothLines: a full run writes the load line and the CPU line, and prints nothing (gt-fjznor).
func TestHostLoadRunWritesBothLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stderr bytes.Buffer
	if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, full("").read); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run = %d, stderr %q: want 0 and nothing", code, stderr.String())
	}
	load, err := os.ReadFile(filepath.Join(dir, "2026-11-20.log"))
	if want := "2026-11-20T16:24:00Z load1=21.53 load5=22.70 load15=21.14 ncpu=24\n"; err != nil || string(load) != want {
		t.Errorf("load file = %q, %v: want %q", load, err, want)
	}
	cpu, err := os.ReadFile(filepath.Join(dir, "cpu", "2026-11-20.log"))
	if want := "2026-11-20T16:24:00Z idle=6.68 mem_used=135291469824 mem_compressor=42949672960 sbx_cpu_s=- control_cpu_s=- docker_cpu_s=- scanner_cpu_s=125.25\n"; err != nil || string(cpu) != want {
		t.Errorf("cpu file = %q, %v: want %q", cpu, err, want)
	}
}

// checkLoudFailure pins a failed read: 1, the step in one stderr line, no file (gt-phge44).
func checkLoudFailure(t *testing.T, step string) {
	t.Helper()
	dir := t.TempDir()
	var stderr bytes.Buffer
	if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, full(step).read); code != 1 {
		t.Errorf("run = %d, want 1", code)
	}
	if want := "gt-hostload: " + step + ": "; !strings.HasPrefix(stderr.String(), want) || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("stderr = %q, want one line starting %q", stderr.String(), want)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("dir = %v, %v: want empty, so no day file", entries, err)
	}
}

// TestHostLoadRunFailsLoudOnLoad: a failing vm.loadavg read returns 1, names the step, writes no file (gt-phge44).
func TestHostLoadRunFailsLoudOnLoad(t *testing.T) {
	t.Parallel()
	checkLoudFailure(t, "vm.loadavg")
}

// TestHostLoadRunFailsLoudOnNcpu: a failing hw.ncpu read after a good load read returns 1, names the step, writes no file (gt-phge44).
func TestHostLoadRunFailsLoudOnNcpu(t *testing.T) {
	t.Parallel()
	checkLoudFailure(t, "hw.ncpu")
}

// TestHostLoadRunCPUErrorStillWritesBothLines: a failing top read still writes
// both lines, returns 1, and names the step once (gt-fjznor).
func TestHostLoadRunCPUErrorStillWritesBothLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := full("top")
	var stderr bytes.Buffer
	if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, s.read); code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if want := "gt-hostload: top: read failed\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	load, err := os.ReadFile(filepath.Join(dir, "2026-11-20.log"))
	if want := "2026-11-20T16:24:00Z load1=21.53 load5=22.70 load15=21.14 ncpu=24\n"; err != nil || string(load) != want {
		t.Errorf("load file = %q, %v: want %q", load, err, want)
	}
	cpu, err := os.ReadFile(filepath.Join(dir, "cpu", "2026-11-20.log"))
	if want := "2026-11-20T16:24:00Z idle=- mem_used=- mem_compressor=- sbx_cpu_s=- control_cpu_s=- docker_cpu_s=- scanner_cpu_s=125.25\n"; err != nil || string(cpu) != want {
		t.Errorf("cpu file = %q, %v: want %q", cpu, err, want)
	}
}

// TestHostLoadRunRefusesCPUAppend: a cpu path that cannot become a directory names the step and returns 1 (gt-fjznor).
func TestHostLoadRunRefusesCPUAppend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, full("").read); code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if want := "gt-hostload: cpu append: "; !strings.HasPrefix(stderr.String(), want) || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("stderr = %q, want one line starting %q", stderr.String(), want)
	}
}

// TestHostLoadRunRefuses: each failing step names itself in one stderr line,
// returns 1, and leaves no day file behind (gt-emjwp9).
func TestHostLoadRunRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	badLoad := full("")
	badLoad.load = "nonsense"
	badNcpu := full("")
	badNcpu.ncpu = "many"
	for _, c := range []struct {
		step string
		args []string
		read func(time.Duration, string, ...string) (string, error)
	}{
		{"flags", []string{"--nope"}, full("").read},
		{"vm.loadavg", []string{"--dir", dir}, full("vm.loadavg").read},
		{"vm.loadavg", []string{"--dir", dir}, badLoad.read},
		{"hw.ncpu", []string{"--dir", dir}, full("hw.ncpu").read},
		{"hw.ncpu", []string{"--dir", dir}, badNcpu.read},
		{"append", []string{"--dir", file}, full("").read},
	} {
		var stderr bytes.Buffer
		if code := run(c.args, &stderr, func() time.Time { return at }, c.read); code != 1 {
			t.Errorf("%s: run = %d, want 1", c.step, code)
		}
		if want := "gt-hostload: " + c.step + ": "; !strings.HasPrefix(stderr.String(), want) || strings.Count(stderr.String(), "\n") != 1 {
			t.Errorf("%s: stderr = %q, want one line starting %q", c.step, stderr.String(), want)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("dir = %v, %v: want only a-file, so no partial day file", entries, err)
	}
}
