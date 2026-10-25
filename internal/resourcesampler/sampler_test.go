package resourcesampler

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// rowKeys is the schema's field order. A row written in another order still
// reads, but two samplers writing one file should differ in values only.
var rowKeys = []string{
	"v", "ts", "interval_s", "source", "class", "component", "name",
	"rig", "polecat", "bead", "cpu_pct", "mem_bytes", "mem_limit_bytes",
}

// fakeClock is the Clock a test moves by hand.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// fakeLister is the ProcessLister a test fills from a fixture.
type fakeLister struct {
	processes []Process
	err       error
}

func (l *fakeLister) ListProcesses(context.Context) ([]Process, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.processes, nil
}

// process builds a process list entry; seconds are what ps prints.
func process(pid int, rssKiB int64, cpuSeconds, elapsedSeconds float64, command string) Process {
	return Process{
		PID:      pid,
		RSSBytes: rssKiB * 1024,
		CPUTime:  time.Duration(cpuSeconds * float64(time.Second)),
		Elapsed:  time.Duration(elapsedSeconds * float64(time.Second)),
		Command:  command,
	}
}

// tick takes one tick and returns every line of the day file, so an assertion
// reads what a reader reads.
func tick(t *testing.T, sampler *Sampler, day time.Time) []string {
	t.Helper()
	if _, err := sampler.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	return dayLines(t, sampler.metricsDir, day)
}

// dayLines returns the day file's lines, nil when the sampler wrote no file.
func dayLines(t *testing.T, metricsDir string, day time.Time) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(metricsDir, rowsSubdir, day.Format(dayLayout)+".jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read day file: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// decodeRows decodes the lines into the maps a reader sees.
func decodeRows(t *testing.T, lines []string) []map[string]any {
	t.Helper()
	rows := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decode row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// jsonKeys returns the keys of one JSON object in file order.
func jsonKeys(t *testing.T, line string) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(line))
	if _, err := decoder.Token(); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("decode key of %q: %v", line, err)
		}
		key, ok := token.(string)
		if !ok {
			t.Fatalf("decode key of %q: token %v is not a string", line, token)
		}
		keys = append(keys, key)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("decode value of %q: %v", line, err)
		}
	}
	return keys
}

// componentRow returns the one row of a tick that names the component.
func componentRow(t *testing.T, rows []map[string]any, component string) map[string]any {
	t.Helper()
	for _, row := range rows {
		if row["component"] == component {
			return row
		}
	}
	t.Fatalf("no %q row among %v", component, componentsOf(rows))
	return nil
}

// componentsOf returns every row's component in order.
func componentsOf(rows []map[string]any) []string {
	components := make([]string, 0, len(rows))
	for _, row := range rows {
		component, _ := row["component"].(string)
		components = append(components, component)
	}
	return components
}

// number returns a numeric row field.
func number(t *testing.T, row map[string]any, key string) float64 {
	t.Helper()
	value, ok := row[key].(float64)
	if !ok {
		t.Fatalf("row[%q] = %#v, want a number", key, row[key])
	}
	return value
}

// null reports whether a row field is JSON null.
func null(row map[string]any, key string) bool {
	return row[key] == nil
}

