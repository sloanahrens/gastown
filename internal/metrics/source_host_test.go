package metrics

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The fixtures are the shapes the commands print on the operator's Mac, with
// the process names, ids and titles replaced.

const hostTopFixture = `Processes: 641 total, 2 running, 639 sleeping, 3204 threads
2026-10-24 13:04:02
Load Avg: 14.07, 12.88, 11.51
CPU usage: 10.42% user, 5.29% sys, 84.29% idle
SharedLibs: 418M resident, 96M data, 21M linkedit.
MemRegions: 201714 total, 12G resident, 214M private, 3102M shared.
PhysMem: 31G used (4211M wired, 12G compressor), 191M unused.
Networks: packets: 1203/8300K in, 1188/1200K out.

Processes: 641 total, 2 running, 639 sleeping, 3204 threads
2026-10-24 13:04:03
Load Avg: 13.77, 12.84, 11.49
CPU usage: 8.13% user, 4.88% sys, 86.99% idle
SharedLibs: 418M resident, 96M data, 21M linkedit.
MemRegions: 201706 total, 12G resident, 214M private, 3102M shared.
PhysMem: 31G used (4200M wired, 12G compressor), 191M unused.
`

const hostLsofFixture = `COMMAND   PID    USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
dolt    41022   sloan   45u  IPv4 0x8a1b2c3d4e5f6071      0t0  TCP 127.0.0.1:3307->127.0.0.1:52946 (ESTABLISHED)
dolt    41022   sloan   46u  IPv4 0x8a1b2c3d4e5f6082      0t0  TCP 127.0.0.1:3307->127.0.0.1:53012 (ESTABLISHED)
bd      41099   sloan    5u  IPv4 0x7f0c1d2e3f405162      0t0  TCP 127.0.0.1:52946->127.0.0.1:3307 (ESTABLISHED)
bd      41112   sloan    5u  IPv4 0x7f0c1d2e3f405173      0t0  TCP 127.0.0.1:53012->127.0.0.1:3307 (ESTABLISHED)
`

const hostLsofHeader = `COMMAND   PID    USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
`

const hostDFFixture = `Filesystem     1024-blocks      Used Available Capacity  Mounted on
overlay          100000000  59000000  41000000      59%  /
`

// hostBDFixture is what bd prints; the probe measures the call rather than
// reading the output, so its shape is all the test needs.
const hostBDFixture = `Found 1 issue:
○ gt-xxxxxxx P2 <title>
`

// hostFake answers the probes with canned output. Its clock moves only when a
// command runs, by that command's took duration, so a latency reading is the
// difference the probe takes between the two Now calls around its Run.
type hostFake struct {
	output map[string]string
	fail   map[string]error
	took   map[string]time.Duration
	now    time.Time
}

// hostFixtureEnv answers every probe with its captured output. bd takes 37 ms
// on the fake clock; the others take none.
func hostFixtureEnv(startAt time.Time) *hostFake {
	return &hostFake{
		output: map[string]string{
			"/usr/bin/top": hostTopFixture,
			"bd":           hostBDFixture,
			"lsof":         hostLsofFixture,
			"docker":       hostDFFixture,
		},
		fail: map[string]error{},
		took: map[string]time.Duration{"bd": 37 * time.Millisecond},
		now:  startAt,
	}
}

func (f *hostFake) env() Env {
	return Env{
		Town: "/town",
		Run: func(_ context.Context, argv []string) ([]byte, error) {
			f.now = f.now.Add(f.took[argv[0]])
			if err := f.fail[argv[0]]; err != nil {
				return nil, err
			}
			return []byte(f.output[argv[0]]), nil
		},
		Now: func() time.Time { return f.now },
	}
}

func TestSourceHostCollectsTheFourColumns(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))

	got, err := newHostSource().Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"cpu_idle_pct":        86.99,
		"bd_latency_ms":       float64(37),
		"dolt_conns":          2,
		"docker_disk_pct":     59.0,
		"host_probe_failures": 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Collect = %v, want %v", got, want)
	}
}

