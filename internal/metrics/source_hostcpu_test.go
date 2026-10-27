package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The fixtures are ps output in the shapes the operator's Mac prints, with the
// process ids and paths replaced. Every cumulative time is a binary fraction,
// so the two samples' differences are exact and a cores column compares
// without a tolerance.

// hostCPUSample is one sample's two tables: the names and cumulative CPU
// times, and the command lines the two VMs are told apart by.
type hostCPUSample struct {
	names, args string
}

var hostCPUSampleOne = hostCPUSample{
	names: `  100   0:05.00 /System/Library/CoreServices/WindowServer
  200   0:00.00 /Applications/Docker.app/Contents/MacOS/com.docker.virtualization
  300   1:02:03.40 /System/Library/Frameworks/Virtualization.framework/Virtualization.VirtualMachine
  400  12:34.50 /opt/homebrew/bin/dolt
  500   0:00.10 /opt/homebrew/bin/gt
  600   0:00.50 /Library/Bitdefender/BDLDaemon
  700   0:10.00 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
  701   0:20.00 /Applications/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper
`,
	args: `  200 /Applications/Docker.app/Contents/MacOS/com.docker.virtualization --vmm virtualization
  300 /System/Library/Frameworks/Virtualization.framework/Virtualization.VirtualMachine --disk /Users/sloan/.colima/gt-sbx/_lima/colima/diffdisk
  400 /opt/homebrew/bin/dolt sql-server --config /Users/sloan/.dolt/config.json
  500 /opt/homebrew/bin/gt status --json
  700 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
  701 /Applications/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper --type=renderer
`,
}

var hostCPUSampleTwo = hostCPUSample{
	names: `  100   0:05.25 /System/Library/CoreServices/WindowServer
  200   0:00.50 /Applications/Docker.app/Contents/MacOS/com.docker.virtualization
  300   1:02:03.40 /System/Library/Frameworks/Virtualization.framework/Virtualization.VirtualMachine
  400  12:36.00 /opt/homebrew/bin/dolt
  501   0:00.25 /opt/homebrew/bin/gt
  700   0:10.50 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
  701   0:20.25 /Applications/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper
  702   0:00.25 /Applications/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper
`,
	args: `  200 /Applications/Docker.app/Contents/MacOS/com.docker.virtualization --vmm virtualization
  300 /System/Library/Frameworks/Virtualization.framework/Virtualization.VirtualMachine --disk /Users/sloan/.colima/gt-sbx/_lima/colima/diffdisk
  400 /opt/homebrew/bin/dolt sql-server --config /Users/sloan/.dolt/config.json
  501 /opt/homebrew/bin/gt mail inbox
  700 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
  701 /Applications/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper --type=renderer
  702 /Applications/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper --type=gpu-process
`,
}

// hostCPUFake answers each sample's two tables by the command that reads them
// and moves its clock only when the source waits, by the window the test set:
// the interval Collect divides by is the fake clock's, never the wall clock's.
type hostCPUFake struct {
	samples []hostCPUSample
	// fail is a command whose read fails outright.
	fail map[string]error
	// failAt is the read number (1 based) of a command that fails while the
	// other reads of it answer, so a failure in the second sample is its own.
	failAt map[string]int
	window time.Duration
	at     time.Time
	// ran is every command the source read with, in order.
	ran []string
	// waited is the delay the source asked for between the samples.
	waited time.Duration
}

func hostCPUFakeEnv(samples ...hostCPUSample) *hostCPUFake {
	return &hostCPUFake{
		samples: samples,
		fail:    map[string]error{},
		failAt:  map[string]int{},
		window:  time.Second,
		at:      time.Date(2026, 10, 26, 17, 7, 0, 0, time.UTC),
	}
}

