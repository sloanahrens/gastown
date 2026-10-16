package hoststat

import (
	"context"
	"errors"
	"math"
	"testing"
)

// macTop holds two real samples of `top -l 2 -n 0 -s 1` from the 24-core
// host of gt-auiji: the load averages sit far past the core count while the
// CPU line reads nearly all of the machine idle.
const macTop = `Processes: 898 total, 20 running, 878 sleeping, 5608 threads
2026/10/15 17:35:36
Load Avg: 32.08, 21.99, 19.88
CPU usage: 39.24% user, 60.75% sys, 0.0% idle
PhysMem: 110G used, 17G unused.

Processes: 933 total, 18 running, 1 stuck, 914 sleeping, 5816 threads
2026/10/15 17:35:37
Load Avg: 35.83, 22.93, 20.23
CPU usage: 32.20% user, 67.79% sys, 95.5% idle
PhysMem: 110G used, 17G unused.
`

// linuxTop holds two samples of `top -b -n 2 -d 1`, whose CPU line is a
// different shape and whose load line opens with the clock.
const linuxTop = `top - 17:35:36 up 3 days,  2:14,  4 users,  load average: 32.08, 21.99, 19.88
Tasks: 512 total,   6 running, 506 sleeping
%Cpu(s):  6.2 us, 12.5 sy, 81.2 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st

top - 17:35:37 up 3 days,  2:15,  4 users,  load average: 35.83, 22.93, 20.23
Tasks: 512 total,   6 running, 506 sleeping
%Cpu(s): 32.2 us, 67.7 sy,  0.1 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st
`

// The CPU idle read is the reading that replaces a thread-inflated load
// average, so its parse is pinned against both hosts' shapes and against the
// since-boot first sample of a two-sample top (gt-auiji).
func TestParseCPUIdle(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   string
		want *float64
	}{
		"macOS takes the sampled line, not the since-boot one": {in: macTop, want: ptr(95.5)},
		"Linux takes the sampled line, not the since-boot one": {in: linuxTop, want: ptr(0.1)},
		"macOS fully idle": {in: "CPU usage: 0.0% user, 0.0% sys, 100.0% idle\n", want: ptr(100.0)},
		"Linux in procps' older printf layout, unit on the field": {in: "%Cpu(s):  6.2%us, 12.5%sy, 81.2%id,  0.0%wa\n", want: ptr(81.2)},
		"Linux fully idle": {in: "%Cpu(s):  0.0 us,  0.0 sy, 100.0 id,  0.0 wa\n", want: ptr(100.0)},
		"no CPU line":      {in: "Load Avg: 1.0, 2.0, 3.0\n"},
		"no idle field":    {in: "CPU usage: 39.24% user, 60.75% sys\n"},
		"no idle reading":  {in: "CPU usage: 39.24% user, high% idle\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := parseCPUIdle(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("parseCPUIdle = %v, want no reading", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("parseCPUIdle = no reading, want %v", *tc.want)
			case tc.want != nil && math.Abs(*got-*tc.want) > 1e-9:
				t.Fatalf("parseCPUIdle = %v, want %v", *got, *tc.want)
			}
		})
	}
}

// The load averages come from the kernel's own counters rather than from top,
// so that a top the timeout kills costs the idle reading and not the load
// average beside it (gt-auiji). Both hosts' shapes are pinned.
func TestParseLoadAvg(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   string
		read func(string) (*float64, *float64, *float64)
		want [3]*float64
	}{
		"Linux": {in: "35.83 22.93 20.23 2/1234 56789\n", read: parseProcLoadavg,
			want: [3]*float64{ptr(35.83), ptr(22.93), ptr(20.23)}},
		"Linux with one field": {in: "35.83\n", read: parseProcLoadavg},
		"Linux with a word":    {in: "busy busy busy\n", read: parseProcLoadavg},
		"macOS": {in: "{ 35.83 22.93 20.23 }\n", read: parseSysctlLoadavg,
			want: [3]*float64{ptr(35.83), ptr(22.93), ptr(20.23)}},
		"macOS empty":      {in: "{ }\n", read: parseSysctlLoadavg},
		"macOS short":      {in: "{ 35.83 }\n", read: parseSysctlLoadavg},
		"macOS unparsable": {in: "{ 35.83 22.93 high }\n", read: parseSysctlLoadavg},
		"macOS without braces": {in: "35.83 22.93 20.23\n", read: parseSysctlLoadavg,
			want: [3]*float64{ptr(35.83), ptr(22.93), ptr(20.23)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l1, l5, l15 := tc.read(tc.in)
			got := [3]*float64{l1, l5, l15}
			for i := range got {
				switch {
				case tc.want[i] == nil && got[i] != nil:
					t.Fatalf("load %d = %v, want no reading", i, *got[i])
				case tc.want[i] != nil && got[i] == nil:
					t.Fatalf("load %d = no reading, want %v", i, *tc.want[i])
				case tc.want[i] != nil && math.Abs(*got[i]-*tc.want[i]) > 1e-9:
					t.Fatalf("load %d = %v, want %v", i, *got[i], *tc.want[i])
				}
			}
		})
	}
}

