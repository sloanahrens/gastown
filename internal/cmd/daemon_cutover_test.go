package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"
)

// autoClock is a fake clock whose waits end at once by moving time forward, so
// a wait that runs to its budget does so on the fake clock with no goroutine
// and no real sleep. After is the only wait the cutover and WaitQuiet make.
type autoClock struct{ *clockwork.FakeClock }

func (a autoClock) After(d time.Duration) <-chan time.Time {
	a.FakeClock.Advance(d)
	ch := make(chan time.Time, 1)
	ch <- a.FakeClock.Now()
	return ch
}

var (
	cutoverLaunchd   = daemon.Owner{Kind: config.SupervisorLaunchd, ID: templates.LaunchdLabel}
	cutoverContainer = daemon.Owner{Kind: config.SupervisorContainer, ID: "scratch-daemon-1"}
)

// cutoverRig is the host a cutover runs on in a test: the clock, the commands
// run, the daemon's state.json, both jobs and what each command changes.
type cutoverRig struct {
	clk   autoClock
	start time.Time
	out   bytes.Buffer
	calls []string

	// state is what state.json holds now; nil is a missing file. beat, when
	// set, gives the heartbeat count at the time it is read.
	state *daemon.State
	beat  func() int64
	// fromJob and toJob are the live states of the two jobs.
	fromJob, toJob templates.SupervisorState
	// onStop and onStart change the host when the stop or a start command runs.
	onStop, onStart func(r *cutoverRig)
	// runErr fails the command whose first word matches the key.
	runErr map[string]error

	quietCalls  int
	quietAnswer func(call int) error
	guardErr    error
	guarded     []daemon.Owner
}

func newCutoverRig() *cutoverRig {
	clk := autoClock{clockwork.NewFakeClockAt(quietTestEpoch)}
	return &cutoverRig{
		clk:     clk,
		start:   clk.Now(),
		state:   &daemon.State{Running: true, Owner: &cutoverLaunchd, HeartbeatCount: 40},
		fromJob: templates.SupervisorState{Loaded: true, PID: 99, LastExit: -1},
		toJob:   templates.SupervisorState{LastExit: -1},
		runErr:  map[string]error{},
	}
}

// toContainer is a rig whose old daemon stops when it is booted out and whose
// container takes the town on its start and beats once a minute after it.
func toContainerRig() *cutoverRig {
	r := newCutoverRig()
	r.onStop = func(r *cutoverRig) {
		r.state = &daemon.State{Running: false, Owner: &cutoverLaunchd, HeartbeatCount: 40}
		r.fromJob = templates.SupervisorState{Loaded: false, LastExit: -1}
	}
	r.onStart = func(r *cutoverRig) { r.beatsFrom(r.clk.Now(), &cutoverContainer) }
	return r
}

// beatsFrom makes state.json read as owner running with one heartbeat for each
// minute that has passed on the clock since from.
func (r *cutoverRig) beatsFrom(from time.Time, owner *daemon.Owner) {
	r.state = &daemon.State{Running: true, Owner: owner}
	r.beat = func() int64 { return int64(r.clk.Since(from) / time.Minute) }
}