func (f *hostCPUFake) env() Env {
	reads := map[string]int{}
	return Env{
		Run: func(_ context.Context, argv []string) ([]byte, error) {
			key := strings.Join(argv, " ")
			f.ran = append(f.ran, key)
			i := reads[key]
			reads[key]++
			if err := f.fail[key]; err != nil {
				return nil, err
			}
			if f.failAt[key] == i+1 {
				return nil, errors.New("ps failed")
			}
			var table string
			switch key {
			case strings.Join(hostCPUNameArgv, " "):
				if i >= len(f.samples) {
					return nil, errors.New("the source read the process names more times than the fake answers")
				}
				table = f.samples[i].names
			case strings.Join(hostCPUArgsArgv, " "):
				if i >= len(f.samples) {
					return nil, errors.New("the source read the command lines more times than the fake answers")
				}
				table = f.samples[i].args
			default:
				return nil, fmt.Errorf("the source ran unexpected command %q", key)
			}
			return []byte(table), nil
		},
		Now: func() time.Time { return f.at },
		Wait: func(d time.Duration) {
			f.waited = d
			f.at = f.at.Add(f.window)
		},
	}
}

// assertHostCPUColumns compares a group's columns with the want map, comparing
// numbers by value and requiring the same set of names.
func assertHostCPUColumns(t *testing.T, got, want map[string]any) {
	t.Helper()
	for name, wantValue := range want {
		gotValue, ok := got[name]
		if !ok {
			t.Errorf("column %s is missing", name)
			continue
		}
		wantNumber, isNumber := wantValue.(float64)
		if !isNumber {
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Errorf("%s = %v, want %v", name, gotValue, wantValue)
			}
			continue
		}
		gotNumber, ok := gotValue.(float64)
		if !ok {
			t.Errorf("%s = %v, want %v", name, gotValue, wantValue)
			continue
		}
		if math.Abs(gotNumber-wantNumber) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, gotNumber, wantNumber)
		}
	}
	for name, gotValue := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected column %s = %v", name, gotValue)
		}
	}
}

// The five named consumers and the three ranked names, each from the
// difference of a process's cumulative CPU time between the two samples:
// Dolt's 1.5 s over the one-second window is 1.5 cores, the zero-change VM is
// a reading of 0.0, and the scanner whose process exited reads nil.
func TestHostCPUCollectsCoresForEveryConsumer(t *testing.T) {
	t.Parallel()
	fake := hostCPUFakeEnv(hostCPUSampleOne, hostCPUSampleTwo)

	got, err := hostCPUSource{}.Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	assertHostCPUColumns(t, got, map[string]any{
		"polecat_vm_cores": 0.0,
		"docker_vm_cores":  0.5,
		"dolt_cores":       1.5,
		"scanner_cores":    nil,
		"gt_cores":         0.25,
		"other_1_name":     "Google Chrome",
		"other_1_cores":    0.5,
		"other_2_name":     "Google Chrome Helper",
		"other_2_cores":    0.5,
		"other_3_name":     "WindowServer",
		"other_3_cores":    0.25,
		"window_s":         1.0,
		"unmeasured_pids":  2,
		"regressed_pids":   0,
	})
	nameRead := strings.Join(hostCPUNameArgv, " ")
	argsRead := strings.Join(hostCPUArgsArgv, " ")
	want := []string{nameRead, argsRead, nameRead, argsRead}
	if !reflect.DeepEqual(fake.ran, want) {
		t.Errorf("the source ran %q, want %q", fake.ran, want)
	}
	if fake.waited != hostCPUWindow {
		t.Errorf("the source waited %v, want %v", fake.waited, hostCPUWindow)
	}
}

// A consumer with no process to measure is nil, and one whose process is there
// with no change is 0.0: a stopped scanner and an idle Dolt must not read
// alike.
func TestHostCPUAConsumerWithNoReadingIsNilNotZero(t *testing.T) {
	t.Parallel()
	fake := hostCPUFakeEnv(
		hostCPUSample{
			names: "  400  12:34.50 /opt/homebrew/bin/dolt\n  600   0:00.50 /Library/Bitdefender/BDLDaemon\n",
			args:  "  400 /opt/homebrew/bin/dolt sql-server\n",
		},
		hostCPUSample{
			names: "  400  12:34.50 /opt/homebrew/bin/dolt\n",
			args:  "  400 /opt/homebrew/bin/dolt sql-server\n",
		},
	)

	got, err := hostCPUSource{}.Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	assertHostCPUColumns(t, got, map[string]any{
		"polecat_vm_cores": nil,
		"docker_vm_cores":  nil,
		"dolt_cores":       0.0,
		"scanner_cores":    nil,
		"gt_cores":         nil,
		"other_1_name":     nil,
		"other_1_cores":    nil,
		"other_2_name":     nil,
		"other_2_cores":    nil,
		"other_3_name":     nil,
		"other_3_cores":    nil,
		"window_s":         1.0,
		"unmeasured_pids":  1,
		"regressed_pids":   0,
	})
}

