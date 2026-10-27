package metrics

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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

// hostLoadFixture is what macOS's `sysctl -n vm.loadavg` prints: the three
// load averages inside braces.
const hostLoadFixture = "{ 13.77 12.84 11.49 }\n"

// hostFake answers the probes with canned output. Its clock moves only when a
// command runs, by that command's took duration, so a latency reading is the
// difference the probe takes between the two Now calls around its Run. The
// bead read takes "bd"'s time and fails when "bd" fails.
type hostFake struct {
	output map[string]string
	fail   map[string]error
	took   map[string]time.Duration
	now    time.Time
	// dirs records each directory a bead store was opened for.
	dirs []string
}

// hostFixtureEnv answers every probe with its captured output. bd takes 37 ms
// on the fake clock; the others take none.
func hostFixtureEnv(startAt time.Time) *hostFake {
	return &hostFake{
		output: map[string]string{
			"/usr/bin/top":     hostTopFixture,
			"/usr/sbin/sysctl": hostLoadFixture,
			"lsof":             hostLsofFixture,
			"docker":           hostDFFixture,
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
		Beads: func(_ context.Context, dir string) BeadStore {
			f.dirs = append(f.dirs, dir)
			return hostStore{f}
		},
		Now: func() time.Time { return f.now },
	}
}

// hostStore is the fixture's bead store: its one List read is the host
// probe's, and it takes bd's time on the fake clock.
type hostStore struct{ f *hostFake }

func (s hostStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	s.f.now = s.f.now.Add(s.f.took["bd"])
	if err := s.f.fail["bd"]; err != nil {
		return nil, err
	}
	return nil, nil
}

func (s hostStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) { return nil, nil }
func (s hostStore) Blocked() ([]*beads.Issue, error)                         { return nil, nil }

func TestSourceHostCollectsTheFiveColumns(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))

	got, err := newHostSource().Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"load1":               13.77,
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
		{"sysctl reads the one-minute load", parseLoadAvg1, hostLoadFixture, 13.77, false},
		{"sysctl output without braces", parseLoadAvg1, " 13.77 12.84 11.49 \n", 13.77, false},
		{"sysctl with one field", parseLoadAvg1, "{ 13.77 }\n", nil, true},
		{"sysctl with a field that is not a number", parseLoadAvg1, "{ one 12.84 11.49 }\n", nil, true},
		{"sysctl with a later field that is not a number", parseLoadAvg1, "{ 13.77 12.84 nope }\n", nil, true},
		{"sysctl printing nothing", parseLoadAvg1, "", nil, true},
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

// The bead read is a list of one issue in the town's own store: an empty board
// is a store that answered, so its time is still a reading.
func TestSourceHostEmptyBoardIsAReading(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))

	got, err := newHostSource().Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["bd_latency_ms"] != 37.0 {
		t.Errorf("bd_latency_ms = %v, want 37 for a read that answered", got["bd_latency_ms"])
	}
	if len(fake.dirs) != 1 || fake.dirs[0] != "/town" {
		t.Errorf("the bead read opened stores for %v, want the town root alone", fake.dirs)
	}
}

// A bead read the store could not answer is a gap, not a fast latency.
func TestSourceHostFailedStoreReadLeavesItsColumnNil(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	fake.fail["bd"] = errors.New("no Dolt server")

	got, err := newHostSource().Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["bd_latency_ms"] != nil {
		t.Errorf("bd_latency_ms = %v, want nil when the store did not answer", got["bd_latency_ms"])
	}
	if got["host_probe_failures"] != 1 {
		t.Errorf("host_probe_failures = %v, want 1 for the failed read", got["host_probe_failures"])
	}
}

// An Env with no bead store is a wiring error, and the group says so rather
// than report a host with no bd.
func TestSourceHostRefusesAnEnvWithNoStore(t *testing.T) {
	t.Parallel()
	env := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)).env()
	env.Beads = nil
	if cols, err := (hostSource{}).Collect(context.Background(), env); err == nil || cols != nil {
		t.Fatalf("Collect = %v, %v with no bead store; want a null group and an error", cols, err)
	}
}