func TestFirstTick(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	lister := &fakeLister{processes: []Process{
		process(100, 1000, 10, 3600, "/usr/local/bin/dolt sql-server"),
		process(200, 2000, 5, 600, "/usr/local/bin/claude --dangerously"),
		process(300, 500, 1, 300, "/usr/local/go/bin/go test ./..."),
		process(400, 100, 0, 60, "mds_stores"),
		process(500, 900, 2, 7200, "/usr/libexec/kernel_task"),
		process(600, 700, 0, 120, "XProtectService"),
	}}
	sampler := New(dir, lister, clock, 30*time.Second)

	lines := tick(t, sampler, clock.now)
	rows := decodeRows(t, lines)

	wantComponents := []string{"av", "claude", "dolt", "kernel", "spotlight", "tests"}
	if got := componentsOf(rows); !slices.Equal(got, wantComponents) {
		t.Fatalf("components = %v, want %v", got, wantComponents)
	}
	for _, line := range lines {
		if got := jsonKeys(t, line); !slices.Equal(got, rowKeys) {
			t.Fatalf("row keys = %v, want the schema's %v", got, rowKeys)
		}
	}

	dolt := componentRow(t, rows, "dolt")
	if got := number(t, dolt, "v"); got != SchemaVersion {
		t.Errorf("v = %v, want %d", got, SchemaVersion)
	}
	if got := dolt["ts"]; got != "2026-10-24T12:00:00Z" {
		t.Errorf("ts = %v, want the UTC second the clock read", got)
	}
	if got := number(t, dolt, "interval_s"); got != 30 {
		t.Errorf("interval_s = %v, want the configured 30", got)
	}
	if got := dolt["source"]; got != "host" {
		t.Errorf("source = %v, want host", got)
	}
	if got := dolt["class"]; got != "control-plane" {
		t.Errorf("class = %v, want control-plane", got)
	}
	if got := dolt["name"]; got != "dolt" {
		t.Errorf("name = %v, want the process that used the most memory", got)
	}
	if got := number(t, dolt, "mem_bytes"); got != 1000*1024 {
		t.Errorf("mem_bytes = %v, want rss in bytes", got)
	}
	for _, key := range []string{"rig", "polecat", "bead", "cpu_pct", "mem_limit_bytes"} {
		if !null(dolt, key) {
			t.Errorf("dolt %s = %#v, want null", key, dolt[key])
		}
	}

	for component, class := range map[string]string{
		"av": "operator", "claude": "operator", "kernel": "operator",
		"spotlight": "operator", "tests": "ci",
	} {
		if got := componentRow(t, rows, component)["class"]; got != class {
			t.Errorf("%s class = %v, want %s", component, got, class)
		}
	}

	wantDir, wantFile := expectedModes(t)
	if info, err := os.Stat(filepath.Join(dir, rowsSubdir)); err != nil {
		t.Errorf("stat rows directory: %v", err)
	} else if got := info.Mode().Perm(); got != wantDir {
		t.Errorf("rows directory mode = %o, want %o", got, wantDir)
	}
	path := filepath.Join(dir, rowsSubdir, clock.now.Format(dayLayout)+".jsonl")
	if info, err := os.Stat(path); err != nil {
		t.Errorf("stat day file: %v", err)
	} else if got := info.Mode().Perm(); got != wantFile {
		t.Errorf("day file mode = %o, want %o", got, wantFile)
	}
}

// expectedModes measures the modes the sampler asks for (0755 for the
// directory, 0644 for the file) through this process's umask, which no test can
// read without setting it.
func expectedModes(t *testing.T) (dir, file fs.FileMode) {
	t.Helper()
	probeDir := filepath.Join(t.TempDir(), "probe")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatalf("probe directory: %v", err)
	}
	info, err := os.Stat(probeDir)
	if err != nil {
		t.Fatalf("stat probe directory: %v", err)
	}
	dir = info.Mode().Perm()

	probeFile := filepath.Join(t.TempDir(), "probe")
	f, err := os.OpenFile(probeFile, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("probe file: %v", err)
	}
	defer f.Close()
	info, err = os.Stat(probeFile)
	if err != nil {
		t.Fatalf("stat probe file: %v", err)
	}
	return dir, info.Mode().Perm()
}