func (r *cutoverRig) plan(to string) *cutoverPlan {
	launchd := cutoverSide{
		name:      config.SupervisorLaunchd,
		owner:     cutoverLaunchd,
		stop:      fixedArgv("launchctl", "bootout", "gui/501/com.gastown.daemon"),
		start:     fixedArgv("launchctl", "kickstart", "-k", "gui/501/com.gastown.daemon"),
		bootstrap: fixedArgv("launchctl", "bootstrap", "gui/501", "/plist"),
		job:       func() templates.SupervisorState { return r.fromJob },
		stopped:   launchdStopped,
		restore:   "launchctl bootstrap gui/501 /plist",
		diagnose:  "launchctl print gui/501/com.gastown.daemon",
	}
	container := cutoverSide{
		name:     config.SupervisorContainer,
		owner:    cutoverContainer,
		stop:     fixedArgv("docker", "compose", "stop", "daemon"),
		start:    fixedArgv("docker", "compose", "up", "-d", "daemon"),
		job:      func() templates.SupervisorState { return r.toJob },
		stopped:  containerStopped,
		restore:  "docker compose up -d daemon",
		diagnose: "docker logs scratch-daemon-1",
	}
	p := &cutoverPlan{
		townRoot: "/town",
		clk:      r.clk,
		out:      &r.out,
		state: func() (*daemon.State, error) {
			if r.state != nil && r.beat != nil {
				st := *r.state
				st.HeartbeatCount = r.beat()
				return &st, nil
			}
			return r.state, nil
		},
		run: func(argv []string) error {
			r.calls = append(r.calls, strings.Join(argv, " "))
			if err := r.runErr[argv[0]+" "+argv[1]]; err != nil {
				return err
			}
			switch {
			case argv[0] == "launchctl" && argv[1] == "bootout", argv[0] == "docker" && argv[2] == "stop":
				if r.onStop != nil {
					r.onStop(r)
				}
			case argv[1] == "kickstart", argv[2] == "up":
				if r.onStart != nil {
					r.onStart(r)
				}
			}
			return nil
		},
		quiet: func(ctx context.Context, budget time.Duration) error {
			r.quietCalls++
			if r.quietAnswer != nil {
				return r.quietAnswer(r.quietCalls)
			}
			return nil
		},
		guard: func(side cutoverSide) error {
			r.guarded = append(r.guarded, side.owner)
			return r.guardErr
		},
	}
	if to == config.SupervisorContainer {
		p.from, p.to, p.rollback = launchd, container, "gt daemon cutover --to launchd"
	} else {
		p.from, p.to, p.rollback = container, launchd, "gt daemon cutover --to container"
	}
	return p
}

func fixedArgv(a ...string) func() ([]string, error) {
	return func() ([]string, error) { return a, nil }
}

// elapsed is how much fake time the rig's clock has moved.
func (r *cutoverRig) elapsed() time.Duration { return r.clk.Since(r.start) }

func TestCutoverToContainerSucceedsInOrder(t *testing.T) {
	t.Parallel()
	r := toContainerRig()
	if err := r.plan(config.SupervisorContainer).execute(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, r.out.String())
	}
	want := []string{"launchctl bootout gui/501/com.gastown.daemon", "docker compose up -d daemon"}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands = %v, want %v", r.calls, want)
	}
	if r.quietCalls != 2 {
		t.Errorf("quiet checked %d times, want twice (steps 1 and 2)", r.quietCalls)
	}
	if len(r.guarded) != 1 || r.guarded[0] != cutoverContainer {
		t.Errorf("guard asked about %v, want the container owner once, before its start", r.guarded)
	}
}

// --to launchd is the same shape the other way: quiet, the container stop, a
// clean stop, then the launchd bootstrap and kickstart in that order.
func TestCutoverToLaunchdStopsTheContainerThenBootstrapsAndKicksstarts(t *testing.T) {
	t.Parallel()
	r := newCutoverRig()
	r.state = &daemon.State{Running: true, Owner: &cutoverContainer, HeartbeatCount: 40}
	r.toJob = templates.SupervisorState{Loaded: true, Status: "running", LastExit: -1}
	r.onStop = func(r *cutoverRig) {
		r.state = &daemon.State{Running: false, Owner: &cutoverContainer, HeartbeatCount: 40}
		r.toJob = templates.SupervisorState{Loaded: true, Status: "exited", LastExit: -1}
	}
	r.onStart = func(r *cutoverRig) { r.beatsFrom(r.clk.Now(), &cutoverLaunchd) }
	// For --to launchd the container is the side that goes down.
	p := r.plan(config.SupervisorLaunchd)
	p.from.job = func() templates.SupervisorState { return r.toJob }

	if err := p.execute(context.Background()); err != nil {
		t.Fatalf("execute: %v\n%s", err, r.out.String())
	}
	want := []string{
		"docker compose stop daemon",
		"launchctl bootstrap gui/501 /plist",
		"launchctl kickstart -k gui/501/com.gastown.daemon",
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands = %v, want %v", r.calls, want)
	}
	if r.quietCalls != 2 {
		t.Errorf("quiet checked %d times, want twice", r.quietCalls)
	}
	if len(r.guarded) != 1 || r.guarded[0] != cutoverLaunchd {
		t.Errorf("guard asked about %v, want the launchd owner once", r.guarded)
	}
}

