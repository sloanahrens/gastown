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

// hostFake answers the probes with canned output and steps through a fixed
// clock, so no test starts a process or reads the wall clock.
type hostFake struct {
	output map[string]string
	fail   map[string]error
	times  []time.Time
	now    int
}

// hostFixtureEnv answers every probe with its captured output. The clock
// starts at startAt and every reading moves it forward by step.
func hostFixtureEnv(startAt time.Time, step time.Duration) *hostFake {
	return &hostFake{
		output: map[string]string{
			"/usr/bin/top": hostTopFixture,
			"bd":           hostBDFixture,
			"lsof":         hostLsofFixture,
			"docker":       hostDFFixture,
		},
		fail:  map[string]error{},
		times: []time.Time{startAt, startAt.Add(step)},
	}
}

func (f *hostFake) env() Env {
	return Env{
		Town: "/town",
		Run: func(_ context.Context, argv []string) ([]byte, error) {
			if err := f.fail[argv[0]]; err != nil {
				return nil, err
			}
			return []byte(f.output[argv[0]]), nil
		},
		Now: func() time.Time {
			if f.now >= len(f.times) {
				f.now++
				return f.times[len(f.times)-1]
			}
			at := f.times[f.now]
			f.now++
			return at
		},
	}
}

func TestSourceHostCollectsTheFourColumns(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC)
	fake := hostFixtureEnv(start, 37*time.Millisecond)

	got, err := (hostSource{}).Collect(context.Background(), fake.env())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"cpu_idle_pct":    86.99,
		"bd_latency_ms":   float64(37),
		"dolt_conns":      2,
		"docker_disk_pct": 59.0,
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

// Each probe is failed in turn: the columns of the commands that answered stay
// filled, so one broken command costs one reading.
func TestSourceHostFailedCommandLeavesOnlyThatColumnNil(t *testing.T) {
	t.Parallel()
	for _, probe := range hostProbes() {
		t.Run(probe.column, func(t *testing.T) {
			t.Parallel()
			fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC), time.Millisecond)
			fake.fail[probe.argv[0]] = errors.New("command failed")

			got, err := (hostSource{}).Collect(context.Background(), fake.env())
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got[probe.column] != nil {
				t.Errorf("%s = %v, want nil when its command fails", probe.column, got[probe.column])
			}
			for _, other := range hostProbes() {
				if other.column != probe.column && got[other.column] == nil {
					t.Errorf("%s = nil, want it filled while only %s failed", other.column, probe.column)
				}
			}
		})
	}
}

// A command that runs past its deadline is abandoned and its column is nil,
// the same as one that fails outright.
func TestSourceHostTimedOutCommandLeavesOnlyThatColumnNil(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC), time.Millisecond)
	env := fake.env()
	answer := env.Run
	env.Run = func(ctx context.Context, argv []string) ([]byte, error) {
		if argv[0] != "lsof" {
			return answer(ctx, argv)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	// The snapshot's own context is already over, so lsof's wait ends at once:
	// the test proves the deadline path without spending hostCommandTimeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := (hostSource{}).Collect(ctx, env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["dolt_conns"] != nil {
		t.Errorf("dolt_conns = %v, want nil for a command that missed its deadline", got["dolt_conns"])
	}
	if got["cpu_idle_pct"] == nil || got["bd_latency_ms"] == nil || got["docker_disk_pct"] == nil {
		t.Errorf("Collect nulled a column whose command answered: %v", got)
	}
}

// Output the parser cannot read nulls the group, so a command whose format
// changed is missing from the record rather than recorded as a wrong reading.
func TestSourceHostUnreadableOutputNullsTheGroup(t *testing.T) {
	t.Parallel()
	fake := hostFixtureEnv(time.Date(2026, 10, 24, 13, 4, 2, 0, time.UTC), time.Millisecond)
	fake.output["/usr/bin/top"] = "Processes: 641 total\n"

	got, err := (hostSource{}).Collect(context.Background(), fake.env())
	if err == nil {
		t.Fatal("Collect accepted top output with no CPU line")
	}
	if got != nil {
		t.Errorf("Collect = %v beside an error, want a null group", got)
	}
}

// Every docker command names the desktop-linux context, so a host whose global
// context points at a kind cluster still reads the Docker Desktop VM.
func TestSourceHostDockerArgvNamesTheContext(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, probe := range hostProbes() {
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
