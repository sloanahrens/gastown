package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The flag's default is the documented command, word for word.
func TestDashboardJevSpendCmdFlagDefault(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("jev-spend-cmd")
	if f == nil {
		t.Fatal("gt dashboard has no --jev-spend-cmd flag")
	}
	const want = "gt jev spend --json --hours 24 --usd-per-billion-tokens 42"
	if f.DefValue != want {
		t.Errorf("--jev-spend-cmd defaults to %q, want %q", f.DefValue, want)
	}
	if !strings.Contains(dashboardCmd.Long, want) {
		t.Errorf("the help does not document the default command %q", want)
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
	argv := strings.Fields(defaultJevSpendCmd)
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
	if strings.Join(ran, " ") != defaultJevSpendCmd {
		t.Errorf("ran %q, want the default command %q", ran, defaultJevSpendCmd)
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
