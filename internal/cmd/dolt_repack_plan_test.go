package cmd

import (
	"bytes"
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
