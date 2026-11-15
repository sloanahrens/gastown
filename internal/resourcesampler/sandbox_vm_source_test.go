package resourcesampler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// vmProcFilesHealthy is the text of the five proc files one healthy gt-sbx VM
// read prints: four cores, 1500 of 10000 aggregate ticks busy, 8 GiB of 16 GiB
// in use, and a load and pressure a VM under light work reports.
func vmProcFilesHealthy() map[string]string {
	return map[string]string{
		"loadavg":         "0.52 0.58 0.59 1/1234 56789\n",
		"pressure/cpu":    "some avg10=1.50 avg60=0.50 avg300=0.10 total=999\n",
		"pressure/memory": "some avg10=2.25 avg60=1.00 avg300=0.50 total=888\nfull avg10=0.75 avg60=0.25 avg300=0.10 total=777\n",
		"stat":            vmStatText(4, 1500, 8500),
		"meminfo":         "MemTotal:       16384000 kB\nMemFree:        1638400 kB\nMemAvailable:   8192000 kB\n",
	}
}

// vmProcText renders the text one VM read prints: each file under a marker
// naming it, then the end marker. A file the test leaves out prints no section;
// a file mapped to the empty text prints its marker and no text, the way a read
// prints a file the kernel does not publish.
func vmProcText(files map[string]string) string {
	var text strings.Builder
	for _, name := range vmProcFiles {
		body, ok := files[name]
		if !ok {
			continue
		}
		text.WriteString("@@ " + name + " @@\n" + body)
	}
	text.WriteString("@@ end @@\n")
	return text.String()
}

// vmStatText is a /proc/stat with cores cores: the aggregate cpu line's busy
// ticks split across user and system, its idle ticks, and one all-zero line per
// core, which is what the builder counts the cores by.
func vmStatText(cores int, busy, idle uint64) string {
	var text strings.Builder
	fmt.Fprintf(&text, "cpu  %d 0 %d %d 0 0 0 0 0 0\n", busy/2, busy-busy/2, idle)
	for i := range cores {
		fmt.Fprintf(&text, "cpu%d 0 0 0 0 0 0 0 0 0 0\n", i)
	}
	text.WriteString("intr 0\nctxt 0\nbtime 1700000000\n")
	return text.String()
}

// vmSampleTime is the sample time the VM row tests run at.
func vmSampleTime() time.Time { return time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC) }

// TestSandboxVMProcRow is the bead's acceptance: fake proc text produces one VM
// row with the design row table's values, and cpu_pct is the busy share of two
// /proc/stat readings, in percent of one core (gt-9y0kii).
func TestSandboxVMProcRow(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	first := vmSampleTime()
	second := first.Add(60 * time.Second)

	firstRow, reading, err := VMRow(vmProcText(vmProcFilesHealthy()), first, nil, workspaces)
	if err != nil {
		t.Fatalf("the first sample: %v", err)
	}
	if want := (VMReading{Cores: 4, Busy: 1500, Total: 10000, At: first}); !reflect.DeepEqual(reading, want) {
		t.Errorf("the first sample's reading = %+v, want %+v", reading, want)
	}
	wantFirst := Row{
		V:                  1,
		TS:                 "2026-11-14T11:00:00Z",
		Source:             "sandbox",
		Class:              "sandbox",
		Component:          "vm",
		Name:               "gt-sbx",
		MemBytes:           (16384000 - 8192000) * 1024,
		Workspace:          "customer-zero",
		Cluster:            seatStringPtr("gt-sbx"),
		PressureCPUSomePct: seatFloatPtr(1.5),
		PressureMemSomePct: seatFloatPtr(2.25),
		PressureMemFullPct: seatFloatPtr(0.75),
		Load1:              seatFloatPtr(0.52),
	}
	if !reflect.DeepEqual(firstRow, wantFirst) {
		t.Errorf("the first sample's row = %+v, want %+v", firstRow, wantFirst)
	}

	// 1500 more busy ticks over 10000 more aggregate ticks on four cores is
	// 0.6 of a core, so 60 percent of one core.
	files := vmProcFilesHealthy()
	files["stat"] = vmStatText(4, 3000, 17000)
	secondRow, _, err := VMRow(vmProcText(files), second, &reading, workspaces)
	if err != nil {
		t.Fatalf("the second sample: %v", err)
	}
	wantSecond := wantFirst
	wantSecond.TS = "2026-11-14T11:01:00Z"
	wantSecond.IntervalS = 60
	wantSecond.CPUPct = seatFloatPtr(60)
	if !reflect.DeepEqual(secondRow, wantSecond) {
		t.Errorf("the second sample's row = %+v, want %+v", secondRow, wantSecond)
	}
}

