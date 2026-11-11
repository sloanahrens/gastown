package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/hostprobe"
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
dolt    41022   operator 45u  IPv4 0x8a1b2c3d4e5f6071      0t0  TCP 127.0.0.1:3307->127.0.0.1:52946 (ESTABLISHED)
dolt    41022   operator 46u  IPv4 0x8a1b2c3d4e5f6082      0t0  TCP 127.0.0.1:3307->127.0.0.1:53012 (ESTABLISHED)
bd      41099   operator  5u  IPv4 0x7f0c1d2e3f405162      0t0  TCP 127.0.0.1:52946->127.0.0.1:3307 (ESTABLISHED)
bd      41112   operator  5u  IPv4 0x7f0c1d2e3f405173      0t0  TCP 127.0.0.1:53012->127.0.0.1:3307 (ESTABLISHED)
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
// bead read takes "bd"'s time and fails when "bd" fails; firstRead, when set,
// is what the store's first read costs, standing in for the seam's per-process
// setup.
type hostFake struct {
	output    map[string]string
	fail      map[string]error
	took      map[string]time.Duration
	now       time.Time
	firstRead time.Duration
	reads     int
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
	took := s.f.took["bd"]
	if s.f.reads == 0 && s.f.firstRead > 0 {
		took = s.f.firstRead
	}
	s.f.reads++
	s.f.now = s.f.now.Add(took)
	if err := s.f.fail["bd"]; err != nil {
		return nil, err
	}
	return nil, nil
}

func (s hostStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) { return nil, nil }
func (s hostStore) Blocked() ([]*beads.Issue, error)                         { return nil, nil }

func (s hostStore) ShowMultiple([]string) (map[string]*beads.Issue, error) { return nil, nil }

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
		{"lsof line that is not an established connection", parseDoltConns, hostLsofHeader + "dolt 41022 operator 45u IPv4 0x8a TCP 127.0.0.1:3307 (LISTEN)\n", nil, true},
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
	if len(fake.dirs) != 2 || fake.dirs[0] != "/town" || fake.dirs[1] != "/town" {
		t.Errorf("the bead reads opened stores for %v, want the town root for the warm-up and again for the timed read", fake.dirs)
	}
}

// The warm-up and the timed read get a deadline each: the warm-up's store is
// opened with a context that has finished before the timed read's store is
// opened, and the timed read's deadline is later than the warm-up's, so a
// warm-up that spends its whole budget cannot shorten what the timed read is
// allowed.
func TestSourceHostWarmUpEndsBeforeTheTimedReadStarts(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	env := fake.env()
	openStore := env.Beads
	var opened []context.Context
	env.Beads = func(ctx context.Context, dir string) BeadStore {
		opened = append(opened, ctx)
		return openStore(ctx, dir)
	}

	if _, err := (hostSource{}).Collect(context.Background(), env); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(opened) != 2 {
		t.Fatalf("the bead column opened %d store(s), want one for the warm-up and one for the timed read", len(opened))
	}
	select {
	case <-opened[0].Done():
	default:
		t.Error("the warm-up's store was still live when the timed read opened")
	}
	warm, ok := opened[0].Deadline()
	if !ok {
		t.Fatal("the warm-up ran with no deadline")
	}
	timed, ok := opened[1].Deadline()
	if !ok {
		t.Fatal("the timed read ran with no deadline")
	}
	if !timed.After(warm) {
		t.Errorf("the timed read's deadline %v is not after the warm-up's %v: the two share one window", timed, warm)
	}
}

