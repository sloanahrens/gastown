package resourcesampler

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/workspaceid"
)

// seatFiles is the cgroup text of a healthy live seat, keyed by file name: the
// five files the design reads, with the values a seat row carries.
func seatFiles() map[string]string {
	return map[string]string{
		"cpu.stat":        "usage_usec 1000000\nnr_periods 0\n",
		"memory.current":  "1073741824\n",
		"memory.max":      "2147483648\n",
		"cpu.pressure":    "some avg10=3.50 avg60=1.00 avg300=0.50 total=1234\n",
		"memory.pressure": "some avg10=4.50 avg60=1.50 avg300=0.75 total=4321\nfull avg10=0.50 avg60=0.25 avg300=0.10 total=1234\n",
	}
}

// seatRead renders the text one seat read prints: each file under a marker
// naming it, then the end marker. A file the test leaves out prints no section.
func seatRead(files map[string]string) string {
	var text strings.Builder
	for _, name := range []string{"cpu.stat", "memory.current", "memory.max", "cpu.pressure", "memory.pressure"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		text.WriteString("@@ " + name + " @@\n" + body)
	}
	text.WriteString("@@ end @@\n")
	return text.String()
}

// seatWorkspaces is a town workspace map that gives gastown the acme id, so a
// seat row's workspace field is the id of the rig the seat names.
func seatWorkspaces(t *testing.T) *workspaceid.Map {
	t.Helper()
	townRoot := t.TempDir()
	writeWorkspaceMap(t, townRoot, `{"default":"customer-zero","rigs":{"gastown":"acme"}}`)
	m, err := workspaceid.Load(townRoot)
	if err != nil {
		t.Fatalf("load workspace map: %v", err)
	}
	return m
}

// seatIdentity is the seat the row tests sample.
func seatIdentity() Seat {
	return Seat{Name: "mango-toast", Rig: "gastown", Polecat: "toast", Bead: "gt-abc"}
}

func seatStringPtr(value string) *string  { return &value }
func seatFloatPtr(value float64) *float64 { return &value }
func seatInt64Ptr(value int64) *int64     { return &value }

// seatCPUText is a cgroup read whose usage_usec is usec.
func seatCPUText(usec int64) map[string]string {
	files := seatFiles()
	files["cpu.stat"] = "usage_usec " + strconv.FormatInt(usec, 10) + "\n"
	return files
}

// floatText renders a nullable reading for an error message, since %v of a
// pointer prints its address.
func floatText(value *float64) string {
	if value == nil {
		return "null"
	}
	return strconv.FormatFloat(*value, 'g', -1, 64)
}

// TestSandboxSourceSeatRow is the bead's acceptance: a live seat's cgroup text
// produces one row with the design row table's values, cpu_pct measured from
// two samples, and the pressure fields (gt-kuncsk).
func TestSandboxSourceSeatRow(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	seat := seatIdentity()
	first := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	second := first.Add(60 * time.Second)

	firstRow, reading, err := SeatRow(seat, seatRead(seatFiles()), first, nil, workspaces)
	if err != nil {
		t.Fatalf("the first sample: %v", err)
	}
	wantFirst := Row{
		V:                  1,
		TS:                 "2026-11-14T09:30:00Z",
		IntervalS:          0,
		Source:             "sandbox",
		Class:              "sandbox",
		Component:          "seat",
		Name:               "mango-toast",
		Rig:                seatStringPtr("gastown"),
		Polecat:            seatStringPtr("toast"),
		Bead:               seatStringPtr("gt-abc"),
		MemBytes:           1 << 30,
		MemLimitBytes:      seatInt64Ptr(2 << 30),
		Workspace:          "acme",
		Cluster:            seatStringPtr(SandboxCluster),
		PressureCPUSomePct: seatFloatPtr(3.5),
		PressureMemSomePct: seatFloatPtr(4.5),
		PressureMemFullPct: seatFloatPtr(0.5),
	}
	if !reflect.DeepEqual(firstRow, wantFirst) {
		t.Errorf("the first sample's row = %+v, want %+v", firstRow, wantFirst)
	}

	secondRow, _, err := SeatRow(seat, seatRead(seatCPUText(1750000)), second, &reading, workspaces)
	if err != nil {
		t.Fatalf("the second sample: %v", err)
	}
	wantSecond := wantFirst
	wantSecond.TS = "2026-11-14T09:31:00Z"
	wantSecond.IntervalS = 60
	wantSecond.CPUPct = seatFloatPtr(1.25)
	if !reflect.DeepEqual(secondRow, wantSecond) {
		t.Errorf("the second sample's row = %+v, want %+v", secondRow, wantSecond)
	}
}

