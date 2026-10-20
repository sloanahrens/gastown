package cmd

import (
	"os"
	"path/filepath"
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

// writeJevScript writes an executable shell script into a temp dir and returns
// its path, standing in for the spend command.
func writeJevScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jev-spend.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDashboardJevSpendFoldsTheCommandsJSON(t *testing.T) {
	t.Parallel()

	script := writeJevScript(t, `cat <<'EOF'
{"calls":14,"input_tokens":900,"output_tokens":100,"usd":1.75,"lines_skipped":0,
 "by_caller":{"deacon":{"calls":4,"usd":0.25},"overseer":{"calls":10,"usd":1.5}}}
EOF`)
	read := dashboardJevSpend([]string{script, "--json", "--hours", "6"})
	if read == nil {
		t.Fatal("a configured command has no reader")
	}
	j := read()
	if j == nil {
		t.Fatal("the command's report read as a failure")
	}
	if j.Calls != 14 || j.USD != 1.75 || j.TopCaller != "overseer" || j.TopCallerCalls != 10 || j.TopCallerUSD != 1.5 || j.Window != "6h" {
		t.Errorf("row = %+v", *j)
	}
}

// A command that fails, is missing, or prints something else reads as nil, the
// failed read the hub shows as unavailable.
func TestDashboardJevSpendFailureReadsAsNil(t *testing.T) {
	t.Parallel()

	cases := map[string][]string{
		"exits non-zero":     {writeJevScript(t, `echo '{"calls":1,"usd":1}'; exit 3`)},
		"prints nothing":     {writeJevScript(t, `true`)},
		"prints text":        {writeJevScript(t, `echo 'gt: unknown command'`)},
		"prints no usd":      {writeJevScript(t, `echo '{"calls":1}'`)},
		"is not a file":      {filepath.Join(t.TempDir(), "no-such-command")},
		"is not on the PATH": {"gt-no-such-jev-command-xyz", "jev", "spend"},
	}
	for name, argv := range cases {
		if got := dashboardJevSpend(argv)(); got != nil {
			t.Errorf("%s: read %+v, want a failed read", name, *got)
		}
	}
}

// An empty command leaves the Jev row out, like an unset DeepSeek command.
func TestDashboardJevSpendEmptyCommandIsNoReader(t *testing.T) {
	t.Parallel()

	if dashboardJevSpend(strings.Fields("  ")) != nil {
		t.Error("an empty --jev-spend-cmd still has a reader")
	}
}

// The Jev command and the DeepSeek command are separate readers: a Jev command
// that fails leaves the DeepSeek one reading.
func TestDashboardJevSpendFailureLeavesTheDeepSeekReader(t *testing.T) {
	t.Parallel()

	deepseek := dashboardSpend([]string{writeJevScript(t, `echo '{"balance":12}'`)})
	jev := dashboardJevSpend([]string{writeJevScript(t, `exit 1`)})
	if jev() != nil {
		t.Error("the failing Jev command read")
	}
	if got := deepseek(); string(got) == "" {
		t.Error("the DeepSeek reader went blank beside a failing Jev command")
	}
}