// The reading is one steady-state read: the seam's first-use setup is spent
// before the clock starts, so a slow first call does not inflate bd_latency_ms.
func TestSourceHostLatencyExcludesTheSeamsFirstUse(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	fake.firstRead = 1200 * time.Millisecond

	got, err := newHostSource().Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["bd_latency_ms"] != 37.0 {
		t.Errorf("bd_latency_ms = %v, want 37 for the read after the seam warm-up, not the first read's 1237", got["bd_latency_ms"])
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
		{name: "lsof with a line that is no connection", command: "lsof", output: hostLsofHeader + "dolt 41022 operator 45u IPv4 0x1 0t0 TCP *:3307 (LISTEN)\n", column: "dolt_conns"},
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

// TestRunnerDefaultNamesTheProbeContainer: the docker column newHostSource
// builds execs into the container hostprobe names for the Docker Desktop disk,
// not the runner container retired with the Docker Forgejo stack. It reads the
// production probes, so a default changed at the call site is caught.
func TestRunnerDefaultNamesTheProbeContainer(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, probe := range newHostSource().probes() {
		if probe.column != "docker_disk_pct" {
			continue
		}
		checked++
		argv := strings.Join(probe.argv, " ")
		if !strings.Contains(argv, hostprobe.DockerDiskContainer) {
			t.Errorf("docker argv = %q, want it to name %s", argv, hostprobe.DockerDiskContainer)
		}
		if strings.Contains(argv, "forgejo-runner-daemon-1") {
			t.Errorf("docker argv = %q names the retired runner container", argv)
		}
	}
	if checked != 1 {
		t.Fatalf("checked %d docker probes, want 1", checked)
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

// hostProbeFileFixture is the probe file the host agent writes at the given
// time: an idle share and the Docker Desktop disk's use, with no field in error.
func hostProbeFileFixture(at time.Time) hostprobe.Probes {
	return hostprobe.Probes{
		At:         at,
		CPUIdlePct: hostprobe.Field[float64]{Value: 71.5},
		DockerDisk: hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: 83}},
	}
}

// writeHostProbesFile publishes p under the temp town as the host agent does.
func writeHostProbesFile(t *testing.T, town string, p hostprobe.Probes) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(hostprobe.Path(town)), 0o755); err != nil {
		t.Fatalf("creating the probe directory: %v", err)
	}
	if err := hostprobe.Write(hostprobe.Path(town), p); err != nil {
		t.Fatalf("writing the probe file: %v", err)
	}
}

// containerEnv is the fixture's env for a snapshot in the town's container: the
// host facts come from the file under town, and a host command is a failure.
func containerEnv(t *testing.T, fake *hostFake, town string, cgroup podCgroup) Env {
	t.Helper()
	env := fake.env()
	env.Town = town
	env.HostProbes = hostProbesFromFile(town)
	env.Run = func(_ context.Context, argv []string) ([]byte, error) {
		t.Errorf("container mode ran the host command %q", strings.Join(argv, " "))
		return nil, errors.New("no host command may run in container mode")
	}
	cgroup.install(&env, fake)
	return env
}

// podCgroup is the pod's cgroup as the CPU sample reads it. An empty cpuMax or
// stat entry is a file that does not exist. The first cpu.stat read answers
// stat[0]; once the window has passed, the next answers stat[1]. The window
// moves the fake clock by its length, unless frozen, which leaves the clock
// where it was. The noWait and noReadFile flags leave those seams unwired.
type podCgroup struct {
	cpuMax     string
	stat       [2]string
	frozen     bool
	noWait     bool
	noReadFile bool
}

// hostPodCgroupHalfBusy is one core over a one-second window in which half a
// second of CPU was burned: the pod reads 50% idle.
var hostPodCgroupHalfBusy = podCgroup{
	cpuMax: "100000 100000",
	stat:   [2]string{"usage_usec 1000000\n", "usage_usec 1500000\n"},
}

// install wires the cgroup files and the window onto env.
func (c podCgroup) install(env *Env, f *hostFake) {
	sampled := false
	env.ReadFile = func(path string) ([]byte, error) {
		var body string
		switch path {
		case podCPUMaxPath:
			body = c.cpuMax
		case podCPUStatPath:
			body = c.stat[0]
			if sampled {
				body = c.stat[1]
			}
		}
		if body == "" {
			return nil, os.ErrNotExist
		}
		return []byte(body), nil
	}
	env.Wait = func(d time.Duration) {
		sampled = true
		if !c.frozen {
			f.now = f.now.Add(d)
		}
	}
	if c.noWait {
		env.Wait = nil
	}
	if c.noReadFile {
		env.ReadFile = nil
	}
}

// TestSnapshotReadsHostFactsFromProbesInContainerMode: with the town's
// supervisor set to the container, the host group is the host agent's file and
// none of the Mac's commands runs there.
func TestSnapshotReadsHostFactsFromProbesInContainerMode(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)
	town := t.TempDir()
	writeHostProbesFile(t, town, hostProbeFileFixture(at))
	env := containerEnv(t, hostFixtureEnv(at), town, hostPodCgroupHalfBusy)

	rec := Collect(context.Background(), env, []Source{newHostSource()}, noDeadline)
	got := rec.Groups["host"]
	// load1 and dolt_conns have no fact in the file and the container's own
	// would name the container, so they stay unknown. cpu_idle_pct is the pod's
	// cgroup's 50%, not the file's 71.5, which the host agent measured on the Mac.
	want := map[string]any{
		"load1":               nil,
		"cpu_idle_pct":        50.0,
		"bd_latency_ms":       float64(37),
		"dolt_conns":          nil,
		"docker_disk_pct":     83.0,
		"host_probe_failures": 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("host group = %v, want the pod cgroup's CPU and the probe file's disk %v", got, want)
	}
}