func TestSourceHostParsers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		parse   func(string) (any, error)
		out     string
		want    any
		wantErr bool
	}{
		{"top reads the last sample", parseCPUIdlePct, hostTopFixture, 86.99, false},
		{"top without a CPU line", parseCPUIdlePct, "Processes: 641 total\n", nil, true},
		{"top CPU line without idle", parseCPUIdlePct, "CPU usage: 10.42% user, 5.29% sys\n", nil, true},
		{"lsof counts each connection once", parseDoltConns, hostLsofFixture, 2, false},
		{"lsof with no connections", parseDoltConns, hostLsofHeader, 0, false},
		{"lsof printing nothing", parseDoltConns, "", nil, true},
		{"lsof line that is not an established connection", parseDoltConns, hostLsofHeader + "dolt 41022 sloan 45u IPv4 0x8a TCP 127.0.0.1:3307 (LISTEN)\n", nil, true},
		{"df reads the root capacity", parseDockerDiskPct, hostDFFixture, 59.0, false},
		{"df without a root line", parseDockerDiskPct, "Filesystem 1024-blocks Used Available Capacity Mounted on\n", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.parse(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse(%q) = %v, want an error", tc.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q): %v", tc.out, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parse(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// An empty bd answer is no reading: the call returned, but nothing answered.
func TestSourceHostEmptyLatencyAnswerLeavesItsColumnNil(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	fake.output["bd"] = ""

	got, err := newHostSource().Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["bd_latency_ms"] != nil {
		t.Errorf("bd_latency_ms = %v, want nil when bd printed nothing", got["bd_latency_ms"])
	}
	if got["host_probe_failures"] != 1 {
		t.Errorf("host_probe_failures = %v, want 1 for the empty answer", got["host_probe_failures"])
	}
}

// Each probe is failed in turn: the columns of the commands that answered stay
// filled, so one broken command costs one reading.
func TestSourceHostFailedCommandLeavesOnlyThatColumnNil(t *testing.T) {
	t.Parallel()
	for _, probe := range newHostSource().probes() {
		t.Run(probe.column, func(t *testing.T) {
			t.Parallel()
			fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
			fake.fail[probe.argv[0]] = errors.New("command failed")

			got, err := newHostSource().Collect(context.Background(), fake.env())
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got[probe.column] != nil {
				t.Errorf("%s = %v, want nil when its command fails", probe.column, got[probe.column])
			}
			if got["host_probe_failures"] != 1 {
				t.Errorf("host_probe_failures = %v, want 1", got["host_probe_failures"])
			}
			for _, other := range newHostSource().probes() {
				if other.column != probe.column && got[other.column] == nil {
					t.Errorf("%s = nil, want it filled while only %s failed", other.column, probe.column)
				}
			}
		})
	}
}

// Every probe runs under its own hostCommandTimeout, counted from when it
// starts, so a wedged command costs its column and no more. The check reads the
// deadline the probe passed to Run; no test waits out the five seconds.
func TestSourceHostEachProbeRunsUnderItsOwnDeadline(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	env := fake.env()
	answer := env.Run
	before := time.Now()
	env.Run = func(ctx context.Context, argv []string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		switch {
		case !ok:
			t.Errorf("%s ran with no deadline", argv[0])
		case deadline.Before(before.Add(hostCommandTimeout)):
			t.Errorf("%s deadline %v is sooner than hostCommandTimeout after the snapshot began", argv[0], deadline)
		case deadline.After(time.Now().Add(hostCommandTimeout)):
			t.Errorf("%s deadline %v is later than hostCommandTimeout from now", argv[0], deadline)
		}
		if argv[0] == "lsof" {
			// The command that missed its deadline: the probe must read it as nil.
			return nil, context.DeadlineExceeded
		}
		return answer(ctx, argv)
	}

	got, err := newHostSource().Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["dolt_conns"] != nil || got["host_probe_failures"] != 1 {
		t.Errorf("dolt_conns = %v, host_probe_failures = %v, want nil and 1 for a command that missed its deadline", got["dolt_conns"], got["host_probe_failures"])
	}
	if got["cpu_idle_pct"] == nil || got["bd_latency_ms"] == nil || got["docker_disk_pct"] == nil {
		t.Errorf("Collect nulled a column whose command answered: %v", got)
	}
}

// Output a parser cannot read is one nil column and one counted failure, never
// a null group and never a wrong reading: a command whose format changed shows
// as a gap in its own column while the other three stay filled.
func TestSourceHostUnreadableOutputNilsOnlyItsColumnAndCountsIt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, command, output, column string
	}{
		{name: "top with no CPU line", command: "/usr/bin/top", output: "Processes: 641 total\n", column: "cpu_idle_pct"},
		{name: "lsof with a line that is no connection", command: "lsof", output: hostLsofHeader + "dolt 41022 sloan 45u IPv4 0x1 0t0 TCP *:3307 (LISTEN)\n", column: "dolt_conns"},
		{name: "lsof that printed nothing at all", command: "lsof", output: "", column: "dolt_conns"},
		{name: "df with no root line", command: "docker", output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n", column: "docker_disk_pct"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
			fake.output[tc.command] = tc.output

			got, err := newHostSource().Collect(context.Background(), fake.env())
			if err != nil {
				t.Fatalf("Collect returned %v; an unreadable probe must not null the group", err)
			}
			if got[tc.column] != nil || got["host_probe_failures"] != 1 {
				t.Errorf("%s = %v, host_probe_failures = %v, want nil and 1", tc.column, got[tc.column], got["host_probe_failures"])
			}
			for _, probe := range newHostSource().probes() {
				if probe.column != tc.column && got[probe.column] == nil {
					t.Errorf("%s = nil, want it filled while only %s was unreadable", probe.column, tc.column)
				}
			}
		})
	}
}

// A command that never returns is cut at the probe's own deadline. The fake
// blocks until its context ends; the caller's context stays alive, so the
// deadline that freed it can only be the probe's own timeout. Nothing sleeps:
// the wait is the 20 ms deadline.
func TestSourceHostAHungCommandIsCutAtItsOwnDeadline(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	env := fake.env()
	answer := env.Run
	hung := 0
	env.Run = func(ctx context.Context, argv []string) ([]byte, error) {
		if argv[0] == "lsof" {
			hung++
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return answer(ctx, argv)
	}
	caller, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	src := newHostSource()
	src.timeout = 20 * time.Millisecond
	got, err := src.Collect(caller, env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if hung != 1 {
		t.Fatalf("the hung command ran %d times, want once", hung)
	}
	if caller.Err() != nil {
		t.Fatalf("the caller's context ended (%v): the probe's own deadline did not free the command", caller.Err())
	}
	if got["dolt_conns"] != nil || got["host_probe_failures"] != 1 {
		t.Errorf("dolt_conns = %v, host_probe_failures = %v, want nil and 1", got["dolt_conns"], got["host_probe_failures"])
	}
	if got["cpu_idle_pct"] == nil || got["bd_latency_ms"] == nil || got["docker_disk_pct"] == nil {
		t.Errorf("the hung command cost more than its own column: %v", got)
	}
}

// Every docker command names the desktop-linux context, so a host whose global
// context points at a kind cluster still reads the Docker Desktop VM.
func TestSourceHostDockerArgvNamesTheContext(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, probe := range newHostSource().probes() {
		if probe.argv[0] != "docker" {
			continue
		}
		checked++
		if head := strings.Join(probe.argv[:3], " "); head != "docker --context desktop-linux" {
			t.Errorf("%s argv = %q, want it to start docker --context desktop-linux", probe.column, strings.Join(probe.argv, " "))
		}
	}
	if checked == 0 {
		t.Fatal("no docker probe to check")
	}
}

// The Dolt port and the container are the source's fields, so the commands
// read the ones the source was built with.
func TestSourceHostProbesUseTheConfiguredPortAndContainer(t *testing.T) {
	t.Parallel()
	host := hostSource{doltPort: "4321", dockerContainer: "runner-test"}
	argv := map[string][]string{}
	for _, probe := range host.probes() {
		argv[probe.argv[0]] = probe.argv
	}
	if got := strings.Join(argv["lsof"], " "); !strings.Contains(got, "-iTCP:4321") {
		t.Errorf("lsof argv = %q, want it to watch port 4321", got)
	}
	if got := strings.Join(argv["docker"], " "); !strings.Contains(got, "exec runner-test df") {
		t.Errorf("docker argv = %q, want it to exec in runner-test", got)
	}
}
