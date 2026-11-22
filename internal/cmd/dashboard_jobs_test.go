package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The dashboard's jobs source is configured with the command gt jobs --json on
// the five-minute cadence the hub polls it on, as the Jev spend source beside
// it has its command (gt-qvi28g).
func TestDashboardJobsCommand(t *testing.T) {
	t.Parallel()

	if got, want := strings.Join(jobsArgv(), " "), "gt jobs --json"; got != want {
		t.Fatalf("the jobs command is %q, want %q", got, want)
	}
	if jobsEvery != 5*time.Minute {
		t.Errorf("the jobs cadence is %v, want five minutes", jobsEvery)
	}

	var ran []string
	read := dashboardJobs(jobsArgv(), fakeJevRun(`[{"name":"dolt_gc","state":"ok","outcome":"ok"}]`, nil, &ran))
	if j := read(); j == nil || len(j.Rows) != 1 || j.Rows[0].Name != "dolt_gc" || j.Worst != "ok" {
		t.Fatalf("read %+v, want the one ok dolt_gc row", j)
	}
	if got := strings.Join(ran, " "); got != "gt jobs --json" {
		t.Errorf("ran %q, want the jobs command", got)
	}

	// A command that fails or prints anything but the list reads as nil, the
	// failed read the hub keeps the last good list over (gt-16wpss).
	for name, run := range map[string]func(context.Context, []string) ([]byte, error){
		"exits non-zero": fakeJevRun("", errors.New("exit status 3"), nil),
		"prints text":    fakeJevRun("gt: unknown command", nil, nil),
	} {
		if got := dashboardJobs(jobsArgv(), run)(); got != nil {
			t.Errorf("%s: read %+v, want a failed read", name, got)
		}
	}

	// newDashboardHub is the only place the source is configured, and the hub
	// hides its config, so the assignments are read from its body, as
	// TestDashboardHubReadsTitlesThroughOpenDashboardBeads reads the reader.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "dashboard.go"))
	if err != nil {
		t.Fatalf("reading dashboard.go: %v", err)
	}
	hub := sourceFunc(t, string(src), "newDashboardHub")
	for _, want := range []string{"dashboardJobs(jobsArgv(), execSpend)", "JobsEvery:"} {
		if !strings.Contains(hub, want) {
			t.Errorf("newDashboardHub does not configure the jobs source: no %q", want)
		}
	}
}
