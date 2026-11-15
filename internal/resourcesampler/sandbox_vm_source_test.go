package resourcesampler

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

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