// A factory that hands back no store leaves the bd column a counted gap
// rather than panicking the snapshot.
func TestSourceHostNilStoreLeavesItsColumnNil(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	env := fake.env()
	env.Beads = func(context.Context, string) BeadStore { return nil }

	got, err := newHostSource().Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["bd_latency_ms"] != nil || got["host_probe_failures"] != 1 {
		t.Errorf("bd_latency_ms = %v, host_probe_failures = %v, want nil and 1", got["bd_latency_ms"], got["host_probe_failures"])
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
			if probe.store {
				fake.fail["bd"] = errors.New("command failed")
			} else {
				fake.fail[probe.argv[0]] = errors.New("command failed")
			}

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
// starts, so a wedged read costs its column and no more. The check reads the
// deadline the probe passed to Run or to the store it opened; no test waits out
// the five seconds.
func TestSourceHostEachProbeRunsUnderItsOwnDeadline(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	env := fake.env()
	answer := env.Run
	openStore := env.Beads
	before := time.Now()
	checkDeadline := func(name string, ctx context.Context) {
		deadline, ok := ctx.Deadline()
		switch {
		case !ok:
			t.Errorf("%s ran with no deadline", name)
		case deadline.Before(before.Add(hostCommandTimeout)):
			t.Errorf("%s deadline %v is sooner than hostCommandTimeout after the snapshot began", name, deadline)
		case deadline.After(time.Now().Add(hostCommandTimeout)):
			t.Errorf("%s deadline %v is later than hostCommandTimeout from now", name, deadline)
		}
	}
	env.Run = func(ctx context.Context, argv []string) ([]byte, error) {
		checkDeadline(argv[0], ctx)
		if argv[0] == "lsof" {
			// The command that missed its deadline: the probe must read it as nil.
			return nil, context.DeadlineExceeded
		}
		return answer(ctx, argv)
	}
	env.Beads = func(ctx context.Context, dir string) BeadStore {
		checkDeadline("the bead read", ctx)
		return openStore(ctx, dir)
	}

	got, err := newHostSource().Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["dolt_conns"] != nil || got["host_probe_failures"] != 1 {
		t.Errorf("dolt_conns = %v, host_probe_failures = %v, want nil and 1 for a command that missed its deadline", got["dolt_conns"], got["host_probe_failures"])
	}
	if got["load1"] == nil || got["cpu_idle_pct"] == nil || got["bd_latency_ms"] == nil || got["docker_disk_pct"] == nil {
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
		{name: "sysctl with a truncated load line", command: "/usr/sbin/sysctl", output: "{ 13.77 }\n", column: "load1"},
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
	if got["load1"] == nil || got["cpu_idle_pct"] == nil || got["bd_latency_ms"] == nil || got["docker_disk_pct"] == nil {
		t.Errorf("the hung command cost more than its own column: %v", got)
	}
}

// The probes run one after another, each under hostCommandTimeout, so the
// group's worst case is one timeout per probe. It must fit the deadline the
// snapshot gives the group, or the last column is always a timeout and never a
// reading.
func TestSourceHostProbesFitTheGroupDeadline(t *testing.T) {
	t.Parallel()
	probes := newHostSource().probes()
	if worst := time.Duration(len(probes)) * hostCommandTimeout; worst >= SourceTimeout {
		t.Errorf("%d probes at %s each cost %s, want under the group's %s", len(probes), hostCommandTimeout, worst, SourceTimeout)
	}
}

// Every docker command names the desktop-linux context, so a host whose global
// context points at a kind cluster still reads the Docker Desktop VM.
func TestSourceHostDockerArgvNamesTheContext(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, probe := range newHostSource().probes() {
		if len(probe.argv) == 0 || probe.argv[0] != "docker" {
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
		if len(probe.argv) > 0 {
			argv[probe.argv[0]] = probe.argv
		}
	}
	if got := strings.Join(argv["lsof"], " "); !strings.Contains(got, "-iTCP:4321") {
		t.Errorf("lsof argv = %q, want it to watch port 4321", got)
	}
	if got := strings.Join(argv["docker"], " "); !strings.Contains(got, "exec runner-test df") {
		t.Errorf("docker argv = %q, want it to exec in runner-test", got)
	}
}