// TestSandboxVMProcCPU is the bead's acceptance: the first tick has null
// cpu_pct and a later one a positive value, and the readings a previous sample
// cannot be subtracted from report null rather than an invented percentage
// (gt-9y0kii).
func TestSandboxVMProcCPU(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	start := vmSampleTime()
	previous := func(cores, busy, total uint64) *VMReading {
		return &VMReading{Cores: cores, Busy: busy, Total: total, At: start}
	}
	minute := start.Add(60 * time.Second)
	for _, test := range []struct {
		name         string
		cores        int
		busy         uint64
		idle         uint64
		now          time.Time
		previous     *VMReading
		wantInterval float64
		wantCPU      *float64
	}{
		{"the first sample", 4, 6000, 18000, start, nil, 0, nil},
		{"one core fully busy", 4, 6000, 18000, minute, previous(4, 0, 0), 60, seatFloatPtr(100)},
		{"every core fully busy", 4, 24000, 0, minute, previous(4, 0, 0), 60, seatFloatPtr(400)},
		{"a measured zero", 4, 0, 24000, minute, previous(4, 0, 0), 60, seatFloatPtr(0)},
		{"a counter that went backwards", 4, 0, 24000, minute, previous(4, 100, 5000), 60, nil},
		{"a total that did not advance", 4, 6000, 18000, minute, previous(4, 0, 24000), 60, nil},
		{"a core count that changed", 4, 6000, 18000, minute, previous(2, 0, 0), 60, nil},
		{"a clock that did not advance", 4, 6000, 18000, start, previous(4, 0, 0), 0, nil},
		{"a fractional share", 1, 1, 599, minute, previous(1, 0, 0), 60, seatFloatPtr(0.17)},
	} {
		files := vmProcFilesHealthy()
		files["stat"] = vmStatText(test.cores, test.busy, test.idle)
		row, _, err := VMRow(vmProcText(files), test.now, test.previous, workspaces)
		if err != nil {
			t.Errorf("%s: %v", test.name, err)
			continue
		}
		if row.IntervalS != test.wantInterval {
			t.Errorf("%s: interval_s = %v, want %v", test.name, row.IntervalS, test.wantInterval)
		}
		if !reflect.DeepEqual(row.CPUPct, test.wantCPU) {
			t.Errorf("%s: cpu_pct = %v, want %v", test.name, floatText(row.CPUPct), floatText(test.wantCPU))
		}
	}
}

// TestSandboxVMProcUnpublished covers the readings a kernel may not publish: a
// VM with no load average and no pressure files writes its row with those
// fields null, not with zeros (gt-9y0kii).
func TestSandboxVMProcUnpublished(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	files := vmProcFilesHealthy()
	files["loadavg"] = ""
	files["pressure/cpu"] = ""
	files["pressure/memory"] = ""
	row, _, err := VMRow(vmProcText(files), vmSampleTime(), nil, workspaces)
	if err != nil {
		t.Fatalf("a read of a kernel without PSI: %v", err)
	}
	want := Row{
		V:         1,
		TS:        "2026-11-14T11:00:00Z",
		Source:    "sandbox",
		Class:     "sandbox",
		Component: "vm",
		Name:      "gt-sbx",
		MemBytes:  (16384000 - 8192000) * 1024,
		Workspace: "customer-zero",
		Cluster:   seatStringPtr("gt-sbx"),
	}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("row = %+v, want %+v", row, want)
	}
}

