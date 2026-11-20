package land

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// runnerTaskLine is the first line of a gate job log, as Forgejo's runner writes it.
const runnerTaskLine = "Runner 14b9572a-0fbc-4368-ac6b-63af30a5f49c (version:v13.2.0) " +
	"received task 20031 of job gate, triggered by event: push"

// TestParseGateRunLog reads a passing gate's log. Setup runs from the runner's task line to the
// first lint line, 16s; the repeated lint line at 10:00:17 leaves lint_s at 1.5.
func TestParseGateRunLog(t *testing.T) {
	t.Parallel()
	log := "2026-11-19T10:00:00Z " + runnerTaskLine + "\r\n" +
		"2026-11-19T10:00:16Z lint: golangci-lint run --timeout=5m\r\n" +
		"2026-11-19T10:00:17Z lint: hygiene scan\r\n" +
		"2026-11-19T10:00:17.5Z gate: build (go build ./...)\r\n" +
		"2026-11-19T10:00:21.5Z gate: unit tier (every package)\r\n" +
		"2026-11-19T10:02:30Z gate: PASSED in 139s wall\r\n"

	want := GateRun{SetupS: 16, LintS: 1.5, BuildS: 4, UnitWallS: 139, Passed: true}
	if got := ParseGateRunLog(log); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseGateRunLog() = %+v, want %+v", got, want)
	}
}

// TestParseGateRunLogFailed reads a failed gate's verdict: the wall comes from the line and passed
// is false.
func TestParseGateRunLogFailed(t *testing.T) {
	t.Parallel()
	log := "2026-11-19T10:00:00Z " + runnerTaskLine + "\r\n" +
		"2026-11-19T10:00:03Z lint: golangci-lint run\r\n" +
		"2026-11-19T10:00:05Z gate: build (go build ./...)\r\n" +
		"2026-11-19T10:00:09Z gate: unit tier (every package)\r\n" +
		"2026-11-19T10:02:28Z gate: FAILED at unit tier (Go suite, exit 1) after 139s wall\r\n"

	want := GateRun{SetupS: 3, LintS: 2, BuildS: 4, UnitWallS: 139}
	if got := ParseGateRunLog(log); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseGateRunLog() = %+v, want %+v", got, want)
	}
}