// A town that is not quiet at step 1 or step 2 has nothing run, and each wait
// ends on the fake clock at its budget naming the blocker. These run the real
// WaitQuiet over probes: the hold is on for the first check and off for the
// second.
func TestCutoverNotQuietRunsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		heldAt     func(read int) bool
		wantBudget time.Duration
	}{
		{"never quiet at step 1", func(int) bool { return false }, cutoverQuietBudget},
		{"quiet at step 1, not at step 2", func(read int) bool { return read == 1 }, cutoverRecheckBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := toContainerRig()
			p := r.plan(config.SupervisorContainer)
			reads := 0
			probes := QuietProbes{
				DispatchHeld: func() (bool, error) { reads++; return tc.heldAt(reads), nil },
				SlingRunning: func() (bool, error) { return false, nil },
				GateBusy:     func() (bool, error) { return false, nil },
			}
			p.quiet = func(ctx context.Context, budget time.Duration) error {
				return WaitQuiet(ctx, probes, r.clk, budget, defaultQuietPoll)
			}

			err := p.execute(context.Background())

			if err == nil || !strings.Contains(err.Error(), "the dispatch hold is not on") || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("error = %v, want a refusal naming the dispatch hold and that nothing changed", err)
			}
			if len(r.calls) != 0 {
				t.Errorf("a town that is not quiet ran %v", r.calls)
			}
			if r.elapsed() < tc.wantBudget {
				t.Errorf("the wait ended after %s of fake time, want it to run to its %s budget", r.elapsed(), tc.wantBudget)
			}
		})
	}
}

// Step 3 passes only on a clean stop in state.json AND the old job reading as
// not running, both known. A heartbeat that stopped moving, a state that still
// says running, a job that is still loaded and a job state that cannot be read
// are each not proof, and a stopped heartbeat alone is never accepted.
func TestCutoverOldDaemonStoppedNeedsBothProofs(t *testing.T) {
	t.Parallel()
	stale := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		state *daemon.State
		job   templates.SupervisorState
		want  bool
	}{
		{"clean stop and the job unloaded", &daemon.State{Running: false, Owner: &cutoverLaunchd}, templates.SupervisorState{}, true},
		{"a stale heartbeat with the state still running and the job unloaded", &daemon.State{Running: true, LastHeartbeat: stale, Owner: &cutoverLaunchd}, templates.SupervisorState{}, false},
		{"a stale heartbeat with the state still running and the job loaded", &daemon.State{Running: true, LastHeartbeat: stale, Owner: &cutoverLaunchd}, templates.SupervisorState{Loaded: true, PID: 7}, false},
		{"a clean stop but the job still loaded", &daemon.State{Running: false, Owner: &cutoverLaunchd}, templates.SupervisorState{Loaded: true, PID: 7}, false},
		{"a clean stop but the job state unknown", &daemon.State{Running: false, Owner: &cutoverLaunchd}, templates.SupervisorState{Err: errors.New("launchctl: boom")}, false},
		{"no state.json", nil, templates.SupervisorState{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCutoverRig()
			r.state, r.fromJob = tc.state, tc.job
			got, why := r.plan(config.SupervisorContainer).oldDaemonStopped()
			if got != tc.want {
				t.Errorf("oldDaemonStopped = %v (%s), want %v", got, why, tc.want)
			}
			if !got && why == "" {
				t.Error("a refusal of the proof names no reason")
			}
		})
	}
}

