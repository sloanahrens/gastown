package hoststat

import (
	"context"
	"errors"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
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
			got := ParseCPUIdle(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("ParseCPUIdle = %v, want no reading", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("ParseCPUIdle = no reading, want %v", *tc.want)
			case tc.want != nil && math.Abs(*got-*tc.want) > 1e-9:
				t.Fatalf("ParseCPUIdle = %v, want %v", *got, *tc.want)
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
	top := ParseTop(ps, 5)
	if len(top) != 2 || top[0].Name != "go" || top[0].CPU < 135 || top[1].Name != "dolt" {
		t.Fatalf("ParseTop = %+v", top)
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
// sample (gt-kc8v1).
func TestSampleCPUIdleReadsTopAlone(t *testing.T) {
	t.Parallel()
	got, err := sampleCPUIdle(context.Background(), func(context.Context) ([]byte, error) { return []byte(macTop), nil })
	if err != nil || got != *ParseCPUIdle(macTop) {
		t.Fatalf("sampleCPUIdle = %v, %v, want the idle share of top's last CPU line", got, err)
	}
}

// A top cut short by its caller was not slow: the error says it was canceled,
// not that it outlived the sample window (gt-kc8v1).
func TestSampleCPUIdleNamesACancelledTopApartFromATimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := sampleCPUIdle(ctx, func(ctx context.Context) ([]byte, error) {
		cancel()
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	})
	if err == nil || !strings.Contains(err.Error(), "canceled") || strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v, want a cancellation, not a timeout", err)
	}
}

// A top that cannot be read says why: a failed run, a timeout and an output
// with no idle share are three different faults to the operator reading the
// daemon log (gt-kc8v1).
func TestSampleCPUIdleNamesWhyTopCouldNotBeRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		top     func(context.Context) ([]byte, error)
		wantErr string
	}{
		{"top fails", func(context.Context) ([]byte, error) { return nil, errors.New("refused") }, "top failed: refused"},
		{"top outlives the sample window", func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }, "did not finish"},
		{"top prints no idle share", func(context.Context) ([]byte, error) { return []byte("garbage\n"), nil }, "no CPU idle share"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The caller's own deadline stands in for sampleTimeout.
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err := sampleCPUIdle(ctx, tt.top)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// linuxProcTop holds two samples of `top -b -c -n 2 -d 1 -w 512`. A Dolt
// server that has run for days is near idle in the first sample, which is its
// average since it started, and burning four cores in the second (gt-avsc3).
const linuxProcTop = `top - 17:35:36 up 3 days,  2:14,  4 users,  load average: 4.08, 3.99, 3.88
Tasks: 512 total,   6 running, 506 sleeping
%Cpu(s):  6.2 us, 12.5 sy, 81.2 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st

    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND
   4242 sloan     20   0   12.1g   2.0g  40000 S   3.1   1.6 900:12.34 /usr/local/bin/dolt sql-server --config /gt/.dolt-data/config.yaml
      1 root      20   0  168000  12000   8000 S   0.0   0.0   0:09.00 /sbin/init splash

top - 17:35:37 up 3 days,  2:14,  4 users,  load average: 4.08, 3.99, 3.88
Tasks: 512 total,   6 running, 506 sleeping
%Cpu(s): 18.2 us,  3.5 sy, 78.2 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st

    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND
   4242 sloan     20   0   12.1g   2.0g  40000 S 398,5   1.6 900:16.34 /usr/local/bin/dolt sql-server --config /gt/.dolt-data/config.yaml
      1 root      20   0  168000  12000   8000 S   0.0   0.0   0:09.00 /sbin/init splash
`

// ps pcpu on Linux is CPU time over a process's whole life, so a Dolt server
// that spun for hours after days idle (or the reverse) reads as its average.
// The Linux process table must come from two samples a second apart, and the
// reading that reaches the parsers must be the second sample's rate; macOS,
// whose pcpu is a recent-usage figure, keeps ps (gt-avsc3).
func TestPsOutputLinuxUsesInstantCPU(t *testing.T) {
	t.Parallel()

	name, args := procTableCommand("linux")
	if name != "top" || !slices.Contains(args, "-b") || !slices.Contains(args, "-c") {
		t.Fatalf("Linux process table command = %s %v, want a batch top with full command lines", name, args)
	}
	if i := slices.Index(args, "-n"); i < 0 || i+1 >= len(args) || args[i+1] != "2" {
		t.Fatalf("Linux process table command = %s %v, want two samples so the second is a current rate", name, args)
	}
	if name, args := procTableCommand("darwin"); name != "ps" || !slices.Equal(args, []string{"-axo", "pcpu=,command="}) {
		t.Fatalf("macOS process table command = %s %v, want the unchanged ps", name, args)
	}

	out, err := procTable(linuxProcTop)
	if err != nil {
		t.Fatalf("procTable: %v", err)
	}
	got := parseDoltCores(string(out))
	if got == nil || math.Abs(*got-3.985) > 1e-9 {
		t.Fatalf("DoltCores from the second sample = %v, want 3.985", got)
	}
	if top := ParseTop(string(out), 5); len(top) != 1 || top[0].Name != "dolt" || top[0].CPU != 398.5 {
		t.Fatalf("ParseTop from the second sample = %+v, want dolt at 398.5", top)
	}
}

// A top whose process table cannot be read must fail the probe, so Sample
// leaves DoltCores and Top unset instead of reporting a healthy-looking zero.
func TestProcTableRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"no process table":        "Tasks: 1 total\n",
		"no COMMAND column":       "    PID USER  %CPU\n   1 root   3.0\n",
		"no CPU column":           "    PID USER COMMAND\n   1 root /sbin/init\n",
		"COMMAND is not the last": "    PID USER  %CPU COMMAND  TIME+\n   1 root   3.0 init 0:09.00\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if out, err := procTable(in); err == nil {
				t.Fatalf("procTable = %q, nil error, want one", out)
			}
		})
	}
}