func TestCPUDeltaPerComponent(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	lister := &fakeLister{processes: []Process{
		process(100, 1000, 10, 3600, "/usr/local/bin/dolt"),
		process(101, 5000, 10, 3600, "/usr/local/bin/dolt-sql"),
		process(300, 500, 1, 300, "/usr/local/go/bin/go test ./..."),
		process(301, 4000, 1, 300, "make build"),
	}}
	sampler := New(dir, lister, clock, 30*time.Second)

	first := decodeRows(t, tick(t, sampler, clock.now))
	if got := componentRow(t, first, "dolt")["name"]; got != "dolt-sql" {
		t.Errorf("first tick name = %v, want the largest memory dolt-sql", got)
	}
	if got := componentRow(t, first, "tests")["name"]; got != "make" {
		t.Errorf("first tick name = %v, want the largest memory make", got)
	}

	clock.advance(30 * time.Second)
	lister.processes = []Process{
		process(100, 1000, 25, 3630, "/usr/local/bin/dolt"),
		process(101, 9000, 13, 3630, "/usr/local/bin/dolt-sql"),
		process(300, 500, 4, 330, "/usr/local/go/bin/go test ./..."),
		process(301, 4000, 4, 330, "make build"),
	}
	second := decodeRows(t, tick(t, sampler, clock.now))
	if len(second) != 4 {
		t.Fatalf("two ticks wrote %d rows, want four", len(second))
	}

	dolt := componentRow(t, second[2:], "dolt")
	if got := number(t, dolt, "cpu_pct"); got != 60 {
		t.Errorf("dolt cpu_pct = %v, want 50+10", got)
	}
	if got := number(t, dolt, "mem_bytes"); got != (1000+9000)*1024 {
		t.Errorf("dolt mem_bytes = %v, want the component's processes summed", got)
	}
	if got := dolt["name"]; got != "dolt" {
		t.Errorf("dolt name = %v, want the process with the most CPU, not the most memory", got)
	}

	tests := componentRow(t, second[2:], "tests")
	if got := number(t, tests, "cpu_pct"); got != 20 {
		t.Errorf("tests cpu_pct = %v, want 10+10", got)
	}
	if got := tests["name"]; got != "make" {
		t.Errorf("tests name = %v, want the larger memory on a CPU tie", got)
	}
}

func TestDeltaGuards(t *testing.T) {
	tests := []struct {
		name      string
		second    Process
		component string
		wantName  string
		wantCPU   *float64
	}{
		{
			name:      "a stable pid measures its delta",
			second:    process(100, 1000, 25, 3630, "/usr/local/bin/dolt"),
			component: "dolt", wantName: "dolt", wantCPU: cpu(50),
		},
		{
			name:      "a process that used no CPU is zero, not unknown",
			second:    process(100, 1000, 10, 3630, "/usr/local/bin/dolt"),
			component: "dolt", wantName: "dolt", wantCPU: cpu(0),
		},
		{
			name:      "unchanged elapsed time is still a live pid",
			second:    process(100, 1000, 13, 3600, "/usr/local/bin/dolt"),
			component: "dolt", wantName: "dolt", wantCPU: cpu(10),
		},
		{
			name:      "a pid whose elapsed time went backwards was reused",
			second:    process(100, 1000, 90, 5, "/usr/local/bin/dolt"),
			component: "dolt", wantName: "dolt",
		},
		{
			name:      "a cpu total that went backwards is no delta",
			second:    process(100, 1000, 5, 3630, "/usr/local/bin/dolt"),
			component: "dolt", wantName: "dolt",
		},
		{
			name:      "a renamed pid is a different process",
			second:    process(100, 1000, 25, 3630, "/usr/bin/python3 serve.py"),
			component: "other", wantName: "python3",
		},
		{
			name:      "a pid the previous tick did not list has no baseline",
			second:    process(102, 1000, 25, 3630, "/usr/local/bin/dolt"),
			component: "dolt", wantName: "dolt",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
			lister := &fakeLister{processes: []Process{
				process(100, 1000, 10, 3600, "/usr/local/bin/dolt"),
			}}
			sampler := New(dir, lister, clock, 30*time.Second)
			tick(t, sampler, clock.now)

			clock.advance(30 * time.Second)
			lister.processes = []Process{test.second}
			rows := decodeRows(t, tick(t, sampler, clock.now))[1:]
			row := componentRow(t, rows, test.component)
			if got := row["name"]; got != test.wantName {
				t.Errorf("name = %v, want %s", got, test.wantName)
			}
			if test.wantCPU == nil {
				if !null(row, "cpu_pct") {
					t.Errorf("cpu_pct = %v, want null", row["cpu_pct"])
				}
				return
			}
			if got := number(t, row, "cpu_pct"); got != *test.wantCPU {
				t.Errorf("cpu_pct = %v, want %v", got, *test.wantCPU)
			}
		})
	}
}