// An idle host ranks none: a process that is there but consumed nothing over
// the window is not one of the three busiest consumers.
func TestHostCPUAnIdleHostRanksNoOne(t *testing.T) {
	t.Parallel()
	sample := hostCPUSample{
		names: "  800   0:07.00 /usr/sbin/notifyd\n",
		args:  "  800 /usr/sbin/notifyd\n",
	}
	fake := hostCPUFakeEnv(sample, sample)

	got, err := hostCPUSource{}.Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	assertHostCPUColumns(t, got, map[string]any{
		"polecat_vm_cores": nil,
		"docker_vm_cores":  nil,
		"dolt_cores":       nil,
		"scanner_cores":    nil,
		"gt_cores":         nil,
		"other_1_name":     nil,
		"other_1_cores":    nil,
		"other_2_name":     nil,
		"other_2_cores":    nil,
		"other_3_name":     nil,
		"other_3_cores":    nil,
		"window_s":         1.0,
		"unmeasured_pids":  0,
		"regressed_pids":   0,
	})
}

// The window is the clock's, not the constant the source waits for: the cores
// are the difference divided by the interval the two samples were taken over.
func TestHostCPUDividesByTheMeasuredWindow(t *testing.T) {
	t.Parallel()
	fake := hostCPUFakeEnv(
		hostCPUSample{
			names: "  400  12:34.50 /opt/homebrew/bin/dolt\n",
			args:  "  400 /opt/homebrew/bin/dolt sql-server\n",
		},
		hostCPUSample{
			names: "  400  12:36.00 /opt/homebrew/bin/dolt\n",
			args:  "  400 /opt/homebrew/bin/dolt sql-server\n",
		},
	)
	fake.window = 2 * time.Second

	got, err := hostCPUSource{}.Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["dolt_cores"] != 0.75 {
		t.Errorf("dolt_cores = %v, want 0.75 over the two-second window", got["dolt_cores"])
	}
	if got["window_s"] != 2.0 {
		t.Errorf("window_s = %v, want 2", got["window_s"])
	}
}

// A cumulative time that fell between samples is a reused process id: neither
// process's total is a reading of this window's other one, so the pair is
// counted and the consumer reads nil.
func TestHostCPUAPidReusedBetweenSamplesIsNoReading(t *testing.T) {
	t.Parallel()
	fake := hostCPUFakeEnv(
		hostCPUSample{
			names: "  400  12:00.00 /opt/homebrew/bin/dolt\n",
			args:  "  400 /opt/homebrew/bin/dolt sql-server\n",
		},
		hostCPUSample{
			names: "  400   0:00.05 /opt/homebrew/bin/dolt\n",
			args:  "  400 /opt/homebrew/bin/dolt sql-server\n",
		},
	)

	got, err := hostCPUSource{}.Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["dolt_cores"] != nil {
		t.Errorf("dolt_cores = %v, want nil for a reused process id", got["dolt_cores"])
	}
	if got["regressed_pids"] != 1 {
		t.Errorf("regressed_pids = %v, want 1", got["regressed_pids"])
	}
}

