package cmd

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
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
	var mu sync.Mutex
	return func(_ context.Context, argv []string) ([]byte, error) {
		if ran != nil {
			mu.Lock()
			*ran = append(*ran, argv)
			mu.Unlock()
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
	read := dashboardJevScorecardWith(strings.Fields("gt jev scorecard --json --since 7d"), []string{"finding", "landing"}, time.Minute, fakeJevScorecardRun(reports, "", &ran))
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
	var lines []string
	for _, argv := range ran {
		lines = append(lines, strings.Join(argv, " "))
	}
	sort.Strings(lines)
	if want := []string{"gt jev scorecard --json --since 7d --battery finding", "gt jev scorecard --json --since 7d --battery landing"}; !reflect.DeepEqual(lines, want) {
		t.Errorf("ran %q, want %q", lines, want)
	}
	if want := (dashboard.JevTotals{Batteries: 2, Lines: 2, Labeled: 1, Earning: 1}); sc.Totals != want {
		t.Errorf("Totals = %+v, want %+v", sc.Totals, want)
	}
}

// The reader keeps each question's figures so the panel's chip can say what the
// scorecard scored, and a figure the report leaves null stays absent.
func TestDashboardJevScorecardKeepsTheQuestionsFigures(t *testing.T) {
	t.Parallel()

	report := `{"battery":"finding","items":9,"labeled":7,"questions":[{"id":"fix_worthy","kind":"noul","logged":9,"labeled":7,"base_rate":0.25,"precision":0.625,"verdict":"earning"},{"id":"noise","kind":"noul","logged":9,"labeled":2,"base_rate":null,"precision":null,"verdict":"not-yet"}]}`
	read := dashboardJevScorecardWith(strings.Fields("gt jev scorecard --json"), []string{"finding"}, time.Minute, fakeJevScorecardRun(map[string]string{"finding": report}, "", nil))
	sc := read()
	if sc == nil {
		t.Fatal("the report read as a failure")
	}
	qs := sc.Batteries[0].Questions
	if len(qs) != 2 {
		t.Fatalf("questions = %+v, want two", qs)
	}
	if q := qs[0]; q.Labeled != 7 || q.BaseRate == nil || *q.BaseRate != 0.25 || q.Precision == nil || *q.Precision != 0.625 {
		t.Errorf("fix_worthy = %+v, want the report's figures", q)
	}
	if q := qs[1]; q.Labeled != 2 || q.BaseRate != nil || q.Precision != nil {
		t.Errorf("noise = %+v, want a null base rate and precision left absent", q)
	}
}

// A run that fails, prints text, or reports another battery drops only its own
// line: the other battery still draws and the lost one is named. The spend
// reader is not involved.
func TestDashboardJevScorecardFailureDropsOnlyThatBattery(t *testing.T) {
	t.Parallel()

	good := `{"battery":"finding","items":1,"questions":[]}`
	cases := map[string]func(context.Context, []string) ([]byte, error){
		"exits non-zero": fakeJevScorecardRun(map[string]string{"finding": good}, "landing", nil),
		"prints text":    fakeJevScorecardRun(map[string]string{"finding": good, "landing": "gt: unknown battery"}, "", nil),
		"prints another": fakeJevScorecardRun(map[string]string{"finding": good, "landing": good}, "", nil),
		"prints nothing": fakeJevScorecardRun(map[string]string{"finding": good}, "", nil),
		"times out": func(_ context.Context, argv []string) ([]byte, error) {
			if argv[len(argv)-1] == "landing" {
				return nil, context.DeadlineExceeded
			}
			return []byte(good), nil
		},
	}
	for name, run := range cases {
		got := dashboardJevScorecardWith([]string{"gt", "jev", "scorecard"}, []string{"finding", "landing"}, time.Minute, run)()
		if got == nil {
			t.Errorf("%s: one good battery read as a failed read", name)
			continue
		}
		if !reflect.DeepEqual(got.Unavailable, []string{"landing"}) || len(got.Batteries) != 2 ||
			got.Batteries[0].Failed || !got.Batteries[1].Failed || got.Batteries[1].Kept {
			t.Errorf("%s: scorecard = %+v, want finding drawn and landing a placeholder", name, *got)
		}
		if got.Totals.Batteries != 1 || got.Totals.Lines != 1 {
			t.Errorf("%s: Totals = %+v, want only finding counted", name, got.Totals)
		}
	}
}

// With five batteries, one that fails and one that times out leave the other
// three lines and name the two.
func TestDashboardJevScorecardOneFailingOneTimingOutAmongFive(t *testing.T) {
	t.Parallel()

	report := func(name string) string {
		return `{"battery":"` + name + `","items":3,"questions":[]}`
	}
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		switch name := argv[len(argv)-1]; name {
		case "dispatch":
			return nil, errors.New("exit status 1")
		case "drift":
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			return []byte(report(name)), nil
		}
	}
	batteries := []string{"finding", "dispatch", "submission", "drift", "testsel"}
	got := dashboardJevScorecardWith([]string{"gt", "jev", "scorecard"}, batteries, 20*time.Millisecond, run)()
	if got == nil {
		t.Fatal("three good batteries read as a failed read")
	}
	var drawn []string
	for _, b := range got.Batteries {
		if !b.Failed {
			drawn = append(drawn, b.Name)
		}
	}
	if want := []string{"finding", "submission", "testsel"}; !reflect.DeepEqual(drawn, want) {
		t.Errorf("drawn batteries = %q, want %q", drawn, want)
	}
	if want := []string{"dispatch", "drift"}; !reflect.DeepEqual(got.Unavailable, want) {
		t.Errorf("Unavailable = %q, want %q", got.Unavailable, want)
	}
	if got.Totals.Batteries != 3 || got.Totals.Lines != 9 {
		t.Errorf("Totals = %+v, want the three good batteries", got.Totals)
	}
}