// TestSandboxVMProcRefusals covers every text a VM read must refuse: each case
// writes no row rather than one of zeros (gt-9y0kii). Each row is one reason to
// fail.
func TestSandboxVMProcRefusals(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	healthy := vmProcText(vmProcFilesHealthy())
	with := func(name, body string) string {
		files := vmProcFilesHealthy()
		files[name] = body
		return vmProcText(files)
	}
	without := func(name string) string {
		files := vmProcFilesHealthy()
		delete(files, name)
		return vmProcText(files)
	}
	for _, test := range []struct {
		name  string
		text  string
		noMap bool
	}{
		{name: "an empty read"},
		{name: "text before the first marker", text: "0.52 0.58\n" + healthy},
		{name: "a marker naming no proc file", text: strings.Replace(healthy, "@@ meminfo @@", "@@ uptime @@", 1)},
		{name: "a section the VM does not have", text: strings.Replace(healthy, "@@ end @@\n", "@@ uptime @@\n1\n@@ end @@\n", 1)},
		{name: "a section printed twice", text: strings.Replace(healthy, "@@ end @@\n", "@@ stat @@\ncpu  1 0 1 1 0 0 0 0 0 0\n@@ end @@\n", 1)},
		{name: "text after the end marker", text: healthy + "trailing\n"},
		{name: "a marker after the end marker", text: healthy + "@@ loadavg @@\n"},
		{name: "a second end marker", text: healthy + "@@ end @@\n"},
		{name: "a read that ends before its end marker", text: strings.TrimSuffix(healthy, "@@ end @@\n")},
		{name: "a read with no stat", text: without("stat")},
		{name: "a read with no meminfo", text: without("meminfo")},
		{name: "an empty stat", text: with("stat", "")},
		{name: "a stat with no cpu line", text: with("stat", "intr 0\nctxt 0\n")},
		{name: "a stat naming no cpu", text: with("stat", "cpu  1 2 3 4 5 6 7 8 9 10\n")},
		{name: "a cpu line with no idle", text: with("stat", "cpu  1 2 3\ncpu0 0 0 0 0\n")},
		{name: "a cpu line that is not a tick count", text: with("stat", "cpu  1 2 3 soon 5 6 7 8\ncpu0 0 0 0 0\n")},
		{name: "a loadavg that is not a load", text: with("loadavg", "soon 0.58 0.59 1/1234 56789\n")},
		{name: "a negative load1", text: with("loadavg", "-1.00 0.58 0.59 1/1234 56789\n")},
		{name: "a NaN load1", text: with("loadavg", "NaN 0.58 0.59 1/1234 56789\n")},
		{name: "an infinite load1", text: with("loadavg", "+Inf 0.58 0.59 1/1234 56789\n")},
		{name: "a cpu pressure that is not pressure", text: with("pressure/cpu", "hello\n")},
		{name: "a cpu pressure with no some line", text: with("pressure/cpu", "full avg10=1.00\n")},
		{name: "a memory pressure with no full line", text: with("pressure/memory", "some avg10=2.25\n")},
		{name: "a memory pressure with an unreadable some avg10", text: with("pressure/memory", "some avg10=soon\nfull avg10=0.75\n")},
		{name: "a memory pressure with an unreadable full avg10", text: with("pressure/memory", "some avg10=2.25\nfull avg10=soon\n")},
		{name: "a meminfo with no MemTotal", text: with("meminfo", "MemFree: 100 kB\nMemAvailable: 50 kB\n")},
		{name: "a meminfo with no MemAvailable", text: with("meminfo", "MemTotal: 16384000 kB\nMemFree: 100 kB\n")},
		{name: "a MemTotal line with no unit", text: with("meminfo", "MemTotal: 16384000\nMemAvailable: 8192000 kB\n")},
		{name: "a MemTotal in another unit", text: with("meminfo", "MemTotal: 16384000 MB\nMemAvailable: 8192000 kB\n")},
		{name: "a MemTotal that is not a number", text: with("meminfo", "MemTotal: lots kB\nMemAvailable: 8192000 kB\n")},
		{name: "a MemAvailable larger than the MemTotal", text: with("meminfo", "MemTotal: 1000 kB\nMemAvailable: 2000 kB\n")},
		{name: "no workspace map", text: healthy, noMap: true},
	} {
		var workspaceMap *workspaceid.Map
		if !test.noMap {
			workspaceMap = workspaces
		}
		row, reading, err := VMRow(test.text, vmSampleTime(), nil, workspaceMap)
		if err == nil {
			t.Errorf("%s: no error, row %+v", test.name, row)
			continue
		}
		if !reflect.DeepEqual(row, Row{}) || !reflect.DeepEqual(reading, VMReading{}) {
			t.Errorf("%s: error with row %+v and reading %+v, want neither", test.name, row, reading)
		}
	}
}

// vmProcRead is an exec answer for one live seat's VM read: the proc text of
// files, under the markers the parser reads.
func vmProcRead(files map[string]string) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) {
		return vmProcText(files), nil
	}
}