// A read that fails or prints a table the parser cannot use nulls the whole
// group with an error: a group of nils would read as a host with nothing
// running.
func TestHostCPUAnUnreadableProcessTableFailsTheGroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		fail   string
		later  bool
		first  hostCPUSample
		noWait bool
	}{
		{name: "the names cannot be read", fail: "names", first: hostCPUSampleOne},
		{name: "the second sample's names cannot be read", fail: "names", later: true, first: hostCPUSampleOne},
		{name: "the command lines cannot be read", fail: "args", first: hostCPUSampleOne},
		{name: "a name line's CPU time is not one", first: hostCPUSample{names: "  400  12x:34.50 /opt/homebrew/bin/dolt\n", args: hostCPUSampleOne.args}},
		{name: "the name table is not a table", first: hostCPUSample{names: "PID TIME COMM\n", args: hostCPUSampleOne.args}},
		{name: "the name table is empty", first: hostCPUSample{names: "\n", args: hostCPUSampleOne.args}},
		{name: "a name line names no program", first: hostCPUSample{names: "  400  12:34.50\n", args: hostCPUSampleOne.args}},
		{name: "the command-line table is not a table", first: hostCPUSample{names: hostCPUSampleOne.names, args: "PID ARGS\n"}},
		{name: "the command-line table is empty", first: hostCPUSample{names: hostCPUSampleOne.names, args: "\n"}},
		{name: "the samples share a timestamp", first: hostCPUSampleOne, noWait: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := hostCPUFakeEnv(tc.first, hostCPUSampleTwo)
			switch tc.fail {
			case "names":
				if tc.later {
					fake.failAt[strings.Join(hostCPUNameArgv, " ")] = 2
				} else {
					fake.fail[strings.Join(hostCPUNameArgv, " ")] = errors.New("ps failed")
				}
			case "args":
				fake.fail[strings.Join(hostCPUArgsArgv, " ")] = errors.New("ps failed")
			}
			if tc.noWait {
				fake.window = 0
			}

			got, err := hostCPUSource{}.Collect(context.Background(), fake.env())
			if err == nil {
				t.Fatalf("Collect = %v, want an error", got)
			}
			if got != nil {
				t.Errorf("Collect returned %v beside its error, want no columns", got)
			}
		})
	}
}

// A source that samples cannot take its window without the wait seam, and an
// Env built without it is an error rather than a zero-length window.
func TestHostCPUWithNoWaitSeamFailsTheGroup(t *testing.T) {
	t.Parallel()
	env := hostCPUFakeEnv(hostCPUSampleOne, hostCPUSampleTwo).env()
	env.Wait = nil

	got, err := hostCPUSource{}.Collect(context.Background(), env)
	if err == nil {
		t.Fatalf("Collect = %v, want an error", got)
	}
	if got != nil {
		t.Errorf("Collect returned %v beside its error, want no columns", got)
	}
}

// Every host source reads cumulative CPU time, never ps's pcpu, which is a
// decayed average. The check pins the CPU group's reads and walks the host
// group's probes, so a command added to either is covered here.
func TestHostCPUSourcesReadNoCPUPercent(t *testing.T) {
	t.Parallel()
	want := [][]string{
		{"ps", "-axo", "pid=,time=,comm="},
		{"ps", "-axo", "pid=,args="},
	}
	got := [][]string{hostCPUNameArgv, hostCPUArgsArgv}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the CPU group's reads = %q, want %q", got, want)
	}
	argvs := append([][]string{}, got...)
	for _, probe := range newHostSource().probes() {
		argvs = append(argvs, probe.argv)
	}
	for _, argv := range argvs {
		for _, arg := range argv {
			if strings.Contains(arg, "pcpu") {
				t.Errorf("%q reads ps pcpu, want cumulative CPU time (time)", strings.Join(argv, " "))
			}
		}
	}
}

// The parser reads every shape ps prints TIME in, and a shape it does not know
// is an error rather than zero CPU.
func TestParseHostCPUTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		field   string
		want    float64
		wantErr bool
	}{
		{"macOS hundredths", "0:00.05", 0.05, false},
		{"macOS minutes and seconds", "12:34.50", 754.5, false},
		{"macOS hours", "1:02:03.40", 3723.4, false},
		{"days", "2-01:02:03", 176523, false},
		{"linux whole seconds", "00:00", 0, false},
		{"linux minutes", "03:07", 187, false},
		{"no colon", "5", 0, true},
		{"too many fields", "1:2:3:4", 0, true},
		{"not a number", "0:0x.05", 0, true},
		{"days that are not a number", "x-01:02:03", 0, true},
		{"empty", "", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseHostCPUTime(tc.field)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseHostCPUTime(%q) = %v, want an error", tc.field, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHostCPUTime(%q): %v", tc.field, err)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("parseHostCPUTime(%q) = %v, want %v", tc.field, got, tc.want)
			}
		})
	}
}