// When every battery fails there is nothing to draw, so the read fails whole
// and the hub shows the scorecard unavailable.
func TestDashboardJevScorecardEveryBatteryFailingReadsAsNil(t *testing.T) {
	t.Parallel()

	run := func(context.Context, []string) ([]byte, error) { return nil, context.DeadlineExceeded }
	if got := dashboardJevScorecardWith([]string{"gt"}, []string{"finding", "landing"}, time.Minute, run)(); got != nil {
		t.Errorf("read %+v, want a failed read", *got)
	}
}

// The scorecard read has its own timeout: the slowest battery answers in about
// 32s, past the spend command's 25s, and each run is bounded by that timeout.
func TestDashboardJevScorecardTimeoutIsApartFromTheSpendTimeout(t *testing.T) {
	t.Parallel()

	if dashboardJevScorecardTimeout != 60*time.Second {
		t.Errorf("dashboardJevScorecardTimeout = %v, want 60s", dashboardJevScorecardTimeout)
	}
	if dashboardJevScorecardTimeout <= dashboardSpendTimeout {
		t.Errorf("the scorecard timeout %v is not above the spend timeout %v", dashboardJevScorecardTimeout, dashboardSpendTimeout)
	}
	var left time.Duration
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("a scorecard run has no deadline")
		}
		left = time.Until(d)
		return nil, errors.New("stop")
	}
	dashboardJevScorecardWith([]string{"gt"}, []string{"finding"}, dashboardJevScorecardTimeout, run)()
	if left <= dashboardSpendTimeout || left > dashboardJevScorecardTimeout {
		t.Errorf("a run had %v left, want between the spend timeout and %v", left, dashboardJevScorecardTimeout)
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

	var mu sync.Mutex
	var asked []string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		mu.Lock()
		asked = append(asked, argv[len(argv)-1])
		mu.Unlock()
		return nil, errors.New("stop")
	}
	dashboardJevScorecardWith([]string{"gt"}, scorecard.Registered(), time.Minute, run)()
	sort.Strings(asked)
	if want := scorecard.Registered(); !reflect.DeepEqual(asked, want) {
		t.Errorf("asked %q, want the registered batteries %q", asked, want)
	}
	for _, name := range asked {
		if name == scorecard.FixtureBattery {
			t.Errorf("the dashboard asked for the test-only %q battery: %q", name, asked)
		}
	}
}

// The runs go out together: each one waits until every battery has started, so
// a read that ran them one after another would stall and time the battery out.
func TestDashboardJevScorecardRunsBatteriesTogether(t *testing.T) {
	t.Parallel()

	batteries := []string{"dispatch", "drift", "finding", "landing"}
	var started sync.WaitGroup
	started.Add(len(batteries))
	allStarted := make(chan struct{})
	go func() {
		started.Wait()
		close(allStarted)
	}()
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		name := argv[len(argv)-1]
		started.Done()
		select {
		case <-allStarted:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []byte(`{"battery":"` + name + `","items":1,"questions":[]}`), nil
	}
	got := dashboardJevScorecardWith([]string{"gt", "jev", "scorecard"}, batteries, 5*time.Second, run)()
	if got == nil {
		t.Fatal("the batteries read as a failed read")
	}
	if len(got.Unavailable) != 0 {
		t.Errorf("Unavailable = %q, want none: the batteries did not run together", got.Unavailable)
	}
	var names []string
	for _, b := range got.Batteries {
		names = append(names, b.Name)
	}
	if !reflect.DeepEqual(names, batteries) {
		t.Errorf("battery order = %q, want %q", names, batteries)
	}
}