// TestSnapshotUnderLaunchdIsUnchanged holds the host group to the expectations
// TestSourceHostCollectsTheFiveColumns had before container mode existed: with
// no probe seam the source runs the host's own commands.
func TestSnapshotUnderLaunchdIsUnchanged(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC))
	env := fake.env()
	// A town with no probe file: launchd must not look for one.
	env.Town = t.TempDir()

	rec := Collect(context.Background(), env, []Source{newHostSource()}, noDeadline)
	got := rec.Groups["host"]
	want := map[string]any{
		"load1":               13.77,
		"cpu_idle_pct":        86.99,
		"bd_latency_ms":       float64(37),
		"dolt_conns":          2,
		"docker_disk_pct":     59.0,
		"host_probe_failures": 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("host group = %v, want the commands' five columns %v", got, want)
	}
}

// TestSnapshotProbesMissingIsUnknown: a probe file the reader refuses, and a
// field the agent recorded as failed, leave the facts unknown rather than the
// zero each field decodes to.
func TestSnapshotProbesMissingIsUnknown(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)
	cpuFailed := hostProbeFileFixture(at)
	cpuFailed.CPUIdlePct = hostprobe.Field[float64]{Err: "top printed no CPU usage line"}
	dockerFailed := hostProbeFileFixture(at)
	dockerFailed.DockerDisk = hostprobe.Field[hostprobe.Disk]{Err: "df printed no line for the / filesystem"}
	tests := []struct {
		name         string
		write        bool
		probes       hostprobe.Probes
		cgroup       podCgroup
		wantCPUIdle  any
		wantDiskUsed any
		wantFailures int
	}{
		{name: "missing file", cgroup: hostPodCgroupHalfBusy, wantCPUIdle: 50.0, wantDiskUsed: nil, wantFailures: 1},
		{
			name:         "stale file",
			write:        true,
			probes:       hostProbeFileFixture(at.Add(-4 * time.Minute)),
			cgroup:       hostPodCgroupHalfBusy,
			wantCPUIdle:  50.0,
			wantDiskUsed: nil,
			wantFailures: 1,
		},
		{
			name:         "pod cpu.stat unreadable",
			write:        true,
			probes:       hostProbeFileFixture(at),
			cgroup:       podCgroup{cpuMax: "100000 100000"},
			wantCPUIdle:  nil,
			wantDiskUsed: 83.0,
			wantFailures: 1,
		},
		{
			name:         "probe file cpu field failed is not read",
			write:        true,
			probes:       cpuFailed,
			cgroup:       hostPodCgroupHalfBusy,
			wantCPUIdle:  50.0,
			wantDiskUsed: 83.0,
			wantFailures: 0,
		},
		{
			name:         "docker disk field failed",
			write:        true,
			probes:       dockerFailed,
			cgroup:       hostPodCgroupHalfBusy,
			wantCPUIdle:  50.0,
			wantDiskUsed: nil,
			wantFailures: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.write {
				writeHostProbesFile(t, town, tc.probes)
			}
			env := containerEnv(t, hostFixtureEnv(at), town, tc.cgroup)

			got, err := newHostSource().Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got["cpu_idle_pct"] != tc.wantCPUIdle || got["docker_disk_pct"] != tc.wantDiskUsed {
				t.Errorf("cpu_idle_pct = %v, docker_disk_pct = %v, want %v and %v (nil is unknown, never zero)",
					got["cpu_idle_pct"], got["docker_disk_pct"], tc.wantCPUIdle, tc.wantDiskUsed)
			}
			if got["host_probe_failures"] != tc.wantFailures {
				t.Errorf("host_probe_failures = %v, want %d", got["host_probe_failures"], tc.wantFailures)
			}
			if got["bd_latency_ms"] != 37.0 {
				t.Errorf("bd_latency_ms = %v, want 37: the store read is the container's own and still runs", got["bd_latency_ms"])
			}
		})
	}
}