// TestParseGateRunLogPartial: a log whose markers are missing, untimed or out of order parses to
// the record its remaining markers allow, with no error. Each row drives one refusal path.
func TestParseGateRunLogPartial(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		log  string
		want GateRun
	}{
		{name: "empty log", log: "", want: GateRun{}},
		{name: "no gate markers", log: "2026-11-19T10:00:00Z go test ./internal/land\r\n", want: GateRun{}},
		{name: "markers without a timestamp", log: runnerTaskLine + "\nlint: x\ngate: build\ngate: unit tier\ngate: PASSED in 139s wall\n", want: GateRun{}},
		{name: "marker after an untimed first word", log: "run gate: PASSED in 139s wall\n", want: GateRun{}},
		{name: "lint and build with no task line", log: "2026-11-19T10:00:03Z lint: x\n2026-11-19T10:00:05Z gate: build\n", want: GateRun{LintS: 2}},
		{name: "task with no lint line", log: "2026-11-19T10:00:00Z " + runnerTaskLine + "\n", want: GateRun{}},
		{
			name: "received task without the Runner prefix",
			log: "2026-11-19T10:00:00Z the runner received task 20031 of job gate\n" +
				"2026-11-19T10:00:16Z lint: x\n",
			want: GateRun{},
		},
		{
			name: "Runner line that received no task",
			log: "2026-11-19T10:00:00Z Runner 14b9572a-0fbc-4368-ac6b-63af30a5f49c (version:v13.2.0) starting job gate\n" +
				"2026-11-19T10:00:16Z lint: x\n",
			want: GateRun{},
		},
		{
			name: "lint before the task line",
			log: "2026-11-19T10:00:05Z " + runnerTaskLine + "\n" +
				"2026-11-19T10:00:00Z lint: x\n" +
				"2026-11-19T10:00:05Z gate: build\n" +
				"2026-11-19T10:00:09Z gate: unit tier\n",
			want: GateRun{LintS: 5, BuildS: 4},
		},
		{name: "failed verdict with no wall", log: "2026-11-19T10:00:00Z gate: FAILED at build\n", want: GateRun{}},
		{name: "wall too large for a float", log: "2026-11-19T10:00:00Z gate: PASSED in " + strings.Repeat("9", 400) + "s wall\n", want: GateRun{Passed: true}},
		{name: "tier line without a verdict", log: "2026-11-19T10:02:00Z tierwall: tier 12s bar 90s\n", want: GateRun{}},
		{name: "tier line with a non-numeric wall", log: "2026-11-19T10:02:00Z tierwall: tier xs bar 90s OK\n", want: GateRun{}},
		{name: "tier line with a non-numeric bar", log: "2026-11-19T10:02:00Z tierwall: tier 12s bar ys OK\n", want: GateRun{}},
		{name: "tier wall too large for a float", log: "2026-11-19T10:02:00Z tierwall: tier " + strings.Repeat("9", 400) + "s bar 90s OK\n", want: GateRun{}},
		{name: "cpus line missing its cgroup", log: "2026-11-19T10:02:00Z tierwall: cpus: NumCPU=16 GOMAXPROCS=12\n", want: GateRun{}},
		{name: "cpus count too large for an int", log: "2026-11-19T10:02:00Z tierwall: cpus: NumCPU=" + strings.Repeat("9", 400) + " GOMAXPROCS=12 cgroup=max\n", want: GateRun{}},
		{name: "procs count too large for an int", log: "2026-11-19T10:02:00Z tierwall: cpus: NumCPU=16 GOMAXPROCS=" + strings.Repeat("9", 400) + " cgroup=max\n", want: GateRun{}},
		{name: "two fields whose seconds do not parse", log: "2026-11-19T10:02:00Z tierwall: example.test/a slows\n", want: GateRun{}},
		{name: "slow test line outside the slow block", log: "2026-11-19T10:02:00Z tierwall: example.test/a TestEarly 6s\n", want: GateRun{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseGateRunLog(tc.log); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseGateRunLog() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParseGateRunLogTierwall reads the tierwall report a gate job log carries: one line per
// package slowest first, the slow tests under their package, the tier line against its bar and
// the cpus line. The phases and WARNING lines between them are neither packages nor slow tests,
// and a slow-test-shaped line after the block closed is not one either.
func TestParseGateRunLogTierwall(t *testing.T) {
	t.Parallel()
	log := "2026-11-19T10:02:00Z tierwall: example.test/a 12.5s\r\n" +
		"2026-11-19T10:02:00Z tierwall: example.test/b 9.5s\r\n" +
		"2026-11-19T10:02:00Z tierwall: example.test/c 0.1s\r\n" +
		"2026-11-19T10:02:00Z tierwall: slow tests\r\n" +
		"2026-11-19T10:02:00Z tierwall:   example.test/a TestAlso 6s\r\n" +
		"2026-11-19T10:02:00Z tierwall:   example.test/b TestSlow 9.5s\r\n" +
		"2026-11-19T10:02:00Z tierwall: phases example.test/a serial 6s parallel 6.5s\r\n" +
		"2026-11-19T10:02:00Z tierwall:   example.test/c TestLate 1s\r\n" +
		"2026-11-19T10:02:00Z tierwall: WARNING package example.test/a is 42 percent of the tier wall (12.5s of 29.3s)\r\n" +
		"2026-11-19T10:02:00Z tierwall: tier 29.3s bar 90s OK\r\n" +
		"2026-11-19T10:02:00Z tierwall: cpus: NumCPU=16 GOMAXPROCS=12 cgroup=max 100000\r\n"

	want := GateRun{
		TierS: 29.3, TierBarS: 90,
		PolePkg: "example.test/a", PoleS: 12.5,
		Packages: []GateRunPackage{
			{Pkg: "example.test/a", S: 12.5},
			{Pkg: "example.test/b", S: 9.5},
			{Pkg: "example.test/c", S: 0.1},
		},
		SlowTests: []GateRunTest{
			{Pkg: "example.test/a", Test: "TestAlso", S: 6},
			{Pkg: "example.test/b", Test: "TestSlow", S: 9.5},
		},
		NumCPU: 16, GOMAXPROCS: 12, Cgroup: "max 100000",
	}
	if got := ParseGateRunLog(log); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseGateRunLog() = %+v, want %+v", got, want)
	}
}

// TestParseGateRunLogVet reads the integration vet's report line: its wall is a Go duration, so
// "1m5.8s" is 65.8 seconds. The vet's other lines, a duration that does not parse and a negative
// wall leave VetS zero, and so does a log without the line.
func TestParseGateRunLogVet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, log string
		want      float64
	}{
		{
			name: "sub-minute wall",
			log:  "2026-11-19T10:02:40Z integration vet: 79 packages in 2 modules type-check with -tags integration in 12.3s\r\n",
			want: 12.3,
		},
		{
			name: "minute and seconds",
			log:  "2026-11-19T10:02:40Z integration vet: 79 packages in 2 modules type-check with -tags integration in 1m5.8s\n",
			want: 65.8,
		},
		{
			name: "no vet line",
			log:  "2026-11-19T10:02:40Z gate: PASSED in 139s wall\n",
			want: 0,
		},
		{
			name: "failed vet",
			log:  "2026-11-19T10:02:40Z integration vet: FAILED in 1 of 2 modules after 12.3s\n",
			want: 0,
		},
		{
			name: "vet selection error",
			log:  "2026-11-19T10:02:40Z integration vet: no package under . adds files with the integration tag; the selection or -root is wrong\n",
			want: 0,
		},
		{
			name: "line missing the module count",
			log:  "2026-11-19T10:02:40Z integration vet: 79 packages type-check with -tags integration in 12.3s\n",
			want: 0,
		},
		{
			name: "duration that is not a duration",
			log:  "2026-11-19T10:02:40Z integration vet: 79 packages in 2 modules type-check with -tags integration in soon\n",
			want: 0,
		},
		{
			name: "duration with no unit",
			log:  "2026-11-19T10:02:40Z integration vet: 79 packages in 2 modules type-check with -tags integration in 12\n",
			want: 0,
		},
		{
			name: "negative duration",
			log:  "2026-11-19T10:02:40Z integration vet: 79 packages in 2 modules type-check with -tags integration in -5s\n",
			want: 0,
		},
		{
			name: "vet line without a timestamp",
			log:  "integration vet: 79 packages in 2 modules type-check with -tags integration in 12.3s\n",
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseGateRunLog(tc.log).VetS; got != tc.want {
				t.Errorf("VetS = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseGateRunLogVetJSON: the vet wall a parsed log carries reaches the record's JSON as
// vet_s, the key gate-runs.jsonl holds.
func TestParseGateRunLogVetJSON(t *testing.T) {
	t.Parallel()
	log := "2026-11-19T10:02:40Z integration vet: 79 packages in 2 modules type-check with -tags integration in 1m5.8s\n"
	b, err := json.Marshal(ParseGateRunLog(log))
	if err != nil {
		t.Fatalf("marshal gate run: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("unmarshal gate run: %v", err)
	}
	if got, ok := rec["vet_s"]; !ok || got != 65.8 {
		t.Errorf("record vet_s = %v (present %v), want 65.8", got, ok)
	}
}

// TestParseGateRunLogOverAndCached reads an OVER tier line and counts the packages go test served
// from its result cache: a timed ok line is not one.
func TestParseGateRunLogOverAndCached(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, log string
		want      GateRun
	}{
		{"tier over its bar", "2026-11-19T10:02:00Z tierwall: tier 115.1s bar 90s OVER\r\n", GateRun{TierS: 115.1, TierBarS: 90, Over: true}},
		{"two cached packages", "2026-11-19T10:02:00Z ok  \texample.test/a\t(cached)\r\n" +
			"2026-11-19T10:02:00Z ok  \texample.test/b\t(cached)\r\n" +
			"2026-11-19T10:02:00Z ok  \texample.test/c\t0.412s\r\n", GateRun{CachedPkgs: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseGateRunLog(tc.log); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseGateRunLog() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