// Cores, not percent: comparing the Dolt server against the CPU count is
// what the reading is for, and load average per core cannot do that on a
// host whose load counts threads (gt-auiji).
func TestParseDoltCores(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   string
		want *float64
	}{
		"one server": {
			in:   " 384.8 /opt/homebrew/bin/go\n  38.2 /opt/homebrew/bin/dolt sql-server --config /x/config.yaml\n  12.0 git\n",
			want: ptr(0.382),
		},
		"a second server process is added": {
			in:   "  38.2 dolt sql-server\n  21.8 /usr/local/bin/dolt sql-server --config /x/config.yaml\n",
			want: ptr(0.6),
		},
		"no dolt process": {in: "  38.2 go build ./...\n"},
		"a name that only starts with dolt is another program": {in: "  38.2 doltserver sql-server\n"},
		"the client is not the server": {
			in: "  38.2 dolt sql -q 'select 1'\n  21.0 dolt backup sync\n",
		},
		"a dolt with no subcommand is not the server":    {in: "  38.2 dolt\n"},
		"another program's sql-server is not the server": {in: "  38.2 postgres sql-server\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := parseDoltCores(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("parseDoltCores = %v, want no reading", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("parseDoltCores = no reading, want %v", *tc.want)
			case tc.want != nil && math.Abs(*got-*tc.want) > 1e-9:
				t.Fatalf("parseDoltCores = %v, want %v", *got, *tc.want)
			}
		})
	}
}

func TestParseTop(t *testing.T) {
	t.Parallel()
	ps := " 95.0 /usr/local/go/bin/go build ./...\n 40.2 /usr/bin/go test ./x\n 30.0 /usr/bin/dolt sql-server\n  0.1 launchd\n"
	top := parseTop(ps, 5)
	if len(top) != 2 || top[0].Name != "go" || top[0].CPU < 135 || top[1].Name != "dolt" {
		t.Fatalf("parseTop = %+v", top)
	}
}

// A sample is one reading per probe, and a probe that fails is only its own
// reading: the load average comes from the kernel's counters rather than from
// top, so a top the timeout kills costs the idle share and nothing else. The
// whole sample is discarded where the probe that failed is the load, which is
// the one reading with no substitute (gt-auiji).
func TestSampleHoldsEachReadingAgainstItsOwnProbeFailing(t *testing.T) {
	t.Parallel()

	const ps = "  38.2 dolt sql-server --config /x/config.yaml\n"
	errf := func() error { return errors.New("refused") }
	load := func(context.Context) (*float64, *float64, *float64) {
		l1, l5, l15 := 30.0, 21.0, 19.0
		return &l1, &l5, &l15
	}
	cases := map[string]struct {
		p        probes
		wantIdle bool
		wantDolt bool
		wantLoad bool
	}{
		"every probe answers": {
			p:        probes{load: load, top: func(context.Context) ([]byte, error) { return []byte(macTop), nil }, ps: func(context.Context) ([]byte, error) { return []byte(ps), nil }},
			wantIdle: true, wantDolt: true, wantLoad: true,
		},
		"top fails": {
			p:        probes{load: load, top: func(context.Context) ([]byte, error) { return nil, errf() }, ps: func(context.Context) ([]byte, error) { return []byte(ps), nil }},
			wantDolt: true, wantLoad: true,
		},
		"ps fails": {
			p:        probes{load: load, top: func(context.Context) ([]byte, error) { return []byte(macTop), nil }, ps: func(context.Context) ([]byte, error) { return nil, errf() }},
			wantIdle: true, wantLoad: true,
		},
		"the load counters fail": {
			p:        probes{load: func(context.Context) (*float64, *float64, *float64) { return nil, nil, nil }, top: func(context.Context) ([]byte, error) { return []byte(macTop), nil }, ps: func(context.Context) ([]byte, error) { return []byte(ps), nil }},
			wantIdle: true, wantDolt: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := sample(context.Background(), tc.p)
			if (r.CPUIdle != nil) != tc.wantIdle {
				t.Errorf("CPUIdle = %v, want a reading: %v", r.CPUIdle, tc.wantIdle)
			}
			if (r.DoltCores != nil) != tc.wantDolt {
				t.Errorf("DoltCores = %v, want a reading: %v", r.DoltCores, tc.wantDolt)
			}
			if (r.Load1 != nil) != tc.wantLoad {
				t.Errorf("Load1 = %v, want a reading: %v", r.Load1, tc.wantLoad)
			}
			if r.Cores < 1 {
				t.Errorf("Cores = %d, want the host's count", r.Cores)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// SampleCPUIdle runs top alone and reads the last CPU line, the second
// sample; a top that fails leaves the reading nil (gt-kc8v1).
func TestSampleCPUIdleReadsTopAlone(t *testing.T) {
	t.Parallel()
	got := sampleCPUIdle(context.Background(), func(context.Context) ([]byte, error) { return []byte(macTop), nil })
	if got == nil || *got != *parseCPUIdle(macTop) {
		t.Fatalf("sampleCPUIdle = %v, want the idle share of top's last CPU line", got)
	}
	if got := sampleCPUIdle(context.Background(), func(context.Context) ([]byte, error) { return nil, errors.New("refused") }); got != nil {
		t.Errorf("sampleCPUIdle after a failed top = %v, want nil", *got)
	}
}