// vmProcWithout is a healthy VM read with one file left out of the text
// entirely, the way a read that never printed the file looks.
func vmProcWithout(name string) map[string]string {
	files := vmProcFilesHealthy()
	delete(files, name)
	return files
}

// vmProcWith is a healthy VM read whose one file carries body.
func vmProcWith(name, body string) map[string]string {
	files := vmProcFilesHealthy()
	files[name] = body
	return files
}

// TestSandboxVMSourceTick is the bead's acceptance: one tick reads the five
// proc files from the one live seat through the exec seam and writes the VM
// row (gt-2qe2m6).
func TestSandboxVMSourceTick(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown", Polecat: "toast"})
	runner := &fakeSeatExec{read: vmProcRead(vmProcFilesHealthy())}

	tick, err := NewVMSource(townRoot, runner, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(tick.Skipped) != 0 {
		t.Errorf("skipped %+v, want no skip", tick.Skipped)
	}
	if !reflect.DeepEqual(runner.calls, []string{"mango-toast"}) {
		t.Errorf("seats read = %v, want the one live seat", runner.calls)
	}
	for i, argv := range runner.argv {
		if !reflect.DeepEqual(argv, vmProcReadArgv()) {
			t.Errorf("call %d ran %v, want the VM read's argv", i, argv)
		}
	}
	if tick.Row == nil {
		t.Fatal("the live seat wrote no VM row")
	}
	want := Row{
		V:                  1,
		TS:                 "2026-11-14T10:00:00Z",
		Source:             "sandbox",
		Class:              "sandbox",
		Component:          "vm",
		Name:               SandboxCluster,
		MemBytes:           (16384000 - 8192000) * 1024,
		Workspace:          "customer-zero",
		Cluster:            seatStringPtr(SandboxCluster),
		PressureCPUSomePct: seatFloatPtr(1.5),
		PressureMemSomePct: seatFloatPtr(2.25),
		PressureMemFullPct: seatFloatPtr(0.75),
		Load1:              seatFloatPtr(0.52),
	}
	if !reflect.DeepEqual(*tick.Row, want) {
		t.Errorf("row = %+v, want %+v", *tick.Row, want)
	}
}

// TestSandboxVMSourceTickCPU is the bead's acceptance: the first tick has null
// cpu_pct and a later one measures it against the VM's previous reading
// (gt-2qe2m6).
func TestSandboxVMSourceTickCPU(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown"})
	clock := &fakeClock{now: seatTickNow()}
	reads := 0
	runner := &fakeSeatExec{read: func(context.Context, string) (string, error) {
		reads++
		files := vmProcFilesHealthy()
		if reads == 1 {
			files["stat"] = vmStatText(4, 1500, 8500)
		} else {
			// 1500 more busy ticks of 10000 more on four cores: 60 percent of
			// one core.
			files["stat"] = vmStatText(4, 3000, 17000)
		}
		return vmProcText(files), nil
	}}
	source := NewVMSource(townRoot, runner, clock, time.Second)

	first, err := source.Tick(context.Background())
	if err != nil {
		t.Fatalf("the first tick: %v", err)
	}
	if first.Row == nil {
		t.Fatal("the first tick wrote no row")
	}
	if first.Row.CPUPct != nil || first.Row.IntervalS != 0 {
		t.Errorf("the first row's cpu_pct/interval_s = %s/%v, want null/0", floatText(first.Row.CPUPct), first.Row.IntervalS)
	}

	clock.now = clock.now.Add(60 * time.Second)
	second, err := source.Tick(context.Background())
	if err != nil {
		t.Fatalf("the second tick: %v", err)
	}
	if second.Row == nil {
		t.Fatal("the second tick wrote no row")
	}
	if second.Row.IntervalS != 60 {
		t.Errorf("the second row's interval_s = %v, want 60", second.Row.IntervalS)
	}
	if second.Row.CPUPct == nil || *second.Row.CPUPct != 60 {
		t.Errorf("the second row's cpu_pct = %s, want 60", floatText(second.Row.CPUPct))
	}
}

// TestSandboxVMSourceTickNoSeat is the bead's acceptance: a tick with no live
// seat writes no VM row, and a record a read cannot go through is skipped
// rather than addressed (gt-2qe2m6).
func TestSandboxVMSourceTickNoSeat(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		write func(t *testing.T, townRoot string)
		// skipOn is the seat the tick must report, empty for a town with no
		// record at all.
		skipOn string
	}{
		{
			name: "an empty town",
		},
		{
			name: "a session name that is not a DNS label",
			write: func(t *testing.T, townRoot string) {
				t.Helper()
				writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "Mango_toast", Rig: "gastown"})
			},
			skipOn: "Mango_toast",
		},
		{
			name: "a record that names no session",
			write: func(t *testing.T, townRoot string) {
				t.Helper()
				dir := session.SandboxRegistryDir(townRoot)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("creating the registry: %v", err)
				}
				path := filepath.Join(dir, "no-seat.json")
				if err := os.WriteFile(path, []byte(`{"session":"","rig":"gastown"}`), 0o644); err != nil {
					t.Fatalf("writing %s: %v", path, err)
				}
			},
			skipOn: "no-seat",
		},
		{
			name: "a record that is not a record",
			write: func(t *testing.T, townRoot string) {
				t.Helper()
				dir := session.SandboxRegistryDir(townRoot)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("creating the registry: %v", err)
				}
				path := filepath.Join(dir, "mango-aaa.json")
				if err := os.WriteFile(path, []byte("{not a record"), 0o644); err != nil {
					t.Fatalf("writing %s: %v", path, err)
				}
			},
			skipOn: "mango-aaa",
		},
	} {
		townRoot := t.TempDir()
		if test.write != nil {
			test.write(t, townRoot)
		}
		runner := &fakeSeatExec{read: vmProcRead(vmProcFilesHealthy())}
		tick, err := NewVMSource(townRoot, runner, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
		if err != nil {
			t.Errorf("%s: tick: %v", test.name, err)
			continue
		}
		if tick.Row != nil {
			t.Errorf("%s: row = %+v, want none", test.name, *tick.Row)
		}
		if len(runner.calls) != 0 {
			t.Errorf("%s: the seam was called for %v, want no exec", test.name, runner.calls)
		}
		if test.skipOn == "" {
			if len(tick.Skipped) != 0 {
				t.Errorf("%s: skipped %+v, want no skip", test.name, tick.Skipped)
			}
			continue
		}
		if len(tick.Skipped) != 1 || tick.Skipped[0].Seat != test.skipOn || tick.Skipped[0].Err == nil {
			t.Errorf("%s: skipped = %+v, want %s with its reason", test.name, tick.Skipped, test.skipOn)
		}
	}
}