// TestHostCPUReadsThePodCgroup: in container mode the host CPU share is the
// pod's cgroup, not the probe file's. The file says 71.5% idle, which is the
// host agent's reading of the Mac; the pod burned half a second of its one core
// in the window, so the column reads 50%. The disk is still the file's.
func TestHostCPUReadsThePodCgroup(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)
	town := t.TempDir()
	writeHostProbesFile(t, town, hostProbeFileFixture(at))
	env := containerEnv(t, hostFixtureEnv(at), town, hostPodCgroupHalfBusy)

	got, err := newHostSource().Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["cpu_idle_pct"] != 50.0 {
		t.Errorf("cpu_idle_pct = %v, want 50 from the pod's cgroup, not the probe file's 71.5", got["cpu_idle_pct"])
	}
	if got["docker_disk_pct"] != 83.0 {
		t.Errorf("docker_disk_pct = %v, want 83 from the probe file", got["docker_disk_pct"])
	}
}

// TestHostCPUIdleFromThePodCgroupSamples: the idle share is the window's burn
// over the cores the pod may use. Two cores with a quarter second burned are
// 87.5% idle; a burn past every core reads fully busy, never negative idle; a
// pod with no quota may use every core the node has.
func TestHostCPUIdleFromThePodCgroupSamples(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cgroup podCgroup
		want   float64
	}{
		{name: "one core half burned", cgroup: hostPodCgroupHalfBusy, want: 50},
		{
			name:   "two cores a quarter burned",
			cgroup: podCgroup{cpuMax: "200000 100000", stat: [2]string{"usage_usec 0\n", "usage_usec 250000\n"}},
			want:   87.5,
		},
		{
			name:   "a burn within the rounding tolerance reads fully busy",
			cgroup: podCgroup{cpuMax: "100000 100000", stat: [2]string{"usage_usec 0\n", "usage_usec 1010000\n"}},
			want:   0,
		},
		{
			name:   "no quota uses every core the node has",
			cgroup: podCgroup{cpuMax: "max 100000", stat: [2]string{"usage_usec 0\n", fmt.Sprintf("usage_usec %d\n", 500000*runtime.NumCPU())}},
			want:   50,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)
			town := t.TempDir()
			env := containerEnv(t, hostFixtureEnv(at), town, tc.cgroup)

			got, err := newHostSource().Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got["cpu_idle_pct"] != tc.want {
				t.Errorf("cpu_idle_pct = %v, want %v", got["cpu_idle_pct"], tc.want)
			}
		})
	}
}

// TestHostCPUIdleRefusesWhatItCannotRead: a pod CPU sample the cgroup cannot
// give is a nil column counted in host_probe_failures, never an idle share read
// from nothing. The disk column is the probe file's and is unaffected.
func TestHostCPUIdleRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	const halfStat = "usage_usec 1000000\n"
	tests := []struct {
		name   string
		cgroup podCgroup
		cancel bool
	}{
		{name: "cpu.max missing", cgroup: podCgroup{stat: hostPodCgroupHalfBusy.stat}},
		{name: "cpu.max names one field", cgroup: podCgroup{cpuMax: "100000", stat: hostPodCgroupHalfBusy.stat}},
		{name: "cpu.max quota is not a number", cgroup: podCgroup{cpuMax: "fast 100000", stat: hostPodCgroupHalfBusy.stat}},
		{name: "cpu.max period is not a number", cgroup: podCgroup{cpuMax: "100000 slow", stat: hostPodCgroupHalfBusy.stat}},
		{name: "cpu.max quota is zero", cgroup: podCgroup{cpuMax: "0 100000", stat: hostPodCgroupHalfBusy.stat}},
		{name: "cpu.stat missing", cgroup: podCgroup{cpuMax: "100000 100000"}},
		{name: "cpu.stat names no usage_usec", cgroup: podCgroup{cpuMax: "100000 100000", stat: [2]string{"usage_bytes 5\n", "usage_bytes 9\n"}}},
		{name: "usage_usec is not a number", cgroup: podCgroup{cpuMax: "100000 100000", stat: [2]string{"usage_usec many\n", "usage_usec 1500000\n"}}},
		{name: "cpu.stat unreadable after the window", cgroup: podCgroup{cpuMax: "100000 100000", stat: [2]string{"usage_usec 1000000\n", ""}}},
		{name: "usage fell between samples", cgroup: podCgroup{cpuMax: "100000 100000", stat: [2]string{"usage_usec 1500000\n", "usage_usec 1000000\n"}}},
		{name: "the window did not move", cgroup: podCgroup{cpuMax: "100000 100000", stat: [2]string{halfStat, "usage_usec 1500000\n"}, frozen: true}},
		{name: "no wait seam", cgroup: podCgroup{cpuMax: "100000 100000", stat: hostPodCgroupHalfBusy.stat, noWait: true}},
		{name: "no file seam", cgroup: podCgroup{cpuMax: "100000 100000", stat: hostPodCgroupHalfBusy.stat, noReadFile: true}},
		{name: "cancelled during the window", cgroup: hostPodCgroupHalfBusy, cancel: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)
			town := t.TempDir()
			writeHostProbesFile(t, town, hostProbeFileFixture(at))
			env := containerEnv(t, hostFixtureEnv(at), town, tc.cgroup)
			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			got, err := newHostSource().Collect(ctx, env)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got["cpu_idle_pct"] != nil {
				t.Errorf("cpu_idle_pct = %v, want nil: an unread pod CPU is unknown, not a reading", got["cpu_idle_pct"])
			}
			if got["docker_disk_pct"] != 83.0 {
				t.Errorf("docker_disk_pct = %v, want the probe file's 83", got["docker_disk_pct"])
			}
			if got["host_probe_failures"] != 1 {
				t.Errorf("host_probe_failures = %v, want 1: the unread CPU is counted", got["host_probe_failures"])
			}
		})
	}
}

