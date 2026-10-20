package cmd

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// The flag's default is the documented command, word for word.
func TestDashboardJevSpendCmdFlagDefault(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("jev-spend-cmd")
	if f == nil {
		t.Fatal("gt dashboard has no --jev-spend-cmd flag")
	}
	const want = "gt jev spend --json --hours 24"
	if f.DefValue != want {
		t.Errorf("--jev-spend-cmd defaults to %q, want %q", f.DefValue, want)
	}
	if !strings.Contains(dashboardCmd.Long, want) {
		t.Errorf("the help does not document the default command %q", want)
	}
}

// The price is its own flag, not a number inside the command string.
func TestDashboardJevPriceFlagDefault(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("jev-usd-per-billion-tokens")
	if f == nil {
		t.Fatal("gt dashboard has no --jev-usd-per-billion-tokens flag")
	}
	if f.DefValue != "42" {
		t.Errorf("--jev-usd-per-billion-tokens defaults to %q, want 42", f.DefValue)
	}
	if strings.Contains(defaultJevSpendCmd, "usd-per-billion-tokens") {
		t.Errorf("the default command %q bakes in a price", defaultJevSpendCmd)
	}
}

func TestJevSpendArgv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		cmd   string
		price float64
		want  []string
	}{
		{"default command takes the flag's price", defaultJevSpendCmd, 42,
			[]string{"gt", "jev", "spend", "--json", "--hours", "24", "--usd-per-billion-tokens", "42"}},
		{"a fractional price keeps its digits", "gt jev spend --json", 0.5,
			[]string{"gt", "jev", "spend", "--json", "--usd-per-billion-tokens", "0.5"}},
		{"a quoted path with a space is one word", `"/opt/my tools/jev spend" --json`, 3,
			[]string{"/opt/my tools/jev spend", "--json", "--usd-per-billion-tokens", "3"}},
		{"single quotes group too", `'/opt/my tools/jev' --json`, 3,
			[]string{"/opt/my tools/jev", "--json", "--usd-per-billion-tokens", "3"}},
		{"a price in the command is kept as written", "gt jev spend --usd-per-billion-tokens 7", 42,
			[]string{"gt", "jev", "spend", "--usd-per-billion-tokens", "7"}},
		{"the = spelling counts as a price", "gt jev spend --usd-per-billion-tokens=7", 42,
			[]string{"gt", "jev", "spend", "--usd-per-billion-tokens=7"}},
		{"an empty command is no argv", "", 42, nil},
		{"a blank command is no argv", "   ", 42, nil},
	}
	for _, tc := range cases {
		got, err := jevSpendArgv(tc.cmd, tc.price)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") || (got == nil) != (tc.want == nil) {
			t.Errorf("%s: argv = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A command or price that could only read as an unavailable row is refused when
// the dashboard starts.
func TestJevSpendArgvRefusesWhatCannotRun(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		cmd   string
		price float64
	}{
		"unbalanced quote": {`"gt jev spend`, 42},
		"zero price":       {defaultJevSpendCmd, 0},
		"negative price":   {defaultJevSpendCmd, -1},
		"infinite price":   {defaultJevSpendCmd, math.Inf(1)},
		"NaN price":        {defaultJevSpendCmd, math.NaN()},
	}
	for name, tc := range cases {
		if got, err := jevSpendArgv(tc.cmd, tc.price); err == nil {
			t.Errorf("%s: argv = %q, want an error", name, got)
		}
	}
}

// An empty command needs no price, so a zero price beside it is not an error.
func TestJevSpendArgvEmptyCommandIgnoresPrice(t *testing.T) {
	t.Parallel()

	if got, err := jevSpendArgv("", 0); err != nil || got != nil {
		t.Errorf("argv = %q, err = %v, want neither", got, err)
	}
}

// fakeJevRun stands in for the process: it records the argv it was asked to
// run and answers with out and err.
func fakeJevRun(out string, err error, got *[]string) func(context.Context, []string) ([]byte, error) {
	return func(_ context.Context, argv []string) ([]byte, error) {
		if got != nil {
			*got = argv
		}
		return []byte(out), err
	}
}

func TestDashboardJevSpendFoldsTheCommandsJSON(t *testing.T) {
	t.Parallel()

	const report = `{"calls":14,"input_tokens":900,"output_tokens":100,"usd":1.75,"lines_skipped":0,
 "by_caller":{"deacon":{"calls":4,"usd":0.25},"overseer":{"calls":10,"usd":1.5}}}`
	argv, err := jevSpendArgv(defaultJevSpendCmd, 42)
	if err != nil {
		t.Fatal(err)
	}
	const wantRan = "gt jev spend --json --hours 24 --usd-per-billion-tokens 42"
	var ran []string
	read := dashboardJevSpendWith(argv, fakeJevRun(report, nil, &ran))
	if read == nil {
		t.Fatal("a configured command has no reader")
	}
	j := read()
	if j == nil {
		t.Fatal("the command's report read as a failure")
	}
	if j.Calls != 14 || j.USD != 1.75 || j.TopCaller != "overseer" || j.TopCallerCalls != 10 || j.TopCallerUSD != 1.5 || j.Window != "24h" {
		t.Errorf("row = %+v", *j)
	}
	if strings.Join(ran, " ") != wantRan {
		t.Errorf("ran %q, want the default command with its price, %q", ran, wantRan)
	}
}

// A command that fails or prints something else reads as nil, the failed read
// the hub shows as unavailable.
func TestDashboardJevSpendFailureReadsAsNil(t *testing.T) {
	t.Parallel()

	cases := map[string]func(context.Context, []string) ([]byte, error){
		"exits non-zero": fakeJevRun(`{"calls":1,"usd":1}`, errors.New("exit status 3"), nil),
		"prints nothing": fakeJevRun("", nil, nil),
		"prints text":    fakeJevRun("gt: unknown command", nil, nil),
		"prints no usd":  fakeJevRun(`{"calls":1}`, nil, nil),
		"times out":      fakeJevRun("", context.DeadlineExceeded, nil),
	}
	for name, run := range cases {
		if got := dashboardJevSpendWith([]string{"gt", "jev", "spend"}, run)(); got != nil {
			t.Errorf("%s: read %+v, want a failed read", name, *got)
		}
	}
}

// A command that is not on the machine is a failed read, not a panic: the real
// runner returns the start error.
func TestDashboardJevSpendMissingCommandReadsAsNil(t *testing.T) {
	t.Parallel()

	if got := dashboardJevSpend([]string{"gt-no-such-jev-command-xyz", "jev", "spend"})(); got != nil {
		t.Errorf("a missing command read %+v, want a failed read", *got)
	}
}

// An empty command leaves the Jev row out, like an unset DeepSeek command.
func TestDashboardJevSpendEmptyCommandIsNoReader(t *testing.T) {
	t.Parallel()

	if dashboardJevSpend(strings.Fields("  ")) != nil {
		t.Error("an empty --jev-spend-cmd still has a reader")
	}
}

func TestDashboardJevScorecardCmdFlagDefault(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("jev-scorecard-cmd")
	if f == nil {
		t.Fatal("gt dashboard has no --jev-scorecard-cmd flag")
	}
	const want = "gt jev scorecard --json --since 7d"
	if f.DefValue != want {
		t.Errorf("--jev-scorecard-cmd defaults to %q, want %q", f.DefValue, want)
	}
	if !strings.Contains(dashboardCmd.Long, want) {
		t.Errorf("the help does not document the default command %q", want)
	}
}

func TestJevScorecardArgv(t *testing.T) {
	t.Parallel()

	got, err := jevScorecardArgv(`"/opt/my tools/gt" jev scorecard --json`)
	if err != nil || strings.Join(got, "|") != "/opt/my tools/gt|jev|scorecard|--json" {
		t.Errorf("argv = %q, err = %v", got, err)
	}
	if got, err := jevScorecardArgv("  "); err != nil || got != nil {
		t.Errorf("a blank command: argv = %q, err = %v, want neither", got, err)
	}
	if got, err := jevScorecardArgv(`"gt jev`); err == nil {
		t.Errorf("an unbalanced quote gave argv %q, want an error", got)
	}
}

// fakeJevScorecardRun answers each battery's run with its own report, keyed by
// the --battery value, and records the argv it saw.
func fakeJevScorecardRun(reports map[string]string, failOn string, ran *[][]string) func(context.Context, []string) ([]byte, error) {
	return func(_ context.Context, argv []string) ([]byte, error) {
		if ran != nil {
			*ran = append(*ran, argv)
		}
		name := argv[len(argv)-1]
		if name == failOn {
			return nil, errors.New("exit status 1")
		}
		return []byte(reports[name]), nil
	}
}

func TestDashboardJevScorecardRunsOncePerBattery(t *testing.T) {
	t.Parallel()

	reports := map[string]string{
		"finding": `{"battery":"finding","items":2,"questions":[{"id":"fix_worthy","verdict":"earning"}],"lines":[{"id":"a","labeled":true},{"id":"b","labeled":false}]}`,
		"landing": `{"battery":"landing","items":0,"questions":[]}`,
	}
	var ran [][]string
	read := dashboardJevScorecardWith(strings.Fields("gt jev scorecard --json --since 7d"), []string{"finding", "landing"}, fakeJevScorecardRun(reports, "", &ran))
	if read == nil {
		t.Fatal("a configured command has no reader")
	}
	sc := read()
	if sc == nil {
		t.Fatal("the reports read as a failure")
	}
	if sc.Window != "7d" || len(sc.Batteries) != 2 {
		t.Fatalf("scorecard = %+v", *sc)
	}
	if b := sc.Batteries[0]; b.Name != "finding" || b.Lines != 2 || b.Labeled != 1 || len(b.Questions) != 1 {
		t.Errorf("finding line = %+v", b)
	}
	if b := sc.Batteries[1]; b.Name != "landing" || b.Lines != 0 {
		t.Errorf("landing line = %+v", b)
	}
	if len(ran) != 2 || strings.Join(ran[0], " ") != "gt jev scorecard --json --since 7d --battery finding" ||
		strings.Join(ran[1], " ") != "gt jev scorecard --json --since 7d --battery landing" {
		t.Errorf("ran %q", ran)
	}
}

// One run that fails, prints text, or reports another battery fails the read,
// the scorecard unavailable the hub shows; the spend reader is not involved.
func TestDashboardJevScorecardFailureReadsAsNil(t *testing.T) {
	t.Parallel()

	good := `{"battery":"finding","items":1,"questions":[]}`
	cases := map[string]func(context.Context, []string) ([]byte, error){
		"exits non-zero": fakeJevScorecardRun(map[string]string{"finding": good}, "landing", nil),
		"prints text":    fakeJevScorecardRun(map[string]string{"finding": good, "landing": "gt: unknown battery"}, "", nil),
		"prints another": fakeJevScorecardRun(map[string]string{"finding": good, "landing": good}, "", nil),
		"prints nothing": fakeJevScorecardRun(map[string]string{"finding": good}, "", nil),
		"times out":      func(context.Context, []string) ([]byte, error) { return nil, context.DeadlineExceeded },
	}
	for name, run := range cases {
		if got := dashboardJevScorecardWith([]string{"gt", "jev", "scorecard"}, []string{"finding", "landing"}, run)(); got != nil {
			t.Errorf("%s: read %+v, want a failed read", name, *got)
		}
	}
}

func TestDashboardJevScorecardEmptyCommandIsNoReader(t *testing.T) {
	t.Parallel()

	if dashboardJevScorecard(nil) != nil {
		t.Error("an empty --jev-scorecard-cmd still has a reader")
	}
}

// The reader asks for every registered battery, so a new adapter appears on the
// dashboard without a change here.
func TestDashboardJevScorecardAsksForEveryRegisteredBattery(t *testing.T) {
	t.Parallel()

	var asked []string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		asked = append(asked, argv[len(argv)-1])
		return nil, errors.New("stop")
	}
	dashboardJevScorecardWith([]string{"gt"}, scorecard.Registered(), run)()
	if len(asked) == 0 || asked[0] != scorecard.Registered()[0] {
		t.Errorf("asked %q, want the registered batteries %q", asked, scorecard.Registered())
	}
}