// TestSandboxSourceCPU drives cpu_pct and interval_s against the previous
// sample: the first sample, a measured zero, a counter that reset, and gaps a
// percentage cannot be measured over (gt-kuncsk).
func TestSandboxSourceCPU(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	seat := seatIdentity()
	start := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	previous := func(usageUsec int64) *SeatReading {
		return &SeatReading{UsageUsec: usageUsec, At: start}
	}
	for _, test := range []struct {
		name         string
		now          time.Time
		usageUsec    int64
		previous     *SeatReading
		wantInterval float64
		wantCPU      *float64
	}{
		{"the first sample", start, 1000000, nil, 0, nil},
		{"a sample after a whole minute", start.Add(60 * time.Second), 1750000, previous(1000000), 60, seatFloatPtr(1.25)},
		{"a measured zero", start.Add(60 * time.Second), 1000000, previous(1000000), 60, seatFloatPtr(0)},
		{"a usage counter that went backwards", start.Add(60 * time.Second), 900000, previous(1000000), 60, nil},
		{"a clock that did not advance", start, 1100000, previous(1000000), 0, nil},
		{"a gap shorter than interval_s carries", start.Add(20 * time.Millisecond), 1000100, previous(1000000), 0, nil},
		{"a fractional gap rounded to the tenth", start.Add(45260 * time.Millisecond), 2000000, previous(1000000), 45.3, seatFloatPtr(2.21)},
	} {
		row, _, err := SeatRow(seat, seatRead(seatCPUText(test.usageUsec)), test.now, test.previous, workspaces)
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

// TestSandboxSourceUnlimitedSeat covers the readings a kernel may not publish:
// no memory limit, no pressure, and a record naming no rig, polecat or bead
// (gt-kuncsk). Each row is one way a nullable reading is absent.
func TestSandboxSourceUnlimitedSeat(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	now := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		memMax string
	}{
		{name: "a kernel that publishes no memory.max", memMax: ""},
		{name: "a memory.max that reads max", memMax: "max\n"},
	} {
		files := seatFiles()
		files["memory.max"] = test.memMax
		files["cpu.pressure"] = ""
		files["memory.pressure"] = ""
		row, _, err := SeatRow(Seat{Name: "mango-toast"}, seatRead(files), now, nil, workspaces)
		if err != nil {
			t.Errorf("%s: %v", test.name, err)
			continue
		}
		want := Row{
			V:         1,
			TS:        "2026-11-14T09:30:00Z",
			Source:    "sandbox",
			Class:     "sandbox",
			Component: "seat",
			Name:      "mango-toast",
			MemBytes:  1 << 30,
			Workspace: "customer-zero",
			Cluster:   seatStringPtr(SandboxCluster),
		}
		if !reflect.DeepEqual(row, want) {
			t.Errorf("%s: row = %+v, want %+v", test.name, row, want)
		}
	}
}

// TestSandboxSourceUnreadableSeat covers every text a seat read must refuse:
// each case writes no row rather than one of zeros (gt-kuncsk). Each row is one
// reason to fail.
func TestSandboxSourceUnreadableSeat(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	now := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	healthy := seatRead(seatFiles())
	with := func(name, body string) string {
		files := seatFiles()
		files[name] = body
		return seatRead(files)
	}
	without := func(name string) string {
		files := seatFiles()
		delete(files, name)
		return seatRead(files)
	}
	noName := Seat{Name: "", Rig: "gastown"}
	for _, test := range []struct {
		name  string
		text  string
		seat  *Seat
		noMap bool
	}{
		{name: "an empty read"},
		{name: "text before the first marker", text: "usage_usec 1000000\n" + healthy},
		{name: "a marker naming no cgroup file", text: strings.Replace(healthy, "@@ memory.max @@", "@@ memory.peak @@", 1)},
		{name: "a section the seat does not have", text: strings.Replace(healthy, "@@ end @@\n", "@@ memory.peak @@\n0\n@@ end @@\n", 1)},
		{name: "a section printed twice", text: strings.Replace(healthy, "@@ end @@\n", "@@ cpu.stat @@\nusage_usec 1\n@@ end @@\n", 1)},
		{name: "text after the end marker", text: healthy + "trailing\n"},
		{name: "a marker after the end marker", text: healthy + "@@ cpu.stat @@\n"},
		{name: "a second end marker", text: healthy + "@@ end @@\n"},
		{name: "a read that ends before its end marker", text: strings.TrimSuffix(healthy, "@@ end @@\n")},
		{name: "a read with no memory.max", text: without("memory.max")},
		{name: "an empty memory.current", text: with("memory.current", "")},
		{name: "a memory.current that is not a number", text: with("memory.current", "lots\n")},
		{name: "a negative memory.current", text: with("memory.current", "-1\n")},
		{name: "an empty cpu.stat", text: with("cpu.stat", "")},
		{name: "a cpu.stat with no usage_usec", text: with("cpu.stat", "nr_periods 0\n")},
		{name: "a usage_usec that is not a number", text: with("cpu.stat", "usage_usec soon\n")},
		{name: "a negative usage_usec", text: with("cpu.stat", "usage_usec -5\n")},
		{name: "a memory.max that is not a limit", text: with("memory.max", "2GiB\n")},
		{name: "a negative memory.max", text: with("memory.max", "-2\n")},
		{name: "a cpu.pressure with no some line", text: with("cpu.pressure", "full avg10=1.00\n")},
		{name: "a cpu.pressure that is not pressure", text: with("cpu.pressure", "hello\n")},
		{name: "a some line with no avg10", text: with("cpu.pressure", "some avg60=1.00\n")},
		{name: "an avg10 that is not a number", text: with("cpu.pressure", "some avg10=soon\n")},
		{name: "a negative avg10", text: with("cpu.pressure", "some avg10=-1.00\n")},
		{name: "a memory.pressure with an unreadable some avg10", text: with("memory.pressure", "some avg10=soon\nfull avg10=0.50\n")},
		{name: "a memory.pressure with no full line", text: with("memory.pressure", "some avg10=4.50\n")},
		{name: "a memory.pressure with an unreadable full avg10", text: with("memory.pressure", "some avg10=4.50\nfull avg10=soon\n")},
		{name: "a seat with no name", text: healthy, seat: &noName},
		{name: "a seat with no workspace map", text: healthy, noMap: true},
	} {
		seat := seatIdentity()
		if test.seat != nil {
			seat = *test.seat
		}
		var workspaceMap *workspaceid.Map
		if !test.noMap {
			workspaceMap = workspaces
		}
		row, reading, err := SeatRow(seat, test.text, now, nil, workspaceMap)
		if err == nil {
			t.Errorf("%s: no error, row %+v", test.name, row)
			continue
		}
		if !reflect.DeepEqual(row, Row{}) || !reflect.DeepEqual(reading, SeatReading{}) {
			t.Errorf("%s: error with row %+v and reading %+v, want neither", test.name, row, reading)
		}
	}
}
