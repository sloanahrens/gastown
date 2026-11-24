package cmd

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
)

func TestPrintRepackPlan(t *testing.T) {
	t.Parallel()
	const repo, reason = "/town/.dolt-archive/git", "8103 loose objects (threshold 2000)"
	due := daemon.RepackPlan{Due: true, Reason: reason, Args: []string{
		"-c", "pack.threads=1", "-c", "gc.auto=0", "-c",
		"pack.windowMemory=256m", "gc", "--prune=2.weeks.ago"}}
	var out bytes.Buffer
	if err := printRepackPlan(&out, repo, due); err != nil {
		t.Fatal(err)
	}
	want := "due: yes\nreason: " + reason + "\ngit -C " + repo + " " +
		"-c pack.threads=1 -c gc.auto=0 -c pack.windowMemory=256m gc --prune=2.weeks.ago\n"
	if got := out.String(); got != want {
		t.Errorf("due output = %q, want %q", got, want)
	}
	out.Reset()
	if err := printRepackPlan(&out, repo, daemon.RepackPlan{Reason: "10 loose objects"}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "due: no\nreason: 10 loose objects\n"; got != want {
		t.Errorf("not-due output = %q, want %q", got, want)
	}
	if err := printRepackPlan(failingWriter{}, repo, due); err == nil {
		t.Error("write error = nil, want one")
	}
}

func TestRunDoltRepackPlanPrintsTheCommand(t *testing.T) {
	t.Parallel()
	const repo, countObjects = "/town/.dolt-archive/git", "count: 8103\nsize: 512\n"
	var calls [][]string
	run := func(argv ...string) ([]byte, error) {
		calls = append(calls, argv)
		return []byte(countObjects), nil
	}
	var out bytes.Buffer
	if err := runDoltRepackPlan(&out, repo, run); err != nil {
		t.Fatal(err)
	}
	want := "due: yes\n" +
		"reason: 8103 loose objects (threshold 2000), 512 KiB loose (threshold 1048576 KiB)\n" +
		"git -C " + repo + " -c pack.threads=1 -c gc.auto=0 -c pack.windowMemory=256m gc --prune=2.weeks.ago\n"
	if got := out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if wantCalls := [][]string{{"git", "-C", repo, "count-objects", "-v"}}; !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("runner calls = %q, want count-objects only %q", calls, wantCalls)
	}
}

func TestRunDoltRepackPlanRefusesAnUnreadableOutput(t *testing.T) {
	t.Parallel()
	const repo = "/town/.dolt-archive/git"
	run := func(...string) ([]byte, error) {
		return []byte("count: many\nsize: 512\n"), nil
	}
	var out bytes.Buffer
	err := runDoltRepackPlan(&out, repo, run)
	if err == nil {
		t.Fatal("error = nil, want one naming the unreadable line")
	}
	if got := out.String(); got != "" {
		t.Errorf("output = %q, want nothing printed on a reader error", got)
	}
}