// TestSandboxVMSourceReadFailures is the bead's acceptance: a failed read of
// any one of the five files writes no row, never one of zeros (gt-2qe2m6).
// Each row is one reason to fail.
func TestSandboxVMSourceReadFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		read func(context.Context, string) (string, error)
	}{
		{
			name: "an exec that fails",
			read: func(context.Context, string) (string, error) {
				return "", errors.New("sandbox mango-toast is not running")
			},
		},
		{
			name: "a text the parser refuses",
			read: func(context.Context, string) (string, error) {
				return "@@ loadavg @@\n", nil
			},
		},
		{name: "no loadavg", read: vmProcRead(vmProcWithout("loadavg"))},
		{name: "no pressure/cpu", read: vmProcRead(vmProcWithout("pressure/cpu"))},
		{name: "no pressure/memory", read: vmProcRead(vmProcWithout("pressure/memory"))},
		{name: "no stat", read: vmProcRead(vmProcWithout("stat"))},
		{name: "no meminfo", read: vmProcRead(vmProcWithout("meminfo"))},
		{name: "an empty stat", read: vmProcRead(vmProcWith("stat", ""))},
		{name: "an empty meminfo", read: vmProcRead(vmProcWith("meminfo", ""))},
		{name: "a loadavg that is not a load", read: vmProcRead(vmProcWith("loadavg", "soon 0.58\n"))},
		{name: "a pressure that is not pressure", read: vmProcRead(vmProcWith("pressure/cpu", "hello\n"))},
		{name: "a meminfo with no MemAvailable", read: vmProcRead(vmProcWith("meminfo", "MemTotal: 16384000 kB\n"))},
	} {
		townRoot := seatTown(t)
		writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown"})
		tick, err := NewVMSource(townRoot, &fakeSeatExec{read: test.read}, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
		if err != nil {
			t.Errorf("%s: tick: %v", test.name, err)
			continue
		}
		if tick.Row != nil {
			t.Errorf("%s: row = %+v, want none", test.name, *tick.Row)
		}
		if len(tick.Skipped) != 1 || tick.Skipped[0].Seat != "mango-toast" || tick.Skipped[0].Err == nil {
			t.Errorf("%s: skipped = %+v, want mango-toast with its reason", test.name, tick.Skipped)
		}
	}
}

