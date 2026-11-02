package hostload

import (
	"reflect"
	"strings"
	"testing"
)

// cannedPS is `ps -axo pid=,command=` for the processes the table test
// attributes: one crew session's shell and build, two landing gates' builds,
// Dolt, a Docker VM, the installer, the daemon, the dashboard, a qemu VM, a
// polecat session's claude and a second crew session.
// Pid 112 is an unclassified process: no owner's directory names it, and its
// command names no owner.
const cannedPS = `    1 /sbin/launchd
  100 -zsh
  101 go test ./internal/hostload
  102 go build ./...
  103 /Users/op/.local/bin/dolt sql-server
  104 com.docker.virtualization --kernel /x/vmlinuz
  105 gt-installer --repo /Users/op/gt/gastown
  106 /Users/op/.local/bin/gt daemon run
  107 /Users/op/.local/bin/gt dashboard
  108 qemu-system-aarch64 -m 8G
  109 claude
  110 -zsh
  112 /usr/local/bin/indexer --watch
  111 go build ./...`

// cannedTop is `top -l 2 -s 1 -o cpu -stats pid,cpu`. The first table is each
// process's average since it started; the table the test reads is the second,
// the CPU over one sample window.
const cannedTop = `Processes: 400 total, 2 running, 398 sleeping
PID    %CPU
1      99.0
100    50.0
110    40.0

PID    %CPU
1      0.0
100    20.0
101    10.0
102    25.0
103    12.0
104    8.0
105    5.0
106    4.0
107    3.0
108    6.0
109    2.0
110    1.0
112    1.5
111    2.0`

// cannedLsof is `lsof -a -d cwd -Fpn` for the processes that used CPU. pid
// 108 is the shape lsof prints for a working directory it cannot read, a "p"
// record with no "n" line; pid 104 runs in the root directory.
const cannedLsof = `p100
n/Users/op/gt/gastown/crew/dunlin/gastown
p101
n/Users/op/gt/gastown/crew/dunlin/gastown
p102
n/private/tmp/gt-landing.noindex/gastown
p103
n/Users/op/gt
p104
n/
p105
n/Users/op/gt/gastown/mayor/rig
p106
n/Users/op/gt
p107
n/Users/op/gt
p108
p109
n/Users/op/gt/gastown/polecats/capable/gastown
p110
n/Users/op/gt/gastown/crew/egret/gastown
p112
n/Users/op/Library/indexer
p111
n/private/tmp/gt-landing-501/gastown`

// TestHostLoadClassifyOwners reads canned ps, top and lsof output and checks the CPU
// is credited to the owner that used it: a crew worktree, a landing gate tree,
// the installer, Dolt and a VM, plus the daemon, the dashboard and the rest.
func TestHostLoadClassifyOwners(t *testing.T) {
	t.Parallel()
	rows, err := Table(cannedPS, cannedTop, cannedLsof)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	want := []Row{
		{Owner: "crew/dunlin", CPU: 30, Procs: 2},
		{Owner: OwnerLanding, CPU: 27, Procs: 2},
		{Owner: OwnerDolt, CPU: 12, Procs: 1},
		{Owner: "vm/com.docker.virtualization", CPU: 8, Procs: 1},
		{Owner: "vm/qemu-system-aarch64", CPU: 6, Procs: 1},
		{Owner: OwnerInstaller, CPU: 5, Procs: 1},
		{Owner: OwnerDaemon, CPU: 4, Procs: 1},
		{Owner: OwnerDashboard, CPU: 3, Procs: 1},
		{Owner: "polecat/capable", CPU: 2, Procs: 1},
		{Owner: OwnerOther, CPU: 1.5, Procs: 1},
		{Owner: "crew/egret", CPU: 1, Procs: 1},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("Table:\n got %+v\nwant %+v", rows, want)
	}
}

// TestClassifyOwners checks a working directory names its owner: a polecat's
// worktree is polecat/<name>, a polecats directory with no name under it falls
// through to the command rules, and a directory naming no owner is other.
func TestClassifyOwners(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cwd     string
		command string
		want    string
	}{
		{"polecat worktree", "/Users/op/gt/gastown/polecats/capable/gastown", "claude", "polecat/capable"},
		{"polecat worktree with a build", "/Users/op/gt/gastown/polecats/capable/gastown", "go build ./...", "polecat/capable"},
		{"polecats directory with no name", "/Users/op/gt/gastown/polecats", "claude", OwnerOther},
		{"crew worktree", "/Users/op/gt/gastown/crew/dunlin/gastown", "claude", "crew/dunlin"},
		{"unclassified directory", "/Users/op/gt/gastown/mayor", "claude", OwnerOther},
	}
	for _, tc := range cases {
		if got := Classify(tc.cwd, tc.command); got != tc.want {
			t.Errorf("%s: Classify(%q, %q) = %q, want %q", tc.name, tc.cwd, tc.command, got, tc.want)
		}
	}
}