// cpu is a pointer to a percentage a table row expects.
func cpu(percent float64) *float64 { return &percent }

func TestCPURounding(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	lister := &fakeLister{processes: []Process{
		process(100, 1000, 10, 3600, "/usr/local/bin/dolt"),
	}}
	sampler := New(dir, lister, clock, 3*time.Second)
	tick(t, sampler, clock.now)

	clock.advance(3 * time.Second)
	lister.processes = []Process{process(100, 1000, 11, 3603, "/usr/local/bin/dolt")}
	rows := decodeRows(t, tick(t, sampler, clock.now))[1:]
	if got := number(t, componentRow(t, rows, "dolt"), "cpu_pct"); got != 33.33 {
		t.Errorf("cpu_pct = %v, want 33.33", got)
	}
}

func TestInterval(t *testing.T) {
	tests := []struct {
		name         string
		advance      time.Duration
		cpuSeconds   float64
		wantInterval float64
		wantCPU      float64
	}{
		{"the clock delta", 45 * time.Second, 9, 45, 20},
		{"the configured interval when the clock did not advance", 0, 3, 30, 10},
		{"a delta rounded to the tenth interval_s carries", 30040 * time.Millisecond, 3, 30, 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
			lister := &fakeLister{processes: []Process{
				process(100, 1000, 10, 3600, "/usr/local/bin/dolt"),
			}}
			sampler := New(dir, lister, clock, 30*time.Second)

			first := decodeRows(t, tick(t, sampler, clock.now))
			if got := number(t, componentRow(t, first, "dolt"), "interval_s"); got != 30 {
				t.Errorf("first interval_s = %v, want the configured 30", got)
			}

			clock.advance(test.advance)
			lister.processes = []Process{
				process(100, 1000, 10+test.cpuSeconds, 3600+test.advance.Seconds(), "/usr/local/bin/dolt"),
			}
			rows := decodeRows(t, tick(t, sampler, clock.now))[1:]
			dolt := componentRow(t, rows, "dolt")
			if got := number(t, dolt, "interval_s"); got != test.wantInterval {
				t.Errorf("interval_s = %v, want %v", got, test.wantInterval)
			}
			if got := number(t, dolt, "cpu_pct"); got != test.wantCPU {
				t.Errorf("cpu_pct = %v, want %v", got, test.wantCPU)
			}
		})
	}
}

func TestProcessListFailure(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	lister := &fakeLister{processes: []Process{
		process(100, 1000, 10, 3600, "/usr/local/bin/dolt"),
	}}
	sampler := New(dir, lister, clock, 30*time.Second)
	tick(t, sampler, clock.now)

	clock.advance(30 * time.Second)
	lister.err = errors.New("ps: exit status 1")
	rows, err := sampler.Tick(context.Background())
	if err == nil {
		t.Fatal("a failed process list returned no error")
	}
	if rows != nil {
		t.Errorf("a failed process list returned %d rows, want none", len(rows))
	}
	if lines := dayLines(t, dir, clock.now); len(lines) != 1 {
		t.Errorf("the day file has %d rows, want the one the first tick wrote", len(lines))
	}

	// The failure dropped the baseline, so the next success reports null CPU
	// rather than dividing two ticks' CPU by one interval, and the tick after it
	// measures a normal delta.
	lister.err = nil
	lister.processes = []Process{process(100, 1000, 25, 3630, "/usr/local/bin/dolt")}
	clock.advance(30 * time.Second)
	after := decodeRows(t, tick(t, sampler, clock.now))
	if !null(componentRow(t, after[1:], "dolt"), "cpu_pct") {
		t.Errorf("the first success after a failure reported CPU, want null")
	}

	clock.advance(30 * time.Second)
	lister.processes = []Process{process(100, 1000, 40, 3660, "/usr/local/bin/dolt")}
	measured := decodeRows(t, tick(t, sampler, clock.now))[2:]
	if got := number(t, componentRow(t, measured, "dolt"), "cpu_pct"); got != 50 {
		t.Errorf("the tick after the first success cpu_pct = %v, want 50", got)
	}
}