// TestSandboxVMSourceFallsBackToAnotherSeat is the attempt-1 finding's
// acceptance: one stale record cannot keep the VM row out of every tick, so a
// seat whose read fails is skipped and the next live seat answers (gt-2qe2m6).
func TestSandboxVMSourceFallsBackToAnotherSeat(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		read func(ctx context.Context, name string) (string, error)
	}{
		{
			name: "an exec that fails",
			read: func(_ context.Context, name string) (string, error) {
				if name == "mango-aaa" {
					return "", errors.New("sandbox mango-aaa is not running")
				}
				return vmProcText(vmProcFilesHealthy()), nil
			},
		},
		{
			name: "a text the parser refuses",
			read: func(_ context.Context, name string) (string, error) {
				if name == "mango-aaa" {
					return "@@ loadavg @@\n", nil
				}
				return vmProcText(vmProcFilesHealthy()), nil
			},
		},
	} {
		townRoot := seatTown(t)
		writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-aaa", Rig: "gastown"})
		writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-zzz", Rig: "gastown"})
		runner := &fakeSeatExec{read: test.read}
		tick, err := NewVMSource(townRoot, runner, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
		if err != nil {
			t.Errorf("%s: tick: %v", test.name, err)
			continue
		}
		if tick.Row == nil {
			t.Errorf("%s: no VM row, want the next live seat's", test.name)
			continue
		}
		if !reflect.DeepEqual(runner.calls, []string{"mango-aaa", "mango-zzz"}) {
			t.Errorf("%s: seats read = %v, want the failed seat and then the next", test.name, runner.calls)
		}
		if len(tick.Skipped) != 1 || tick.Skipped[0].Seat != "mango-aaa" || tick.Skipped[0].Err == nil {
			t.Errorf("%s: skipped = %+v, want mango-aaa with its reason", test.name, tick.Skipped)
		}
	}
}

