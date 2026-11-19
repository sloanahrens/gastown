package vmstart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// startTries is how many times Start tries one VM before it records the step as
// failed, and startPause the wait between failed tries: the launchd job's
// inline loop tried three times, 20 seconds apart (gt-igzpx9).
const (
	startTries = 3
	startPause = 20 * time.Second
)

// startProfiles is the order Start brings the VMs up in. The sandbox VM goes
// first because the launchd job started gt-control first, which let its 5001
// forward loop while the sandbox registry had not yet bound the port
// (gt-igzpx9).
var startProfiles = []string{profileSandbox, profileControl}

// containerStartArgv starts the sandbox VM's kind nodes and registry. The
// docker context is named here because colima start switches Docker's global
// context to the VM it started, so the global one cannot be trusted to reach
// the sandbox VM (gt-igzpx9).
var containerStartArgv = []string{"docker", "--context", "colima-" + profileSandbox,
	"start", "gt-sbx-control-plane", "gt-sbx-worker", "gt-sbx-worker2", "gt-registry"}

// Row is one step run: the VM profile it was run for, the command line, and the
// error the command returned, if any.
type Row struct {
	Profile string
	Command string
	Err     error
}

// Report is what Start ran, in order: one row per step run, so a retried start
// shows each attempt (gt-igzpx9).
type Report []Row

func (r *Report) add(profile, command string, err error) {
	*r = append(*r, Row{Profile: profile, Command: command, Err: err})
}

// String prints one line per row: the profile, the command and "ok" or the
// error.
func (r Report) String() string {
	var b strings.Builder
	for _, row := range r {
		outcome := "ok"
		if row.Err != nil {
			outcome = "error: " + row.Err.Error()
		}
		fmt.Fprintf(&b, "%s  %s  %s\n", row.Profile, row.Command, outcome)
	}
	return b.String()
}

// Start brings both VMs up sandbox first, starts the sandbox VM's containers,
// and returns the report of every step run; a failed step is recorded and the
// later steps that can still run are still tried, and Start returns one error
// naming each failed step at the end, never a success (gt-igzpx9).
func Start(ctx context.Context, run Runner, sleep func(time.Duration), now func() time.Time, home string) (Report, error) { //nolint:unparam // now and home are the repairs' inputs in bead gt-4eig1r
	if run == nil {
		return nil, errors.New("Start: nil runner")
	}
	if sleep == nil {
		return nil, errors.New("Start: nil sleep")
	}
	var report Report
	var failed []error
	for _, profile := range startProfiles {
		argv := []string{"colima", "start", "-p", profile, "--activate=false"}
		var lastErr error
		for try := 1; try <= startTries; try++ {
			_, err := run(ctx, argv[0], argv[1:]...)
			report.add(profile, strings.Join(argv, " "), err)
			lastErr = err
			if err == nil {
				break
			}
			if try < startTries {
				sleep(startPause)
			}
		}
		if lastErr != nil {
			failed = append(failed, fmt.Errorf("%s: %w", strings.Join(argv, " "), lastErr))
		}
	}
	_, err := run(ctx, containerStartArgv[0], containerStartArgv[1:]...)
	report.add(profileSandbox, strings.Join(containerStartArgv, " "), err)
	if err != nil {
		failed = append(failed, fmt.Errorf("%s: %w", strings.Join(containerStartArgv, " "), err))
	}
	if len(failed) > 0 {
		return report, fmt.Errorf("start: %w", errors.Join(failed...))
	}
	return report, nil
}
