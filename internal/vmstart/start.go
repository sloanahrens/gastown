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

// containerStartArgv starts the sandbox VM's kind nodes and registry. The
// docker context is named here because colima start switches Docker's global
// context to the VM it started, so the global one cannot be trusted to reach
// the sandbox VM (gt-igzpx9).
var containerStartArgv = []string{"docker", "--context", "colima-" + profileSandbox,
	"start", "gt-sbx-control-plane", "gt-sbx-worker", "gt-sbx-worker2", "gt-registry"}

// A failed repair's report row names the repair rather than a command: the
// error it carries already names the command that failed, and a repair that
// failed in a read ran no repair command at all (gt-4eig1r).
const (
	dnsRepairRow      = "repair DNS"
	registryRepairRow = "repair the registry forward"
)

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

// StartStep is one step of a start and the work it runs.
type StartStep struct {
	// Name is the step's name, which `gt town vms start --dry-run` prints.
	Name string
	// profile is the VM a VM start brings up, empty for the other steps.
	profile string
	// run is the step's work, passed its own entry so one func serves both VM
	// steps.
	run func(*startRun, StartStep)
}

// StartPlan is every step a start runs, in the order Start runs them, so the
// order the dry run prints cannot disagree with the run (gt-dzduz6). The
// sandbox VM goes first because the launchd job started gt-control first, which
// let its 5001 forward loop while the sandbox registry had not yet bound the
// port (gt-igzpx9).
var StartPlan = []StartStep{
	{Name: profileSandbox, profile: profileSandbox, run: (*startRun).startVM},
	{Name: profileControl, profile: profileControl, run: (*startRun).startVM},
	{Name: "containers", run: (*startRun).startContainers},
	{Name: "DNS", run: (*startRun).repairDNS},
	{Name: "registry", run: (*startRun).repairRegistry},
}

// startRun is one run of Start: the plan it runs, what it runs commands
// through, and what it has recorded so far.
type startRun struct {
	// plan is the list Start runs. The DNS step repairs this list's VM steps;
	// reading StartPlan there would make its own initializer a cycle.
	plan   []StartStep
	ctx    context.Context
	run    Runner
	sleep  func(time.Duration)
	now    func() time.Time
	home   string
	report Report
	failed []error
}

// Start runs StartPlan's steps in order and returns the report of every step
// run; a failed step is recorded and the later steps that can still run are
// still tried, and Start returns one error naming each failed step at the end,
// never a success (gt-igzpx9, gt-4eig1r).
func Start(ctx context.Context, run Runner, sleep func(time.Duration), now func() time.Time, home string) (Report, error) {
	if run == nil {
		return nil, errors.New("Start: nil runner")
	}
	if sleep == nil {
		return nil, errors.New("Start: nil sleep")
	}
	s := &startRun{plan: StartPlan, ctx: ctx, run: run, sleep: sleep, now: now, home: home}
	for _, step := range s.plan {
		step.run(s, step)
	}
	if len(s.failed) > 0 {
		return s.report, fmt.Errorf("start: %w", errors.Join(s.failed...))
	}
	return s.report, nil
}

// startVM brings the step's VM up, trying startTries times with startPause
// between failed tries.
func (s *startRun) startVM(step StartStep) {
	argv := []string{"colima", "start", "-p", step.profile, "--activate=false"}
	var lastErr error
	for try := 1; try <= startTries; try++ {
		_, err := s.run(s.ctx, argv[0], argv[1:]...)
		s.report.add(step.profile, strings.Join(argv, " "), err)
		lastErr = err
		if err == nil {
			break
		}
		if try < startTries {
			s.sleep(startPause)
		}
	}
	if lastErr != nil {
		s.failed = append(s.failed, fmt.Errorf("%s: %w", strings.Join(argv, " "), lastErr))
	}
}

func (s *startRun) startContainers(StartStep) {
	_, err := s.run(s.ctx, containerStartArgv[0], containerStartArgv[1:]...)
	s.report.add(profileSandbox, strings.Join(containerStartArgv, " "), err)
	if err != nil {
		s.failed = append(s.failed, fmt.Errorf("%s: %w", strings.Join(containerStartArgv, " "), err))
	}
}

// repairDNS repairs each VM the plan starts, in plan order, so the repairs
// follow the starts (gt-4eig1r).
func (s *startRun) repairDNS(StartStep) {
	for _, step := range s.plan {
		if step.profile == "" {
			continue
		}
		steps, err := RepairDNS(s.ctx, s.run, step.profile, s.now)
		for _, repair := range steps {
			s.report.add(step.profile, repair.Command, nil)
		}
		if err != nil {
			s.report.add(step.profile, dnsRepairRow, err)
			s.failed = append(s.failed, err)
		}
	}
}

// repairRegistry repairs the registry forward once, for the sandbox VM whose
// registry the repair restarts; canceling gt-control's forward of the port is
// part of that same repair (gt-4eig1r).
func (s *startRun) repairRegistry(StartStep) {
	steps, err := RepairRegistry(s.ctx, s.run, s.home)
	for _, step := range steps {
		s.report.add(profileSandbox, step.Command, nil)
	}
	if err != nil {
		s.report.add(profileSandbox, registryRepairRow, err)
		s.failed = append(s.failed, err)
	}
}