// TestSandboxVMSourceAllCorrupt is the attempt-1 finding's acceptance: a
// registry whose every record is corrupt is told from a cluster with no seat by
// its skips, and still writes no VM row (gt-2qe2m6).
func TestSandboxVMSourceAllCorrupt(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	dir := session.SandboxRegistryDir(townRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the registry: %v", err)
	}
	for _, name := range []string{"mango-aaa", "mango-bbb"} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte("{not a record"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	runner := &fakeSeatExec{read: vmProcRead(vmProcFilesHealthy())}
	tick, err := NewVMSource(townRoot, runner, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tick.Row != nil {
		t.Errorf("row = %+v, want none", *tick.Row)
	}
	if len(tick.Skipped) != 2 || tick.Skipped[0].Seat != "mango-aaa" || tick.Skipped[1].Seat != "mango-bbb" {
		t.Errorf("skipped = %+v, want both corrupt records", tick.Skipped)
	}
	if len(runner.calls) != 0 {
		t.Errorf("the seam was called for %v, want no exec", runner.calls)
	}

	// A town that never ran a sandbox is the empty answer the skips above are
	// told from: no row and nothing to report.
	tick, err = NewVMSource(seatTown(t), &fakeSeatExec{}, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
	if err != nil {
		t.Fatalf("the empty town: %v", err)
	}
	if tick.Row != nil || len(tick.Skipped) != 0 {
		t.Errorf("the empty town's tick = %+v, want no row and no skip", tick)
	}
}

// TestSandboxVMSourceReadTimeout covers the per-seat bound: a seat whose read
// outlasts it is skipped, the next live seat answers, and every exec is handed
// a deadline (gt-2qe2m6).
func TestSandboxVMSourceReadTimeout(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-aaa-hang", Rig: "gastown"})
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-zzz-ok", Rig: "gastown"})
	runner := &fakeSeatExec{read: func(ctx context.Context, name string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			return "", errors.New("the seam was called with no deadline")
		}
		if name != "mango-aaa-hang" {
			return vmProcText(vmProcFilesHealthy()), nil
		}
		<-ctx.Done()
		return "", ctx.Err()
	}}

	tick, err := NewVMSource(townRoot, runner, &fakeClock{now: seatTickNow()}, 20*time.Millisecond).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tick.Row == nil {
		t.Fatal("no VM row, want the seat after the hung one")
	}
	if len(tick.Skipped) != 1 || tick.Skipped[0].Seat != "mango-aaa-hang" {
		t.Fatalf("skipped = %+v, want the hung seat", tick.Skipped)
	}
	if !errors.Is(tick.Skipped[0].Err, context.DeadlineExceeded) {
		t.Errorf("the hung seat's reason = %v, want the deadline", tick.Skipped[0].Err)
	}
	if !reflect.DeepEqual(runner.calls, []string{"mango-aaa-hang", "mango-zzz-ok"}) {
		t.Errorf("seats read = %v, want the hung seat and then the next", runner.calls)
	}
}

// TestSandboxVMSourceDefaultTimeout pins the zero timeout's default at the
// seam: the read is bounded, and the bound is DefaultSeatTimeout (gt-2qe2m6).
func TestSandboxVMSourceDefaultTimeout(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown"})
	var remaining time.Duration
	runner := &fakeSeatExec{read: func(ctx context.Context, _ string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return "", errors.New("the seam was called with no deadline")
		}
		remaining = time.Until(deadline)
		return vmProcText(vmProcFilesHealthy()), nil
	}}

	tick, err := NewVMSource(townRoot, runner, &fakeClock{now: seatTickNow()}, 0).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tick.Row == nil {
		t.Fatal("no VM row")
	}
	if remaining > DefaultSeatTimeout || remaining < DefaultSeatTimeout-time.Second {
		t.Errorf("the read was bounded %s away, want the %s default", remaining, DefaultSeatTimeout)
	}
}

// TestSandboxVMSourceRefusals covers what fails the whole tick: no rows, no
// exec, and an error that names the cause (gt-2qe2m6).
func TestSandboxVMSourceRefusals(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(t *testing.T) (townRoot string, runner SeatExec, clock Clock)
	}{
		{
			name: "no town root",
			setup: func(*testing.T) (string, SeatExec, Clock) {
				return "", &fakeSeatExec{}, &fakeClock{now: seatTickNow()}
			},
		},
		{
			name: "no exec seam",
			setup: func(t *testing.T) (string, SeatExec, Clock) {
				t.Helper()
				return seatTown(t), nil, &fakeClock{now: seatTickNow()}
			},
		},
		{
			name: "a workspace map that will not load",
			setup: func(t *testing.T) (string, SeatExec, Clock) {
				t.Helper()
				townRoot := t.TempDir()
				writeWorkspaceMap(t, townRoot, `{"default":"","rigs":{}}`)
				writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-zzz", Rig: "gastown"})
				return townRoot, &fakeSeatExec{}, &fakeClock{now: seatTickNow()}
			},
		},
		{
			name: "a registry that cannot be listed",
			setup: func(t *testing.T) (string, SeatExec, Clock) {
				t.Helper()
				townRoot := seatTown(t)
				dir := session.SandboxRegistryDir(townRoot)
				if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
					t.Fatalf("creating .runtime: %v", err)
				}
				// A regular file where the registry belongs: the listing fails
				// with something other than "no such directory", the case that
				// must not read as a town with no seat.
				if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
					t.Fatalf("writing %s: %v", dir, err)
				}
				return townRoot, &fakeSeatExec{}, &fakeClock{now: seatTickNow()}
			},
		},
	} {
		townRoot, runner, clock := test.setup(t)
		tick, err := NewVMSource(townRoot, runner, clock, time.Second).Tick(context.Background())
		if err == nil {
			t.Errorf("%s: no error, tick %+v", test.name, tick)
			continue
		}
		if !reflect.DeepEqual(tick, VMTick{}) {
			t.Errorf("%s: error with tick %+v, want none", test.name, tick)
		}
		if fake, ok := runner.(*fakeSeatExec); ok && len(fake.calls) != 0 {
			t.Errorf("%s: the seam was called for %v, want no exec", test.name, fake.calls)
		}
	}
}

