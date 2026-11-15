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
	"strconv"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/steveyegge/gastown/internal/session"
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

// seatFilesWithout is seatFiles with one file a kernel does not publish left
// out: a seat read prints that file's marker and no text.
func seatFilesWithout(name string) map[string]string {
	files := seatFiles()
	delete(files, name)
	return files
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

// TestSandboxSourceLongMarkerName shows that a marker naming a long, unknown
// section is refused with an error cut to what a log line carries, rather than
// with the whole marker the seat printed (gt-kuncsk).
func TestSandboxSourceLongMarkerName(t *testing.T) {
	t.Parallel()
	workspaces := seatWorkspaces(t)
	now := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	marker := "@@ " + strings.Repeat("x", 4096) + " @@"
	text := strings.Replace(seatRead(seatFiles()), "@@ memory.max @@", marker, 1)

	_, _, err := SeatRow(seatIdentity(), text, now, nil, workspaces)
	if err == nil {
		t.Fatal("a marker naming no cgroup file: no error")
	}
	if len(err.Error()) > 200 {
		t.Errorf("the error is %d bytes, want one cut to a log line", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "...") {
		t.Errorf("the error %q does not mark the name as cut", err.Error())
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
	named := func(name string) *Seat { return &Seat{Name: name, Rig: "gastown"} }
	longLabel := strings.Repeat("a", 63)
	tooLong := strings.Join([]string{longLabel, longLabel, longLabel, longLabel}, ".")
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
		{name: "a NaN avg10", text: with("cpu.pressure", "some avg10=NaN\n")},
		{name: "an infinite avg10", text: with("cpu.pressure", "some avg10=+Inf\n")},
		{name: "a memory.pressure with an unreadable some avg10", text: with("memory.pressure", "some avg10=soon\nfull avg10=0.50\n")},
		{name: "a memory.pressure with no full line", text: with("memory.pressure", "some avg10=4.50\n")},
		{name: "a memory.pressure with an unreadable full avg10", text: with("memory.pressure", "some avg10=4.50\nfull avg10=soon\n")},
		{name: "a seat with no name", text: healthy, seat: &noName},
		{name: "a seat name with a space", text: healthy, seat: named("mango toast")},
		{name: "a seat name starting with a dash", text: healthy, seat: named("-mango")},
		{name: "a seat name ending with a dash", text: healthy, seat: named("mango-")},
		{name: "an uppercase seat name", text: healthy, seat: named("Mango-toast")},
		{name: "a seat name with an empty label", text: healthy, seat: named("mango..toast")},
		{name: "a seat name whose label is too long", text: healthy, seat: named(strings.Repeat("a", 64))},
		{name: "a seat name longer than 253 bytes", text: healthy, seat: named(tooLong)},
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

// fakeSeatExec is the exec seam a test fills: read answers one seat's read and
// calls records the seats the source asked about, in call order.
type fakeSeatExec struct {
	read  func(ctx context.Context, name string) (string, error)
	calls []string
	argv  [][]string
}

func (f *fakeSeatExec) Exec(ctx context.Context, name string, _ map[string]string, argv []string, _ []byte) ([]byte, []byte, error) {
	f.calls = append(f.calls, name)
	f.argv = append(f.argv, argv)
	if f.read == nil {
		return nil, nil, errors.New("the seam has no read")
	}
	text, err := f.read(ctx, name)
	return []byte(text), nil, err
}

// seatTown is a town root with the workspace map a seat row's workspace id
// comes from.
func seatTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	writeWorkspaceMap(t, townRoot, `{"default":"customer-zero","rigs":{"gastown":"acme"}}`)
	return townRoot
}

// writeSeatRecord records one seat of the town, through the writer the spawn
// uses.
func writeSeatRecord(t *testing.T, townRoot string, rec session.SandboxRecord) {
	t.Helper()
	if err := session.WriteSandboxRecord(townRoot, rec); err != nil {
		t.Fatalf("writing the record for %q: %v", rec.Session, err)
	}
}

// seatTickNow is the sample time the tick tests run at.
func seatTickNow() time.Time { return time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC) }

// TestSandboxSourceTick reads the town's live seats: one row per record, the
// record's rig, polecat and bead on it, and one exec per seat with the argv the
// parser's text comes from (gt-a24bcc).
func TestSandboxSourceTick(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown", Polecat: "toast", WorkBead: "gt-abc"})
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-nux", Rig: "gastown", Polecat: "nux"})
	runner := &fakeSeatExec{read: func(context.Context, string) (string, error) {
		return seatRead(seatFiles()), nil
	}}

	tick, err := NewSeatSource(townRoot, runner, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(tick.Skipped) != 0 {
		t.Errorf("skipped %+v, want no skip", tick.Skipped)
	}
	if !reflect.DeepEqual(runner.calls, []string{"mango-nux", "mango-toast"}) {
		t.Errorf("seats read = %v, want both records in name order", runner.calls)
	}
	if len(tick.Rows) != 2 {
		t.Fatalf("%d rows, want one per seat: %+v", len(tick.Rows), tick.Rows)
	}
	want := []Row{
		{
			V: 1, TS: "2026-11-14T10:00:00Z", Source: "sandbox", Class: "sandbox", Component: "seat",
			Name: "mango-nux", Rig: seatStringPtr("gastown"), Polecat: seatStringPtr("nux"),
			MemBytes: 1 << 30, MemLimitBytes: seatInt64Ptr(2 << 30), Workspace: "acme",
			Cluster: seatStringPtr(SandboxCluster), PressureCPUSomePct: seatFloatPtr(3.5),
			PressureMemSomePct: seatFloatPtr(4.5), PressureMemFullPct: seatFloatPtr(0.5),
		},
		{
			V: 1, TS: "2026-11-14T10:00:00Z", Source: "sandbox", Class: "sandbox", Component: "seat",
			Name: "mango-toast", Rig: seatStringPtr("gastown"), Polecat: seatStringPtr("toast"),
			Bead: seatStringPtr("gt-abc"), MemBytes: 1 << 30, MemLimitBytes: seatInt64Ptr(2 << 30),
			Workspace: "acme", Cluster: seatStringPtr(SandboxCluster), PressureCPUSomePct: seatFloatPtr(3.5),
			PressureMemSomePct: seatFloatPtr(4.5), PressureMemFullPct: seatFloatPtr(0.5),
		},
	}
	if !reflect.DeepEqual(tick.Rows, want) {
		t.Errorf("rows = %+v, want %+v", tick.Rows, want)
	}
	for i, argv := range runner.argv {
		if !reflect.DeepEqual(argv, seatReadArgv()) {
			t.Errorf("call %d ran %v, want the seat read's argv", i, argv)
		}
	}
}

// TestSandboxSourceReadArgv runs the command one seat read runs through a shell
// and hands what it printed to the parser: every file the seat publishes reaches
// its row, a file the kernel does not publish prints its marker and no text, and
// the reading itself missing is a refused read rather than a row of zeros
// (gt-a24bcc, gt-h44fwh).
func TestSandboxSourceReadArgv(t *testing.T) {
	t.Parallel()
	argv := seatReadArgv()
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("argv = %v, want sh -c <script>", argv)
	}
	for _, test := range []struct {
		name  string
		files map[string]string
		check func(t *testing.T, row Row, err error)
	}{
		{
			name:  "every file the seat publishes",
			files: seatFiles(),
			check: func(t *testing.T, row Row, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("the read's text: %v", err)
				}
				if row.MemBytes != 1<<30 || row.MemLimitBytes == nil || *row.MemLimitBytes != 2<<30 {
					t.Errorf("memory = %d of %v bytes, want 1GiB of 2GiB", row.MemBytes, row.MemLimitBytes)
				}
				if row.PressureCPUSomePct == nil || *row.PressureCPUSomePct != 3.5 {
					t.Errorf("cpu pressure some = %s, want 3.5", floatText(row.PressureCPUSomePct))
				}
				if row.PressureMemFullPct == nil || *row.PressureMemFullPct != 0.5 {
					t.Errorf("memory pressure full = %s, want 0.5", floatText(row.PressureMemFullPct))
				}
			},
		},
		{
			name:  "a file the kernel does not publish",
			files: seatFilesWithout("memory.pressure"),
			check: func(t *testing.T, row Row, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("the read's text: %v", err)
				}
				if row.PressureMemSomePct != nil || row.PressureMemFullPct != nil {
					t.Errorf("memory pressure = %s/%s, want null for a file the kernel does not publish",
						floatText(row.PressureMemSomePct), floatText(row.PressureMemFullPct))
				}
				if row.PressureCPUSomePct == nil {
					t.Error("cpu pressure is null, want the value of the file the seat does publish")
				}
			},
		},
		{
			name:  "the reading itself not published",
			files: seatFilesWithout("cpu.stat"),
			check: func(t *testing.T, _ Row, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("the read of a seat with no cpu.stat wrote a row, want a refused read")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			text, err := seatShell(t, argv, test.files)
			if err != nil {
				t.Fatalf("run the seat read: %v", err)
			}
			row, _, err := SeatRow(seatIdentity(), text, seatTickNow(), nil, seatWorkspaces(t))
			test.check(t, row, err)
		})
	}
}