// A state.json that cannot be read is no proof either, and the cutover reports
// why.
func TestCutoverUnreadableStateIsNoProof(t *testing.T) {
	t.Parallel()
	r := toContainerRig()
	p := r.plan(config.SupervisorContainer)
	p.state = func() (*daemon.State, error) { return nil, errors.New("read failed") }
	if ok, why := p.oldDaemonStopped(); ok || !strings.Contains(why, "read failed") {
		t.Errorf("oldDaemonStopped = (%v, %q), want not proven and the read error named", ok, why)
	}
	if ok, why := p.newDaemonOwns(1); ok || !strings.Contains(why, "read failed") {
		t.Errorf("newDaemonOwns = (%v, %q), want not owned and the read error named", ok, why)
	}
}

// Step 3 times out when the old daemon is never proven stopped: the container
// is not started, the budget on the fake clock is used up, and the command that
// restores the launchd job is printed.
func TestCutoverStopTimeoutNeverStartsTheNewSide(t *testing.T) {
	t.Parallel()
	r := toContainerRig()
	// The job is booted out but state.json keeps saying running: a hung daemon.
	r.onStop = func(r *cutoverRig) { r.fromJob = templates.SupervisorState{Loaded: false, LastExit: -1} }

	err := r.plan(config.SupervisorContainer).execute(context.Background())

	if err == nil || !strings.Contains(err.Error(), "not proven stopped") {
		t.Fatalf("error = %v, want the old daemon not proven stopped", err)
	}
	if got := strings.Join(r.calls, "\n"); got != "launchctl bootout gui/501/com.gastown.daemon" {
		t.Errorf("commands = %v, want only the bootout", r.calls)
	}
	if r.elapsed() < cutoverStopBudget {
		t.Errorf("the wait ended after %s, want it to run to %s", r.elapsed(), cutoverStopBudget)
	}
	out := r.out.String()
	for _, want := range []string{"not proven stopped", "launchctl bootstrap gui/501 /plist"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(r.guarded) != 0 {
		t.Errorf("guard asked before the old daemon was proven stopped: %v", r.guarded)
	}
}

// Step 4 times out when the container never becomes the owner: the rollback
// command and the logs command are printed, no further command runs, and
// nothing is rolled back.
func TestCutoverTakeTimeoutPrintsTheRollbackAndRunsNothingMore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state *daemon.State
	}{
		{"the old owner is still recorded", &daemon.State{Running: false, Owner: &cutoverLaunchd}},
		{"the old owner is recorded running", &daemon.State{Running: true, Owner: &cutoverLaunchd, HeartbeatCount: 5}},
		{"the container is recorded but not running", &daemon.State{Running: false, Owner: &cutoverContainer}},
		{"the container is recorded not running with heartbeats written", &daemon.State{Running: false, Owner: &cutoverContainer, HeartbeatCount: 3}},
		{"state.json is missing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := toContainerRig()
			r.onStart = func(r *cutoverRig) { r.state = tc.state }

			err := r.plan(config.SupervisorContainer).execute(context.Background())

			if err == nil || !strings.Contains(err.Error(), "never took the town") {
				t.Fatalf("error = %v, want the container never took the town", err)
			}
			if len(r.calls) != 2 {
				t.Errorf("commands = %v, want the bootout and the start and nothing after", r.calls)
			}
			if r.elapsed() < cutoverTakeBudget {
				t.Errorf("elapsed %s, want the take wait run to its %s budget", r.elapsed(), cutoverTakeBudget)
			}
			out := r.out.String()
			for _, want := range []string{"never took the town", "gt daemon cutover --to launchd", "docker logs scratch-daemon-1"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

// Step 5 times out when the container owns the town but never writes a second
// heartbeat: it is reported unhealthy with the rollback command, and no further
// command runs.
func TestCutoverHeartbeatTimeoutReportsUnhealthy(t *testing.T) {
	t.Parallel()
	r := toContainerRig()
	// The container takes the town and writes its first heartbeat, then stops.
	r.onStart = func(r *cutoverRig) {
		r.state = &daemon.State{Running: true, Owner: &cutoverContainer}
		r.beat = func() int64 { return 1 }
	}

	err := r.plan(config.SupervisorContainer).execute(context.Background())

	if err == nil || !strings.Contains(err.Error(), "no second heartbeat") {
		t.Fatalf("error = %v, want no second heartbeat", err)
	}
	if len(r.calls) != 2 {
		t.Errorf("commands = %v, want the bootout and the start and nothing after", r.calls)
	}
	if r.elapsed() < cutoverHeartbeatBudget {
		t.Errorf("elapsed %s, want the heartbeat wait run to its %s budget", r.elapsed(), cutoverHeartbeatBudget)
	}
	out := r.out.String()
	for _, want := range []string{"unhealthy", "gt daemon cutover --to launchd", "docker logs scratch-daemon-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// The startup guard is asked before the new side starts, and its refusal stops
// the cutover with the old side stopped and its restore command printed.
func TestCutoverGuardRefusalDoesNotStartTheNewSide(t *testing.T) {
	t.Parallel()
	r := toContainerRig()
	r.guardErr = errors.New("heartbeat is fresh")

	err := r.plan(config.SupervisorContainer).execute(context.Background())

	if err == nil || !strings.Contains(err.Error(), "startup guard refused") {
		t.Fatalf("error = %v, want the guard's refusal", err)
	}
	if got := strings.Join(r.calls, "\n"); got != "launchctl bootout gui/501/com.gastown.daemon" {
		t.Errorf("commands = %v, want only the bootout", r.calls)
	}
	if !strings.Contains(r.out.String(), "launchctl bootstrap gui/501 /plist") {
		t.Errorf("output lacks the restore command:\n%s", r.out.String())
	}
}

// A command that fails ends the cutover at that step: a failed stop starts
// nothing, and a failed or refused start leaves the restore command printed.
func TestCutoverCommandFailuresStopTheCutover(t *testing.T) {
	t.Parallel()
	t.Run("the stop fails", func(t *testing.T) {
		t.Parallel()
		r := toContainerRig()
		r.runErr["launchctl bootout"] = errors.New("launchctl: boom")
		err := r.plan(config.SupervisorContainer).execute(context.Background())
		if err == nil || !strings.Contains(err.Error(), "was not started") {
			t.Fatalf("error = %v, want the new side not started", err)
		}
		if len(r.calls) != 1 {
			t.Errorf("commands = %v, want only the failed stop", r.calls)
		}
	})
	t.Run("the start fails", func(t *testing.T) {
		t.Parallel()
		r := toContainerRig()
		r.runErr["docker compose"] = errors.New("docker: boom")
		err := r.plan(config.SupervisorContainer).execute(context.Background())
		if err == nil || !strings.Contains(err.Error(), "starting container failed") {
			t.Fatalf("error = %v, want the start failure", err)
		}
		if !strings.Contains(r.out.String(), "launchctl bootstrap gui/501 /plist") {
			t.Errorf("output lacks the restore command:\n%s", r.out.String())
		}
	})
	t.Run("the supervisor refuses the start", func(t *testing.T) {
		t.Parallel()
		r := toContainerRig()
		p := r.plan(config.SupervisorContainer)
		p.to.start = func() ([]string, error) { return nil, errSupervisorRefused }
		err := p.execute(context.Background())
		if !errors.Is(err, errSupervisorRefused) {
			t.Fatalf("error = %v, want the supervisor's refusal", err)
		}
		if len(r.calls) != 1 {
			t.Errorf("commands = %v, want only the bootout", r.calls)
		}
	})
}

// A cancelled context ends a wait as not done rather than polling on.
func TestCutoverWaitEndsOnACancelledContext(t *testing.T) {
	t.Parallel()
	r := toContainerRig()
	p := r.plan(config.SupervisorContainer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A fake clock whose After never fires, so only the context can end it.
	p.clk = clockwork.NewFakeClockAt(quietTestEpoch)
	done, why := p.waitFor(ctx, time.Hour, func() (bool, string) { return false, "not yet" })
	if done || !strings.Contains(why, "context canceled") {
		t.Errorf("waitFor = (%v, %q), want not done and the cancellation named", done, why)
	}
}

func TestContainerAndLaunchdStopProof(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		st   templates.SupervisorState
		fn   func(templates.SupervisorState) bool
		want bool
	}{
		{"container absent", templates.SupervisorState{}, containerStopped, true},
		{"container exited", templates.SupervisorState{Loaded: true, Status: "exited"}, containerStopped, true},
		{"container running", templates.SupervisorState{Loaded: true, Status: "running"}, containerStopped, false},
		{"container restarting", templates.SupervisorState{Loaded: true, Status: "restarting"}, containerStopped, false},
		{"container unread", templates.SupervisorState{Err: boom}, containerStopped, false},
		{"launchd unloaded", templates.SupervisorState{}, launchdStopped, true},
		{"launchd loaded", templates.SupervisorState{Loaded: true}, launchdStopped, false},
		{"launchd unread", templates.SupervisorState{Err: boom}, launchdStopped, false},
	} {
		if got := tc.fn(tc.st); got != tc.want {
			t.Errorf("%s: stopped = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The sides come from the town's own settings, and a town that sets neither
// container field is refused before any command is built.
func TestCutoverSidesAndTargets(t *testing.T) {
	t.Parallel()
	town := startGuardTown(t)
	writeComposeFile(t, town)
	c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	plist := writeSupervisorFile(t, "com.gastown.daemon.plist", town)
	c.plistPath = func() (string, error) { return plist, nil }

	launchd, container, err := c.cutoverSides(town)
	if err != nil {
		t.Fatalf("cutoverSides: %v", err)
	}
	if launchd.owner != cutoverLaunchd || container.owner != cutoverContainer {
		t.Errorf("owners = %+v and %+v, want the launchd label and the town's container", launchd.owner, container.owner)
	}
	for _, to := range []string{config.SupervisorContainer, config.SupervisorLaunchd} {
		p, err := c.cutoverTo(town, to, QuietProbes{}, clockwork.NewFakeClockAt(quietTestEpoch), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("cutoverTo(%s): %v", to, err)
		}
		if p.to.name != to {
			t.Errorf("cutoverTo(%s) goes to %s", to, p.to.name)
		}
	}
	if _, err := c.cutoverTo(town, "systemd", QuietProbes{}, clockwork.NewFakeClockAt(quietTestEpoch), &bytes.Buffer{}); err == nil {
		t.Error("cutoverTo accepted a target that is neither container nor launchd")
	}

	unset, _ := stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})
	unset.plistPath = c.plistPath
	if _, _, err := unset.cutoverSides(town); err == nil {
		t.Error("cutoverSides accepted a town that sets neither container field")
	}
	c.plistPath = func() (string, error) { return "", errors.New("no home") }
	if _, _, err := c.cutoverSides(town); err == nil {
		t.Error("cutoverSides ignored a plist path error")
	}
	c.plistPath = func() (string, error) { return plist, nil }
	c.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) { return nil, errors.New("settings unreadable") }
	if _, _, err := c.cutoverSides(town); err == nil {
		t.Error("cutoverSides ignored unreadable settings")
	}
}

// The production guard the cutover asks is the control's guard, with the new
// side's owner and no takeover.
func TestCutoverGuardAsksTheControlsGuard(t *testing.T) {
	t.Parallel()
	town := startGuardTown(t)
	c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	var gotSelf daemon.Owner
	var gotTakeover bool
	c.guard = func(_ string, self daemon.Owner, takeover bool) error {
		gotSelf, gotTakeover = self, takeover
		return errors.New("refused")
	}
	if err := c.guardCutoverStart(town, cutoverSide{owner: cutoverContainer}); err == nil {
		t.Fatal("the guard's refusal was swallowed")
	}
	if gotSelf != cutoverContainer || gotTakeover {
		t.Errorf("guard asked about %+v takeover=%v, want the side's owner and no takeover", gotSelf, gotTakeover)
	}
}

func TestReadCutoverState(t *testing.T) {
	t.Parallel()
	town := startGuardTown(t)
	if st, err := readCutoverState(town); st != nil || err != nil {
		t.Fatalf("missing state.json = (%v, %v), want (nil, nil)", st, err)
	}
	writeTownState(t, town, freshHeartbeatOf(cutoverContainer))
	st, err := readCutoverState(town)
	if err != nil || st == nil || st.Owner == nil || *st.Owner != cutoverContainer {
		t.Fatalf("readCutoverState = (%+v, %v), want the container's state", st, err)
	}
	writeTownDaemonFile(t, town, "state.json", []byte("{not json"))
	if _, err := readCutoverState(town); err == nil {
		t.Error("an unparsable state.json read as a state")
	}
	if err := os.Remove(filepath.Join(town, "daemon", "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(town, "daemon", "state.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readCutoverState(town); err == nil {
		t.Error("a state.json that cannot be read (a directory) read as missing")
	}
}

// The production Deploy supervisor offers the container the town's settings
// name and the start argv for the same project, and refuses a town that sets
// neither field, so no default container name is ever offered for a render.
func TestDeploySupervisorIsBuiltFromTheTownsSettings(t *testing.T) {
	t.Parallel()
	town := startGuardTown(t)
	writeComposeFile(t, town)
	c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))

	sup, err := c.deploySupervisor(town)
	if err != nil {
		t.Fatalf("deploySupervisor: %v", err)
	}
	var _ daemonimage.Supervisor = sup
	if got := sup.ContainerName(); got != "scratch-daemon-1" {
		t.Errorf("ContainerName = %q, want the town's container", got)
	}
	argv, err := sup.StartArgv()
	if err != nil {
		t.Fatalf("StartArgv: %v", err)
	}
	if got := strings.Join(argv, " "); !strings.Contains(got, " -p scratch ") || !strings.HasSuffix(got, " up -d daemon") {
		t.Errorf("start argv = %q, want the town's compose project and up -d daemon", got)
	}

	unset, _ := stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})
	if _, err := unset.deploySupervisor(town); err == nil {
		t.Error("deploySupervisor offered a supervisor for a town that sets neither container field")
	}
	unread, _ := stubContainerTown(t, nil)
	unread.patrolConfig = func(string) (*config.DaemonPatrolConfig, error) { return nil, errors.New("settings unreadable") }
	if _, err := unread.deploySupervisor(town); err == nil {
		t.Error("deploySupervisor ignored unreadable settings")
	}
	noCompose, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	if _, err := noCompose.deploySupervisor(startGuardTown(t)); err == nil {
		t.Error("deploySupervisor accepted a town with no compose file")
	}
}

// The five budgets are the ones the bead states, so a change to one is a
// change to this list too.
func TestCutoverBudgetsAreTheStatedOnes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		got, want time.Duration
	}{
		{"step 1 waits for a quiet town", cutoverQuietBudget, 30 * time.Minute},
		{"step 2 is one more poll", cutoverRecheckBudget, 15 * time.Second},
		{"step 3 is the shutdown budget and 30 seconds", cutoverStopBudget, daemon.ShutdownBudget + 30*time.Second},
		{"step 4 waits 2 minutes for the owner", cutoverTakeBudget, 2 * time.Minute},
		{"step 5 is the heartbeat interval and a minute", cutoverHeartbeatBudget, 6 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: budget is %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}