// TestHostCPUContainerWindowTheDeadlineCannotHoldFailsTheGroup: a deadline that
// leaves less than the window refuses the container's CPU reading before any
// cgroup file is read or any wait runs, as the host CPU group refuses it. The
// deadline is half a window away, so the refusal is the window's, not the
// probes' own timeout.
func TestHostCPUContainerWindowTheDeadlineCannotHoldFailsTheGroup(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)
	town := t.TempDir()
	writeHostProbesFile(t, town, hostProbeFileFixture(at))
	env := containerEnv(t, hostFixtureEnv(at), town, hostPodCgroupHalfBusy)
	readFile := env.ReadFile
	var statReads int
	env.ReadFile = func(path string) ([]byte, error) {
		if path == podCPUStatPath {
			statReads++
		}
		return readFile(path)
	}
	waited := false
	wait := env.Wait
	env.Wait = func(d time.Duration) {
		waited = true
		wait(d)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(hostCPUWindow/2))
	defer cancel()

	got, err := newHostSource().Collect(ctx, env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["cpu_idle_pct"] != nil {
		t.Errorf("cpu_idle_pct = %v, want nil: a window the deadline cannot hold is no reading", got["cpu_idle_pct"])
	}
	if got["host_probe_failures"] != 1 {
		t.Errorf("host_probe_failures = %v, want 1: the refused CPU reading is counted", got["host_probe_failures"])
	}
	if waited || statReads != 0 {
		t.Errorf("waited = %v, cpu.stat reads = %d, want neither: the wait cannot be cut short past the deadline", waited, statReads)
	}
}

// TestHostCPUContainerOvershootBeyondToleranceFails: a burn one point past fully
// busy is rounding and reads fully busy. A burn 50 points past it means the
// counter and the wall clock disagree, so the reading is an error that names
// both counters, and the column is nil rather than a clamped zero.
func TestHostCPUContainerOvershootBeyondToleranceFails(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)
	t.Run("one point past every core clamps to fully busy", func(t *testing.T) {
		t.Parallel()
		env := containerEnv(t, hostFixtureEnv(at), t.TempDir(), podCgroup{
			cpuMax: "100000 100000",
			stat:   [2]string{"usage_usec 0\n", "usage_usec 1010000\n"},
		})

		got, err := podCPUIdlePct(context.Background(), env)
		if err != nil {
			t.Fatalf("podCPUIdlePct: %v", err)
		}
		if got != 0 {
			t.Errorf("podCPUIdlePct = %v, want 0: the rounding overshoot is fully busy", got)
		}
	})
	t.Run("fifty points past every core is an error naming both counters", func(t *testing.T) {
		t.Parallel()
		env := containerEnv(t, hostFixtureEnv(at), t.TempDir(), podCgroup{
			cpuMax: "100000 100000",
			stat:   [2]string{"usage_usec 1000000\n", "usage_usec 2500000\n"},
		})

		got, err := podCPUIdlePct(context.Background(), env)
		if err == nil {
			t.Fatalf("podCPUIdlePct = %v, want an error: a burn 50 points past fully busy is not rounding", got)
		}
		for _, counter := range []string{"usage_usec", "wall clock"} {
			if !strings.Contains(err.Error(), counter) {
				t.Errorf("error %q does not name the %q counter", err, counter)
			}
		}
	})
}