// seatShell runs the argv one seat read runs through an in-process POSIX shell
// whose only command is cat: each file in files is served at its path under the
// seat's cgroup root, and a file the map does not hold is absent, as the kernel
// leaves one it does not publish. It returns what the read printed.
func seatShell(t *testing.T, argv []string, files map[string]string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	runner, err := interp.New(
		interp.Env(expand.ListEnviron()),
		interp.Dir("/"),
		interp.ExecHandler(func(ctx context.Context, args []string) error {
			hc := interp.HandlerCtx(ctx)
			if args[0] != "cat" {
				return fmt.Errorf("the seat read runs %s; the shell here scripts only cat", args[0])
			}
			for _, name := range args[1:] {
				file, under := strings.CutPrefix(name, seatCgroupRoot+"/")
				body, held := files[file]
				if !under || strings.Contains(file, "/") || !held {
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
			return stdout.String(), fmt.Errorf("the seat read exited %d", int(status))
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// nullFile is /dev/null in the shell seatShell runs: writes are dropped and a
// read is empty.
type nullFile struct{}

func (nullFile) Read([]byte) (int, error)    { return 0, io.EOF }
func (nullFile) Write(p []byte) (int, error) { return len(p), nil }
func (nullFile) Close() error                { return nil }

// TestSandboxSourceTickSeatFailures is the bead's acceptance: a seat whose read
// fails writes no row while the other seats still do (gt-a24bcc).
func TestSandboxSourceTickSeatFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		read func(context.Context, string) (string, error)
	}{
		{
			name: "an exec that fails",
			read: func(_ context.Context, name string) (string, error) {
				if name == "mango-aaa" {
					return "", errors.New("sandbox mango-aaa is not running")
				}
				return seatRead(seatFiles()), nil
			},
		},
		{
			name: "a text the parser refuses",
			read: func(_ context.Context, name string) (string, error) {
				if name == "mango-aaa" {
					return "@@ cpu.stat @@\nusage_usec 1000\n", nil
				}
				return seatRead(seatFiles()), nil
			},
		},
	} {
		townRoot := seatTown(t)
		writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-aaa", Rig: "gastown"})
		writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-zzz", Rig: "gastown"})
		tick, err := NewSeatSource(townRoot, &fakeSeatExec{read: test.read}, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
		if err != nil {
			t.Errorf("%s: tick: %v", test.name, err)
			continue
		}
		if len(tick.Rows) != 1 || tick.Rows[0].Name != "mango-zzz" {
			t.Errorf("%s: rows = %+v, want only mango-zzz's", test.name, tick.Rows)
		}
		if len(tick.Skipped) != 1 || tick.Skipped[0].Seat != "mango-aaa" || tick.Skipped[0].Err == nil {
			t.Errorf("%s: skipped = %+v, want mango-aaa with its reason", test.name, tick.Skipped)
		}
	}
}

// TestSandboxSourceTickInvalidName is the bead's acceptance: a name that fails
// validation is never passed to the exec seam (gt-a24bcc).
func TestSandboxSourceTickInvalidName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		write  func(t *testing.T, townRoot string)
		skipOn string
	}{
		{
			name: "a session name that is not a DNS label",
			write: func(t *testing.T, townRoot string) {
				t.Helper()
				// The record writer accepts this session name, so a town can
				// really hold it; it is not a name an exec may address.
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
	} {
		townRoot := seatTown(t)
		test.write(t, townRoot)
		writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-zzz", Rig: "gastown"})
		runner := &fakeSeatExec{read: func(context.Context, string) (string, error) {
			return seatRead(seatFiles()), nil
		}}
		tick, err := NewSeatSource(townRoot, runner, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
		if err != nil {
			t.Errorf("%s: tick: %v", test.name, err)
			continue
		}
		if !reflect.DeepEqual(runner.calls, []string{"mango-zzz"}) {
			t.Errorf("%s: seats read = %v, want only mango-zzz: the refused name reached the seam", test.name, runner.calls)
		}
		if len(tick.Rows) != 1 || tick.Rows[0].Name != "mango-zzz" {
			t.Errorf("%s: rows = %+v, want only mango-zzz's", test.name, tick.Rows)
		}
		if len(tick.Skipped) != 1 || tick.Skipped[0].Seat != test.skipOn || tick.Skipped[0].Err == nil {
			t.Errorf("%s: skipped = %+v, want %s with its reason", test.name, tick.Skipped, test.skipOn)
		}
	}
}

// TestSandboxSourceTickCorruptRecord shows a record that is not a record: the
// seat it stands for writes no row, and the seats beside it still do
// (gt-a24bcc).
func TestSandboxSourceTickCorruptRecord(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-zzz", Rig: "gastown"})
	path := filepath.Join(session.SandboxRegistryDir(townRoot), "mango-aaa.json")
	if err := os.WriteFile(path, []byte("{not a record"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	runner := &fakeSeatExec{read: func(context.Context, string) (string, error) {
		return seatRead(seatFiles()), nil
	}}

	tick, err := NewSeatSource(townRoot, runner, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(tick.Rows) != 1 || tick.Rows[0].Name != "mango-zzz" {
		t.Errorf("rows = %+v, want only mango-zzz's", tick.Rows)
	}
	if len(tick.Skipped) != 1 || tick.Skipped[0].Seat != "mango-aaa" || tick.Skipped[0].Err == nil {
		t.Errorf("skipped = %+v, want mango-aaa with its read error", tick.Skipped)
	}
}

// TestSandboxSourceTickTimeout covers the per-seat bound: a seat whose read
// outlasts it writes no row, the next seat still reads, and each seat's exec is
// handed a deadline (gt-a24bcc).
func TestSandboxSourceTickTimeout(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-aaa-hang", Rig: "gastown"})
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-zzz-ok", Rig: "gastown"})
	runner := &fakeSeatExec{read: func(ctx context.Context, name string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			return "", errors.New("the seam was called with no deadline")
		}
		if name != "mango-aaa-hang" {
			return seatRead(seatFiles()), nil
		}
		<-ctx.Done()
		return "", ctx.Err()
	}}

	tick, err := NewSeatSource(townRoot, runner, &fakeClock{now: seatTickNow()}, 20*time.Millisecond).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(tick.Rows) != 1 || tick.Rows[0].Name != "mango-zzz-ok" {
		t.Errorf("rows = %+v, want the seat after the hung one", tick.Rows)
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

// TestSandboxSourceTickRefusals covers what fails the whole tick: no rows, no
// exec, and an error that names the cause (gt-a24bcc).
func TestSandboxSourceTickRefusals(t *testing.T) {
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
		tick, err := NewSeatSource(townRoot, runner, clock, time.Second).Tick(context.Background())
		if err == nil {
			t.Errorf("%s: no error, tick %+v", test.name, tick)
			continue
		}
		if !reflect.DeepEqual(tick, SeatTick{}) {
			t.Errorf("%s: error with tick %+v, want none", test.name, tick)
		}
		if fake, ok := runner.(*fakeSeatExec); ok && len(fake.calls) != 0 {
			t.Errorf("%s: the seam was called for %v, want no exec", test.name, fake.calls)
		}
	}
}

// TestSandboxSourceTickNoSeats shows a town that never ran a sandbox: no rows,
// no skip, and no error (gt-a24bcc).
func TestSandboxSourceTickNoSeats(t *testing.T) {
	t.Parallel()
	runner := &fakeSeatExec{}
	tick, err := NewSeatSource(t.TempDir(), runner, &fakeClock{now: seatTickNow()}, time.Second).Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(tick.Rows) != 0 || len(tick.Skipped) != 0 {
		t.Errorf("tick = %+v, want no row and no skip", tick)
	}
	if len(runner.calls) != 0 {
		t.Errorf("the seam was called for %v, want no exec", runner.calls)
	}
}

// seatReadsUsage is an exec answer for a seat whose cgroup has used usec
// microseconds of CPU.
func seatReadsUsage(usec int64) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) {
		return seatRead(seatCPUText(usec)), nil
	}
}

// seatTickOneRow runs one tick and returns its only row, failing the test when
// the tick wrote no row or skipped a seat.
func seatTickOneRow(t *testing.T, source *SeatSource) Row {
	t.Helper()
	tick, err := source.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(tick.Skipped) != 0 {
		t.Fatalf("skipped %+v, want no skip", tick.Skipped)
	}
	if len(tick.Rows) != 1 {
		t.Fatalf("%d rows, want one: %+v", len(tick.Rows), tick.Rows)
	}
	return tick.Rows[0]
}

// TestSandboxSourceTickCPUDelta is the bead's acceptance: a seat's first tick
// reports null cpu_pct, and its second, a minute on, reports the percent of one
// core its usage_usec grew by, measured against the first tick's reading
// (gt-bru6l7).
func TestSandboxSourceTickCPUDelta(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown", Polecat: "toast"})
	runner := &fakeSeatExec{read: seatReadsUsage(1000000)}
	clock := &fakeClock{now: seatTickNow()}
	source := NewSeatSource(townRoot, runner, clock, time.Second)

	first := seatTickOneRow(t, source)
	if first.CPUPct != nil || first.IntervalS != 0 {
		t.Errorf("the first tick: cpu_pct %s, interval_s %v, want null and 0", floatText(first.CPUPct), first.IntervalS)
	}

	clock.advance(60 * time.Second)
	runner.read = seatReadsUsage(1750000)
	second := seatTickOneRow(t, source)
	if second.CPUPct == nil || *second.CPUPct != 1.25 {
		t.Errorf("the second tick: cpu_pct %s, want 1.25", floatText(second.CPUPct))
	}
	if second.IntervalS != 60 {
		t.Errorf("the second tick: interval_s %v, want 60", second.IntervalS)
	}
}

// TestSandboxSourceTickCPUReset shows a usage_usec that went backwards at the
// tick level: the tick reports its interval with null cpu_pct, and its reading
// becomes the baseline the next tick measures from, so the seat's CPU is
// measured again from the reset counter rather than hidden until it climbs past
// the old one (gt-jw9m75).
func TestSandboxSourceTickCPUReset(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown", Polecat: "toast"})
	runner := &fakeSeatExec{read: seatReadsUsage(1000000)}
	clock := &fakeClock{now: seatTickNow()}
	source := NewSeatSource(townRoot, runner, clock, time.Second)
	seatTickOneRow(t, source)

	clock.advance(60 * time.Second)
	runner.read = seatReadsUsage(900000)
	reset := seatTickOneRow(t, source)
	if reset.CPUPct != nil || reset.IntervalS != 60 {
		t.Errorf("the reset tick: cpu_pct %s, interval_s %v, want null and 60", floatText(reset.CPUPct), reset.IntervalS)
	}

	clock.advance(60 * time.Second)
	runner.read = seatReadsUsage(960000)
	row := seatTickOneRow(t, source)
	if row.CPUPct == nil || *row.CPUPct != 0.1 {
		t.Errorf("the tick after the reset: cpu_pct %s, want 0.1: 0.06 s of CPU over 60 s", floatText(row.CPUPct))
	}
}

// TestSandboxSourceTickCPUClockStalled shows a tick whose clock did not advance
// since the seat's stored reading: it reports interval_s 0 and null cpu_pct
// rather than a percentage divided by a zero interval, and its reading becomes
// the baseline the next tick measures from (gt-jw9m75).
func TestSandboxSourceTickCPUClockStalled(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown", Polecat: "toast"})
	runner := &fakeSeatExec{read: seatReadsUsage(1000000)}
	clock := &fakeClock{now: seatTickNow()}
	source := NewSeatSource(townRoot, runner, clock, time.Second)
	seatTickOneRow(t, source)

	runner.read = seatReadsUsage(1100000)
	stalled := seatTickOneRow(t, source)
	if stalled.CPUPct != nil || stalled.IntervalS != 0 {
		t.Errorf("the stalled tick: cpu_pct %s, interval_s %v, want null and 0", floatText(stalled.CPUPct), stalled.IntervalS)
	}

	clock.advance(60 * time.Second)
	runner.read = seatReadsUsage(1400000)
	row := seatTickOneRow(t, source)
	if row.CPUPct == nil || *row.CPUPct != 0.5 {
		t.Errorf("the tick after the stall: cpu_pct %s, want 0.5: 0.3 s of CPU over 60 s", floatText(row.CPUPct))
	}
}

// TestSandboxSourceTickCPUKeepsBaseline shows a seat that fails to read keeps
// the reading its next row is measured from: a failed exec and a read that does
// not parse both skip the seat, and the next good row's percent covers the
// whole gap since the last row that wrote (gt-bru6l7).
func TestSandboxSourceTickCPUKeepsBaseline(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown", Polecat: "toast"})
	runner := &fakeSeatExec{read: seatReadsUsage(1000000)}
	clock := &fakeClock{now: seatTickNow()}
	source := NewSeatSource(townRoot, runner, clock, time.Second)
	seatTickOneRow(t, source)

	for _, read := range []struct {
		name string
		read func(context.Context, string) (string, error)
	}{
		{"an exec that fails", func(context.Context, string) (string, error) { return "", errors.New("exec refused") }},
		{"a read that does not parse", func(context.Context, string) (string, error) { return "not a cgroup read", nil }},
	} {
		clock.advance(60 * time.Second)
		runner.read = read.read
		tick, err := source.Tick(context.Background())
		if err != nil {
			t.Fatalf("%s: tick: %v", read.name, err)
		}
		if len(tick.Rows) != 0 || len(tick.Skipped) != 1 {
			t.Errorf("%s: tick = %+v, want one skip and no row", read.name, tick)
		}
	}

	clock.advance(60 * time.Second)
	runner.read = seatReadsUsage(1600000)
	row := seatTickOneRow(t, source)
	if row.IntervalS != 180 {
		t.Errorf("interval_s = %v, want 180: measured from the first tick", row.IntervalS)
	}
	if row.CPUPct == nil || *row.CPUPct != 0.33 {
		t.Errorf("cpu_pct = %s, want 0.33: 0.6 s of CPU over 180 s", floatText(row.CPUPct))
	}
}

// TestSandboxSourceTickCPUSeatGone shows a seat whose record is gone loses its
// reading, so a seat that comes back under the same name starts from a first
// sample instead of from the old cgroup's counter (gt-bru6l7).
func TestSandboxSourceTickCPUSeatGone(t *testing.T) {
	t.Parallel()
	townRoot := seatTown(t)
	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown", Polecat: "toast"})
	runner := &fakeSeatExec{read: seatReadsUsage(1000000)}
	clock := &fakeClock{now: seatTickNow()}
	source := NewSeatSource(townRoot, runner, clock, time.Second)
	seatTickOneRow(t, source)

	record := filepath.Join(session.SandboxRegistryDir(townRoot), "mango-toast.json")
	if err := os.Remove(record); err != nil {
		t.Fatalf("removing the seat's record: %v", err)
	}
	clock.advance(60 * time.Second)
	tick, err := source.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick without the seat: %v", err)
	}
	if len(tick.Rows) != 0 {
		t.Fatalf("rows %+v for a seat with no record, want none", tick.Rows)
	}

	writeSeatRecord(t, townRoot, session.SandboxRecord{Session: "mango-toast", Rig: "gastown", Polecat: "toast"})
	clock.advance(60 * time.Second)
	runner.read = seatReadsUsage(2000000)
	row := seatTickOneRow(t, source)
	if row.CPUPct != nil || row.IntervalS != 0 {
		t.Errorf("the returning seat: cpu_pct %s, interval_s %v, want a first sample: null and 0", floatText(row.CPUPct), row.IntervalS)
	}
}
