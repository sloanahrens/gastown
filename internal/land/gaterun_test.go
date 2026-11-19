package land

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseGateRunLog reads a passing gate's log. The repeated lint line at 10:00:04 leaves lint_s
// at 1.5: the stage starts at the first lint line.
func TestParseGateRunLog(t *testing.T) {
	t.Parallel()
	log := "2026-11-19T10:00:00Z task received\r\n" +
		"2026-11-19T10:00:03.5Z lint: golangci-lint run --timeout=5m\r\n" +
		"2026-11-19T10:00:04Z lint: hygiene scan\r\n" +
		"2026-11-19T10:00:05Z gate: build (go build ./...)\r\n" +
		"2026-11-19T10:00:09Z gate: unit tier (every package)\r\n" +
		"2026-11-19T10:02:28Z gate: PASSED in 139s wall\r\n"

	want := GateRun{SetupS: 3.5, LintS: 1.5, BuildS: 4, UnitWallS: 139, Passed: true}
	if got := ParseGateRunLog(log); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseGateRunLog() = %+v, want %+v", got, want)
	}
}

// TestParseGateRunLogFailed reads a failed gate's verdict: the wall comes from the line and passed
// is false.
func TestParseGateRunLogFailed(t *testing.T) {
	t.Parallel()
	log := "2026-11-19T10:00:00Z task received\r\n" +
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
		{name: "markers without a timestamp", log: "task received\nlint: x\ngate: build\ngate: unit tier\ngate: PASSED in 139s wall\n", want: GateRun{}},
		{name: "marker after an untimed first word", log: "run gate: PASSED in 139s wall\n", want: GateRun{}},
		{name: "lint and build with no task line", log: "2026-11-19T10:00:03Z lint: x\n2026-11-19T10:00:05Z gate: build\n", want: GateRun{LintS: 2}},
		{name: "task with no lint line", log: "2026-11-19T10:00:00Z task received\n", want: GateRun{}},
		{
			name: "lint before the task line",
			log: "2026-11-19T10:00:05Z task received\n" +
				"2026-11-19T10:00:00Z lint: x\n" +
				"2026-11-19T10:00:05Z gate: build\n" +
				"2026-11-19T10:00:09Z gate: unit tier\n",
			want: GateRun{LintS: 5, BuildS: 4},
		},
		{name: "failed verdict with no wall", log: "2026-11-19T10:00:00Z gate: FAILED at build\n", want: GateRun{}},
		{name: "wall too large for a float", log: "2026-11-19T10:00:00Z gate: PASSED in " + strings.Repeat("9", 400) + "s wall\n", want: GateRun{Passed: true}},
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