// TestSandboxVMReadArgv runs the command one VM read runs through a shell and
// hands what it printed to the parser: every file the VM publishes reaches its
// row, a file the kernel does not publish prints its marker and no text, and a
// file the reading itself comes from missing is a refused read rather than a
// row of zeros (gt-2qe2m6).
func TestSandboxVMReadArgv(t *testing.T) {
	t.Parallel()
	argv := vmProcReadArgv()
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("argv = %v, want sh -c <script>", argv)
	}
	for _, test := range []struct {
		name  string
		files map[string]string
		check func(t *testing.T, row Row, err error)
	}{
		{
			name:  "every file the VM publishes",
			files: vmProcFilesHealthy(),
			check: func(t *testing.T, row Row, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("the read's text: %v", err)
				}
				if row.MemBytes != (16384000-8192000)*1024 {
					t.Errorf("memory = %d bytes, want 8GiB in use", row.MemBytes)
				}
				if row.Load1 == nil || *row.Load1 != 0.52 {
					t.Errorf("load1 = %s, want 0.52", floatText(row.Load1))
				}
				if row.PressureCPUSomePct == nil || *row.PressureCPUSomePct != 1.5 {
					t.Errorf("cpu pressure some = %s, want 1.5", floatText(row.PressureCPUSomePct))
				}
			},
		},
		{
			name:  "a kernel without PSI",
			files: vmProcWithout("pressure/cpu"),
			check: func(t *testing.T, row Row, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("the read's text: %v", err)
				}
				if row.PressureCPUSomePct != nil {
					t.Errorf("cpu pressure = %s, want null for a file the kernel does not publish", floatText(row.PressureCPUSomePct))
				}
				if row.MemBytes == 0 {
					t.Error("mem_bytes is 0, want the reading of the file the VM does publish")
				}
			},
		},
		{
			name:  "a kernel with no load average",
			files: vmProcWithout("loadavg"),
			check: func(t *testing.T, row Row, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("the read's text: %v", err)
				}
				if row.Load1 != nil {
					t.Errorf("load1 = %s, want null for a file the kernel does not publish", floatText(row.Load1))
				}
			},
		},
		{
			name:  "no stat",
			files: vmProcWithout("stat"),
			check: func(t *testing.T, _ Row, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("the read of a VM with no stat wrote a row, want a refused read")
				}
			},
		},
		{
			name:  "no meminfo",
			files: vmProcWithout("meminfo"),
			check: func(t *testing.T, _ Row, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("the read of a VM with no meminfo wrote a row, want a refused read")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			text, err := vmProcShell(t, argv, test.files)
			if err != nil {
				t.Fatalf("run the VM read: %v", err)
			}
			row, _, err := VMRow(text, seatTickNow(), nil, seatWorkspaces(t))
			test.check(t, row, err)
		})
	}
}

// vmProcShell runs the argv one VM read runs through an in-process POSIX shell
// whose only command is cat: each file in files is served at its path under
// /proc, and a file the map does not hold is absent, as the kernel leaves one
// it does not publish.
func vmProcShell(t *testing.T, argv []string, files map[string]string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	runner, err := interp.New(
		interp.Env(expand.ListEnviron()),
		interp.Dir("/"),
		interp.ExecHandler(func(ctx context.Context, args []string) error {
			hc := interp.HandlerCtx(ctx)
			if args[0] != "cat" {
				return fmt.Errorf("the VM read runs %s; the shell here scripts only cat", args[0])
			}
			for _, name := range args[1:] {
				file, under := strings.CutPrefix(name, "/proc/")
				body, held := files[file]
				if !under || !held {
					fmt.Fprintf(hc.Stderr, "cat: %s: No such file or directory\n", name)
					return interp.NewExitStatus(1)
				}
				io.WriteString(hc.Stdout, body)
			}
			return nil
		}),
		// The read sends a missing file's error to /dev/null, which is served
		// here too, so the shell touches no path on the host.
		interp.OpenHandler(func(_ context.Context, name string, _ int, _ os.FileMode) (io.ReadWriteCloser, error) {
			if name == "/dev/null" {
				return nullFile{}, nil
			}
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}),
		interp.StdIO(nil, &stdout, io.Discard),
	)
	if err != nil {
		return "", err
	}
	node, err := syntax.NewParser().Parse(strings.NewReader(argv[2]), "")
	if err != nil {
		return "", err
	}
	if err := runner.Run(context.Background(), node); err != nil {
		var status interp.ExitStatus
		if errors.As(err, &status) {
			return stdout.String(), fmt.Errorf("the VM read exited %d", int(status))
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}