// TestHostLoadTableRefusesMalformedReadings checks the table reaches its parse errors:
// a reading it cannot parse is an error, never a table built from the rest.
func TestHostLoadTableRefusesMalformedReadings(t *testing.T) {
	t.Parallel()
	const goodPS = "    1 /sbin/launchd\n"
	const goodTop = "PID %CPU\n1 0.5\n"
	cases := []struct {
		name          string
		ps, top, lsof string
		want          string
	}{
		{name: "ps", ps: "nope /bin/zsh\n", top: goodTop, want: `not "pid command"`},
		{name: "top", ps: goodPS, top: "no table here\n", want: "no process table"},
		{name: "lsof", ps: goodPS, top: goodTop, lsof: "q1\n", want: "is not a pid or a path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Table(tc.ps, tc.top, tc.lsof)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Table = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestHostLoadParsersRefuseMalformedOutput drives each parser's refusal: a reading
// this package cannot read is an error, never an empty table or a zero.
func TestHostLoadParsersRefuseMalformedOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		parse func() error
		want  string
	}{
		{
			name:  "ps line with no command",
			parse: func() error { _, err := ParsePS("12\n"); return err },
			want:  `not "pid command"`,
		},
		{
			name:  "ps line with no pid",
			parse: func() error { _, err := ParsePS("nope /bin/zsh\n"); return err },
			want:  `not "pid command"`,
		},
		{
			name:  "top with no process table",
			parse: func() error { _, err := ParseTop("no table here\n"); return err },
			want:  "no process table",
		},
		{
			name:  "top row shorter than its header",
			parse: func() error { _, err := ParseTop("PID %CPU\n1234\n"); return err },
			want:  "shorter than its header",
		},
		{
			name:  "top row with no pid",
			parse: func() error { _, err := ParseTop("PID %CPU\nx 1.0\n"); return err },
			want:  "has no pid",
		},
		{
			name:  "top row with no cpu",
			parse: func() error { _, err := ParseTop("PID %CPU\n1234 abc\n"); return err },
			want:  "has no CPU",
		},
		{
			name:  "top cpu that is not a number",
			parse: func() error { _, err := ParseTop("PID %CPU\n1234 nan\n"); return err },
			want:  "has no CPU",
		},
		{
			name:  "top with no rows",
			parse: func() error { _, err := ParseTop("PID %CPU\n\n"); return err },
			want:  "held no rows",
		},
		{
			name:  "lsof path before any pid",
			parse: func() error { _, err := ParseLsof("n/tmp\n"); return err },
			want:  "before any pid",
		},
		{
			name:  "lsof line that is neither",
			parse: func() error { _, err := ParseLsof("p10\nq1\n"); return err },
			want:  "is not a pid or a path",
		},
		{
			name:  "lsof pid that is not a number",
			parse: func() error { _, err := ParseLsof("pzz\n"); return err },
			want:  "does not name a pid",
		},
		{
			name:  "load with two numbers",
			parse: func() error { _, err := parseLoad([]string{"1.0", "2.0"}); return err },
			want:  "want 3",
		},
		{
			name:  "negative load average",
			parse: func() error { _, err := parseLoad([]string{"-1.0", "2.0", "3.0"}); return err },
			want:  "not a number",
		},
		{
			name:  "load average that is not a number",
			parse: func() error { _, err := parseLoad([]string{"nan", "2.0", "3.0"}); return err },
			want:  "not a number",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.parse()
			if err == nil {
				t.Fatalf("parsed a reading it should have refused")
			}
			if got := err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to name %q", got, tc.want)
			}
		})
	}
}

// TestHostLoadParseErrorsCarryNoHostText checks a parse error names its rule
// and location without quoting the host's line: the error becomes the sample
// record's err field, and the host's process table does not belong there.
func TestHostLoadParseErrorsCarryNoHostText(t *testing.T) {
	t.Parallel()
	const marker = "SENTINEL"
	cases := []struct {
		name  string
		parse func() error
	}{
		{name: "ps", parse: func() error { _, err := ParsePS(marker + " " + marker + "\n"); return err }},
		{name: "top", parse: func() error { _, err := ParseTop("PID %CPU\n" + marker + " 1.0\n"); return err }},
		{name: "lsof", parse: func() error { _, err := ParseLsof("p10\n" + marker + "\n"); return err }},
		{name: "load", parse: func() error { _, err := parseLoad([]string{marker, "2.0", "3.0"}); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.parse()
			if err == nil {
				t.Fatalf("%s parser accepted %q", tc.name, marker)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error = %q, want it to carry none of the host's text", err)
			}
		})
	}
}

// TestHostLoadParseTickTook reads the tick duration off the daemon's line, and
// refuses text that carries none.
func TestHostLoadParseTickTook(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want float64
		ok   bool
	}{
		{
			name: "seconds",
			text: "spec_dispatch: tick took 1.234s (roster [a 1/1], board 2ms, candidates 0 1ms, sling 1ms); roster 1 calls",
			want: 1234,
			ok:   true,
		},
		{
			name: "milliseconds",
			text: "spec_dispatch: tick took 540ms (roster [a 1/1])",
			want: 540,
			ok:   true,
		},
		{name: "another line", text: "spec_dispatch: deferred", ok: false},
		{name: "no duration", text: "tick took soon", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTickTook(tc.text)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseTickTook(%q) = %v, %v; want %v, %v", tc.text, got, ok, tc.want, tc.ok)
			}
		})
	}
}