// A row whose CPU field is not a number is dropped, not passed on as a
// reading.
func TestProcTableDropsRowsWithoutANumericCPU(t *testing.T) {
	t.Parallel()
	const in = "    PID USER      %CPU COMMAND\n   10 root       n/a broken\n   11 root      12,5 dolt sql-server --config /x\n"
	out, err := procTable(in)
	if err != nil {
		t.Fatalf("procTable: %v", err)
	}
	if got, want := string(out), "12.5 dolt sql-server --config /x\n"; got != want {
		t.Fatalf("procTable = %q, want %q", got, want)
	}
}

// The Linux process table is a ~1s sample, so a canceled context must end it
// with an error instead of letting it run on.
func TestPsOutputEndsWithItsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := psOutput(ctx); err == nil {
		t.Fatalf("psOutput on a canceled context = %q, nil error, want one", out)
	}
}

// A port flag naming the town's own port is the town's server started by
// hand, not a test's, and keeps counting.
func TestParseDoltCoresKeepsAServerOnTheProductionPort(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"--port value": "  200.0 dolt sql-server --port 3307 --data-dir /gt/.dolt-data\n",
		"--port=value": "  200.0 dolt sql-server --port=3307\n",
		"-P value":     "  200.0 dolt sql-server -P 3307\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := parseDoltCores(line); got == nil || math.Abs(*got-2) > 1e-9 {
				t.Fatalf("parseDoltCores = %v, want 2", got)
			}
		})
	}
}

// A test's own sql-server on a temporary data dir or its own port is not the
// town's Dolt, so a suite starting several does not inflate the figure the
// host tile compares against the core count (gt-avsc3).
func TestParseDoltCoresIgnoresTestServers(t *testing.T) {
	t.Parallel()
	const town = "  40.0 /opt/homebrew/bin/dolt sql-server --config /Users/sloan/gt/.dolt-data/config.yaml\n"
	for name, test := range map[string]string{
		"temporary data dir":            "  95.0 dolt sql-server --data-dir /tmp/TestFoo123/001/data\n",
		"temporary data dir as =":       "  95.0 dolt sql-server --data-dir=/private/var/folders/ab/xyz/T/TestFoo/data\n",
		"temporary config":              "  95.0 /usr/bin/dolt sql-server --config /var/folders/ab/xyz/T/gt-test/config.yaml\n",
		"explicit port":                 "  95.0 dolt sql-server --port 33307 --data-dir /x\n",
		"explicit port as =":            "  95.0 dolt sql-server --port=33307\n",
		"other port beside --config":    "  95.0 dolt sql-server --config /x/config.yaml --port 3308\n",
		"explicit short port":           "  95.0 dolt sql-server -P 33307\n",
		"under the process's TMPDIR":    "  95.0 dolt sql-server --data-dir " + strings.TrimSuffix(os.TempDir(), "/") + "/t1/data\n",
		"global flag before sql-server": "  95.0 dolt --data-dir /tmp/TestFoo123/001/data sql-server\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := parseDoltCores(town + test)
			if got == nil || math.Abs(*got-0.4) > 1e-9 {
				t.Fatalf("parseDoltCores = %v, want only the town's 0.4", got)
			}
			if only := parseDoltCores(test); only != nil {
				t.Fatalf("parseDoltCores of a test server alone = %v, want no reading", *only)
			}
		})
	}
}

// The Dolt server is whatever the shared argv matcher says it is: a global
// flag before sql-server still counts, and a process that only mentions the
// words does not.
func TestParseDoltCoresUsesSharedMatcher(t *testing.T) {
	t.Parallel()
	got := parseDoltCores("  200.0 dolt --config x sql-server\n  90.0 tail -f dolt-sql-server.log\n")
	if got == nil || math.Abs(*got-2) > 1e-9 {
		t.Fatalf("parseDoltCores = %v, want 2", got)
	}
}

// isTestServer reads the argv doltargv.IsSQLServer reads, binary first: a
// global flag moves where the subcommand sits but not the token after a port
// flag, so one slice answers both questions about one process (gt-gdqok).
func TestIsTestServerReadsTheArgvTheMatcherReads(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		argv []string
		want bool
	}{
		"town config": {
			argv: []string{"dolt", "sql-server", "--config", "/Users/x/gt/.dolt-data/config.yaml"},
		},
		"town config, global flag": {
			argv: []string{"dolt", "--config", "/Users/x/gt/.dolt-data/config.yaml", "sql-server"},
		},
		"town port named": {
			argv: []string{"dolt", "sql-server", "--port", "3307"},
		},
		"no arguments": {
			argv: []string{"dolt", "sql-server"},
		},
		"temporary data dir": {
			argv: []string{"dolt", "sql-server", "--data-dir", "/tmp/TestFoo123/001/data"},
			want: true,
		},
		"temporary data dir, global flag": {
			argv: []string{"dolt", "--data-dir", "/tmp/TestFoo123/001/data", "sql-server"},
			want: true,
		},
		"the test's own port": {
			argv: []string{"dolt", "sql-server", "--port", "33307"},
			want: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isTestServer(tc.argv); got != tc.want {
				t.Fatalf("isTestServer(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}