func TestEmptyProcessListWritesNothing(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	lister := &fakeLister{}
	sampler := New(dir, lister, clock, 30*time.Second)

	rows, err := sampler.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("an empty process list wrote %d rows", len(rows))
	}
	if _, err := os.Stat(filepath.Join(dir, rowsSubdir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an empty process list created the rows directory: %v", err)
	}

	lister.processes = []Process{process(100, 1000, 10, 3600, "/usr/local/bin/dolt")}
	if rows := decodeRows(t, tick(t, sampler, clock.now)); len(rows) != 1 {
		t.Errorf("the next tick wrote %d rows, want one", len(rows))
	}
}

func TestAppendAcrossTicksAndDays(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 10, 24, 23, 59, 0, 0, time.UTC)}
	lister := &fakeLister{processes: []Process{
		process(100, 1000, 10, 3600, "/usr/local/bin/dolt"),
	}}
	sampler := New(dir, lister, clock, 30*time.Second)

	tick(t, sampler, clock.now)
	clock.advance(30 * time.Second)
	lines := tick(t, sampler, clock.now)
	if len(lines) != 2 {
		t.Fatalf("two ticks wrote %d lines, want two", len(lines))
	}

	clock.advance(2 * time.Minute)
	next := decodeRows(t, tick(t, sampler, clock.now))
	if len(next) != 1 {
		t.Fatalf("the next day's file has %d lines, want one", len(next))
	}
	if got := next[0]["ts"]; got != "2026-10-25T00:01:30Z" {
		t.Errorf("next day ts = %v", got)
	}
}

func TestAppendFailureReturnsRows(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	lister := &fakeLister{processes: []Process{
		process(100, 1000, 10, 3600, "/usr/local/bin/dolt"),
	}}
	sampler := New(blocked, lister, clock, 30*time.Second)

	rows, err := sampler.Tick(context.Background())
	if err == nil {
		t.Fatal("a rows directory that cannot be created returned no error")
	}
	if len(rows) != 1 {
		t.Errorf("a failed append returned %d rows, want the tick's one", len(rows))
	}
}

