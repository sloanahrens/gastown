package cmd

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// A crew session that fails to stop makes runDownWith return an error, so gt
// down exits non-zero instead of printing "All services stopped". The unit tier
// starts no tmux (docs/testing.md), so the session stops are the seam
// downServices.sessions supplies; the daemon and Dolt still stop.
func TestRunDownSessionStopErrorExitsNonZero(t *testing.T) {
	t.Parallel()
	town := downTestTown(t, `{"version":1,"rigs":{"gastown":{"git_url":"file:///tmp/gastown","added_at":"2026-01-01T00:00:00Z"}}}`, "gastown")
	rec := &downServicesRecorder{}
	svc := rec.services()
	svc.sessions = &downSessionStops{
		polecats: func(force, dryRun bool) (int, int) { return 0, 0 },
		crew:     func(dryRun bool) (int, int) { return 1, 1 },
	}

	err := runDownWith(town, svc)

	if err == nil {
		t.Fatal("runDownWith with a crew session that would not stop = nil, want a non-zero exit")
	}
	if !strings.Contains(err.Error(), "not all services stopped") {
		t.Errorf("error = %v, want it to say not all services stopped", err)
	}
	if got, want := strings.Join(rec.stops(), ","), "daemon,dolt"; got != want {
		t.Errorf("stopped %q, want %q: a failed session stop must not skip the town services", got, want)
	}
}

// The tally of a stop pass: one line per session, and the count of those that
// failed, which runDownWith clears allOK on.
func TestReportStopResultsCountsFailures(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	stopped, failed := reportStopResults(&out, []downStopResult{
		{rigName: "gastown", name: "crew/sloan", err: errors.New("kill-session: no server running")},
		{rigName: "gastown", name: "nux"},
	})

	if stopped != 1 || failed != 1 {
		t.Errorf("stopped, failed = %d, %d; want 1, 1", stopped, failed)
	}
	for _, want := range []string{"crew/sloan: kill-session: no server running", "nux stopped"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}
}

func TestIsProcessRunning_CurrentProcess(t *testing.T) {
	t.Parallel()
	if !isProcessRunning(os.Getpid()) {
		t.Error("current process should be detected as running")
	}
}

func TestIsProcessRunning_InvalidPID(t *testing.T) {
	t.Parallel()
	if isProcessRunning(99999999) {
		t.Error("invalid PID should not be detected as running")
	}
}

func TestIsProcessRunning_MaxPID(t *testing.T) {
	t.Parallel()
	if isProcessRunning(2147483647) {
		t.Error("max PID should not be running")
	}
}

// gt down SIGKILLs what the orphan scan returns, so the scan must take only
// real Dolt SQL servers from the process table: a process that merely mentions
// dolt-sql-server is left alone even when its cwd is a town orphan directory,
// and a dolt with a global flag before the subcommand is still found. Of the
// servers, only those rooted in the town outside .dolt-data are orphans.
func TestOrphanDoltServerPIDsUsesSharedMatcher(t *testing.T) {
	t.Parallel()
	ps := strings.Join([]string{
		"  PID ARGS",
		"  100 tail -f dolt-sql-server.log",
		"  101 vim dolt-server.go",
		"  102 /opt/homebrew/bin/dolt sql-server --port 3307",
		"  103 dolt --config x sql-server",
		"  104 dolt status",
		"  105 grep dolt sql-server",
		"  106 dolt --config x sql-server",
		"  107 dolt sql-server",
		"  108 dolt sql-server",
		"  109 dolt sql-server",
	}, "\n")
	cwds := map[int]string{
		100: "/town/gastown/.beads/dolt",
		102: "/town/gastown/.beads/dolt",
		103: "/town/.dolt-data/hq",
		104: "/town/gastown/.beads/dolt",
		105: "/town/gastown/.beads/dolt",
		106: "/town",
		107: "/town-other/.beads/dolt",
		108: "/elsewhere",
		// 109 has no cwd: lsof failed for it.
	}

	got := orphanDoltServerPIDs("/town", ps, func(pid int) string { return cwds[pid] })

	if want := []int{102, 106}; !slices.Equal(got, want) {
		t.Fatalf("orphanDoltServerPIDs = %v, want %v", got, want)
	}
}