func TestHostComponent(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{"/usr/local/bin/dolt sql-server", "dolt"},
		{"/opt/doltgate --serve", "dolt"},
		{"/usr/local/bin/dolt-claude", "dolt"},
		{"/usr/local/bin/claude --dangerously", "claude"},
		{"/usr/local/bin/claude-code", "claude"},
		{"/usr/bin/node /usr/local/lib/claude/index.js", "claude"},
		{"/usr/bin/node server.js", "other"},
		{"/usr/local/bin/gt prime", "gtbd"},
		{"/usr/local/bin/bd list", "gtbd"},
		{"/usr/local/go/bin/go test ./...", "tests"},
		{"make build", "tests"},
		{"/x/compile -o out", "tests"},
		{"/x/link -o out", "tests"},
		{"/x/asm -o out", "tests"},
		{"/x/cgo -o out", "tests"},
		{"/x/vet ./...", "tests"},
		{"/bin/sh -c /tmp/go-build123/b001/exe/test", "tests"},
		{"/bin/sh /usr/local/go/pkg/tool/linux_arm64/compile", "tests"},
		{"/usr/libexec/kernel_task", "kernel"},
		{"/usr/libexec/kernel_task_something", "kernel"},
		{"mds_stores", "spotlight"},
		{"mdworker_shared", "spotlight"},
		{"/System/Library/CoreServices/Spotlight", "spotlight"},
		{"XProtectService", "av"},
		{"falcon-sensor", "av"},
		{"sentinelctl", "av"},
		{"crowdstrike-agent", "av"},
		{"sophosd", "av"},
		{"clamd", "av"},
		{"esetd", "av"},
		{"mcafee-agent", "av"},
		{"qemu-system-x86_64", "vm"},
		{"vmm", "vm"},
		{"vmnetd", "vm"},
		{"hyperkit", "vm"},
		{"virtualizationd", "vm"},
		{"limactl", "vm"},
		{"python3 serve.py", "other"},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			if got := hostComponent(test.command); got != test.want {
				t.Errorf("hostComponent(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}

func TestClassOf(t *testing.T) {
	tests := map[string]string{
		"dolt":      "control-plane",
		"gtbd":      "control-plane",
		"tests":     "ci",
		"claude":    "operator",
		"vm":        "operator",
		"av":        "operator",
		"spotlight": "operator",
		"kernel":    "operator",
		"other":     "other",
		"unknown":   "other",
	}
	for component, want := range tests {
		if got := classOf(component); got != want {
			t.Errorf("classOf(%q) = %q, want %q", component, got, want)
		}
	}
}

func TestParsePS(t *testing.T) {
	out := strings.Join([]string{
		"  100  1000 00:00:10 01:00:00 /usr/local/bin/dolt sql-server",
		"  200  2000 00:00:05.5 00:10:00 /usr/local/bin/claude --dangerously",
		"not a process line",
		"  x  1000 00:00:10 01:00:00 bad-pid",
		"  300  nope 00:00:10 01:00:00 bad-rss",
		"  400  10 bad-cpu 01:00:00 bad-cpu-time",
		"  500  10 00:00:10 bad-elapsed bad-elapsed-time",
		"  600  10 1-02:03:04 2-00:00:00 long-running",
		"",
	}, "\n")
	got := parsePS(out)
	if len(got) != 3 {
		t.Fatalf("parsed %d processes, want three: %+v", len(got), got)
	}
	want := []Process{
		{PID: 100, RSSBytes: 1000 * 1024, CPUTime: 10 * time.Second, Elapsed: time.Hour, Command: "/usr/local/bin/dolt sql-server"},
		{PID: 200, RSSBytes: 2000 * 1024, CPUTime: 5500 * time.Millisecond, Elapsed: 10 * time.Minute, Command: "/usr/local/bin/claude --dangerously"},
		{PID: 600, RSSBytes: 10 * 1024, CPUTime: (86400 + 2*3600 + 3*60 + 4) * time.Second, Elapsed: 2 * 86400 * time.Second, Command: "long-running"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parsePS = %+v, want %+v", got, want)
	}
}

func TestParsePSTime(t *testing.T) {
	tests := []struct {
		text string
		want time.Duration
		ok   bool
	}{
		{"00:00:10", 10 * time.Second, true},
		{"01:00:00", time.Hour, true},
		{"00:10:00.5", 10*time.Minute + 500*time.Millisecond, true},
		{"1-02:03:04", 86400*time.Second + 2*time.Hour + 3*time.Minute + 4*time.Second, true},
		{"1-2", 86400*time.Second + 2*time.Second, true},
		{"", 0, false},
		{"bad", 0, false},
		{"1:2:3:4", 0, false},
		{"-5", 0, false},
	}
	for _, test := range tests {
		got, ok := parsePSTime(test.text)
		if ok != test.ok {
			t.Errorf("parsePSTime(%q) ok = %v, want %v", test.text, ok, test.ok)
			continue
		}
		if ok && got != test.want {
			t.Errorf("parsePSTime(%q) = %v, want %v", test.text, got, test.want)
		}
	}
}
