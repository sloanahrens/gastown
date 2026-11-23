package vmstart

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// queueRunner answers each command from a per-command queue and records every
// call in order; a command with an empty queue is an error, so a run that makes
// one call more than the test scripted fails instead of passing quietly.
type queueRunner struct {
	queues map[string][]reply
	calls  [][]string
}

func (r *queueRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	key := cmdKey(argv)
	q := r.queues[key]
	if len(q) == 0 {
		return nil, fmt.Errorf("unexpected command %q", key)
	}
	r.queues[key] = q[1:]
	return []byte(q[0].out), q[0].err
}

// startArgv is the command Start runs to bring the named VM up.
func startArgv(profile string) []string {
	return []string{"colima", "start", "-p", profile, "--activate=false"}
}

// containersArgv is the command Start runs after both VMs, written out here
// rather than read from containerStartArgv so a change to it fails a test.
var containersArgv = []string{"docker", "--context", "colima-gt-sbx", "start",
	"gt-sbx-control-plane", "gt-sbx-worker", "gt-sbx-worker2", "gt-registry"}

// recordingSleep records every pause Start takes.
func recordingSleep(slept *[]time.Duration) func(time.Duration) {
	return func(d time.Duration) { *slept = append(*slept, d) }
}

// startListenerSandbox is the ps command line of a sandbox forward that owns
// the Mac's registry port, the owner the repair leaves alone.
const startListenerSandbox = "ssh -S /Users/me/.colima/_lima/colima-gt-sbx/ssh.sock -L 127.0.0.1:5001:127.0.0.1:5001"

// answeredRead is what CheckDNS prints for a VM whose lookup answers.
const answeredRead = testResolvConf + "140.82.113.4  github.com\n"

// healthyRepairs queues the reads the repairs make when both VMs answer DNS and
// a healthy sandbox forward owns the registry port, so no repair runs a
// command.
func healthyRepairs() map[string][]reply {
	return map[string][]reply{
		cmdKey(readArgv("gt-sbx")):     {{out: answeredRead}},
		cmdKey(readArgv("gt-control")): {{out: answeredRead}},
		cmdKey(registryLsofArgv):       {{out: "p4242\n"}},
		cmdKey(repairPSArgv("4242")):   {{out: startListenerSandbox}},
		cmdKey(registryCurlArgv):       {{out: "200"}},
	}
}

// startQueues merges the named VM start and container replies into the healthy
// repair reads of a run.
func startQueues(starts map[string][]reply) map[string][]reply {
	queues := healthyRepairs()
	for key, replies := range starts {
		queues[key] = replies
	}
	return queues
}

// repairReads are the calls Start makes after the container start when both VMs
// answer DNS and the sandbox forward healthily owns the registry port.
var repairReads = [][]string{
	readArgv("gt-sbx"), readArgv("gt-control"),
	registryLsofArgv, repairPSArgv("4242"), registryCurlArgv,
}

// startStarts queues the three VM and container starts of a run where all of
// them pass.
func startStarts() map[string][]reply {
	return map[string][]reply{
		cmdKey(startArgv("gt-sbx")):     {{}},
		cmdKey(startArgv("gt-control")): {{}},
		cmdKey(containersArgv):          {{}},
	}
}

// reportCommands lists the report's command column in row order.
func reportCommands(report Report) []string {
	cmds := make([]string, 0, len(report))
	for _, row := range report {
		cmds = append(cmds, row.Command)
	}
	return cmds
}

// callIndexAfter is where calls first holds argv at or after from, or -1.
func callIndexAfter(calls [][]string, argv []string, from int) int {
	for i := from; i < len(calls); i++ {
		if reflect.DeepEqual(calls[i], argv) {
			return i
		}
	}
	return -1
}

// planOrderErr is nil when each step's first call lies after the call matched
// for the step before it, so a call made earlier cannot satisfy it.
func planOrderErr(calls [][]string) error {
	// firstCall is the command each step runs first, written out here so a run
	// that takes the steps in another order fails this check.
	firstCall := map[string][]string{
		"gt-sbx":     startArgv("gt-sbx"),
		"gt-control": startArgv("gt-control"),
		"containers": containersArgv,
		"DNS":        readArgv("gt-sbx"),
		"registry":   registryLsofArgv,
	}
	prev := -1
	for _, step := range StartPlan() {
		argv, ok := firstCall[step.Name]
		if !ok {
			return fmt.Errorf("step %q has no first call written out for it", step.Name)
		}
		at := callIndexAfter(calls, argv, prev+1)
		if at <= prev {
			return fmt.Errorf("step %q ran no %q after the step before it at %d: calls = %q", step.Name, argv, prev, calls)
		}
		prev = at
	}
	return nil
}

// TestVMStartPlanOrder: StartPlan returns the order `gt town vms start
// --dry-run` prints, and a caller's change to its copy cannot change the plan.
func TestVMStartPlanOrder(t *testing.T) {
	t.Parallel()
	var names []string
	for _, step := range StartPlan() {
		names = append(names, step.Name)
	}
	if want := []string{"gt-sbx", "gt-control", "containers", "DNS", "registry"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("StartPlan returns %q, want %q", names, want)
	}

	// A caller that changes the plan it gets cannot change what Start runs.
	mine := StartPlan()
	want := mine[0].Name
	mine[0] = StartStep{Name: "changed"}
	if got := StartPlan()[0].Name; got != want {
		t.Fatalf("StartPlan() after a caller changed its copy = %q, want %q", got, want)
	}
}

// TestVMStartRunOrder: a run where every step passes takes the steps in plan
// order, sleeps nowhere, and reports one row per step. It checks that order
// rather than the whole call list (gt-2dbzp1).
func TestVMStartRunOrder(t *testing.T) {
	t.Parallel()
	r := &queueRunner{queues: startQueues(startStarts())}
	var slept []time.Duration
	report, err := Start(context.Background(), r.run, recordingSleep(&slept), repairClock, "/home/test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := planOrderErr(r.calls); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 0 {
		t.Fatalf("slept %v, want no sleep", slept)
	}
	if len(report) != 3 {
		t.Fatalf("report = %s, want one row per step", report)
	}

	// A step's call recorded once before its own step, as a retried start
	// would be, must not satisfy the order check early.
	repeated := append([][]string{readArgv("gt-sbx")}, r.calls...)
	if err := planOrderErr(repeated); err != nil {
		t.Fatal(err)
	}

	// A run that takes the control VM before the sandbox VM must be refused.
	swapped := append([][]string(nil), r.calls...)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	if err := planOrderErr(swapped); err == nil {
		t.Fatal("planOrderErr over the control VM before the sandbox VM = nil, want an order error")
	}
}

// TestVMStartRunRetries: a VM that fails twice and starts on the third try
// sleeps 20 seconds between tries and is no error; a VM that fails all three is
// recorded and the later profile and the containers still run.
func TestVMStartRunRetries(t *testing.T) {
	t.Parallel()
	exit := errors.New("exit status 1")
	cases := map[string]struct {
		sbxReplies []reply
		wantSleeps []time.Duration
		wantErr    bool
	}{
		"fails twice then starts": {
			sbxReplies: []reply{{err: exit}, {err: exit}, {}},
			wantSleeps: []time.Duration{20 * time.Second, 20 * time.Second},
		},
		"fails three times": {
			sbxReplies: []reply{{err: exit}, {err: exit}, {err: exit}},
			wantSleeps: []time.Duration{20 * time.Second, 20 * time.Second},
			wantErr:    true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &queueRunner{queues: startQueues(map[string][]reply{
				cmdKey(startArgv("gt-sbx")):     tc.sbxReplies,
				cmdKey(startArgv("gt-control")): {{}},
				cmdKey(containersArgv):          {{}},
			})}
			var slept []time.Duration
			report, err := Start(context.Background(), r.run, recordingSleep(&slept), repairClock, "/home/test")
			if !reflect.DeepEqual(slept, tc.wantSleeps) {
				t.Fatalf("slept %v, want %v", slept, tc.wantSleeps)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Start returned nil after three failed tries: %s", report)
				}
				if !strings.Contains(err.Error(), strings.Join(startArgv("gt-sbx"), " ")) {
					t.Fatalf("error = %q, want it to name %q", err, strings.Join(startArgv("gt-sbx"), " "))
				}
				if !reportFailed(report, "gt-sbx") {
					t.Fatalf("report = %s, want the failed gt-sbx step recorded", report)
				}
			} else if err != nil {
				t.Fatalf("Start: %v", err)
			}
			want := append([][]string{startArgv("gt-sbx"), startArgv("gt-sbx"), startArgv("gt-sbx"),
				startArgv("gt-control"), containersArgv}, repairReads...)
			if !reflect.DeepEqual(r.calls, want) {
				t.Fatalf("calls = %q, want %q", r.calls, want)
			}
		})
	}
}

// reportFailed reports whether the report holds a failed row for the profile.
func reportFailed(report Report, profile string) bool {
	for _, row := range report {
		if row.Profile == profile && row.Err != nil {
			return true
		}
	}
	return false
}

// TestVMStartCancelStopsRun: a canceled context ends Start: no later try, no
// pause after the cancel, and no later step runs. Start still returns an error
// that wraps the context's error.
func TestVMStartCancelStopsRun(t *testing.T) {
	t.Parallel()
	exit := errors.New("exit status 1")
	cases := map[string]struct {
		preCancel  bool
		sbxReplies []reply
		cancelOn   []string
		wantCalls  [][]string
	}{
		"canceled before the first start": {
			preCancel:  true,
			sbxReplies: []reply{{}},
		},
		"canceled during a failed try": {
			sbxReplies: []reply{{err: exit}, {}, {}},
			cancelOn:   startArgv("gt-sbx"),
			wantCalls:  [][]string{startArgv("gt-sbx")},
		},
		"canceled before the container step": {
			sbxReplies: []reply{{}},
			cancelOn:   startArgv("gt-control"),
			wantCalls:  [][]string{startArgv("gt-sbx"), startArgv("gt-control")},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &queueRunner{queues: startQueues(map[string][]reply{
				cmdKey(startArgv("gt-sbx")):     tc.sbxReplies,
				cmdKey(startArgv("gt-control")): {{}},
				cmdKey(containersArgv):          {{}},
			})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.preCancel {
				cancel()
			}
			run := func(c context.Context, name string, args ...string) ([]byte, error) {
				out, err := r.run(c, name, args...)
				if tc.cancelOn != nil && reflect.DeepEqual(append([]string{name}, args...), tc.cancelOn) {
					cancel()
				}
				return out, err
			}
			var slept []time.Duration
			report, err := Start(ctx, run, recordingSleep(&slept), repairClock, "/home/test")
			if err == nil {
				t.Fatalf("Start returned nil over a canceled context: %s", report)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %q, want it to wrap %v", err, context.Canceled)
			}
			if len(slept) != 0 {
				t.Fatalf("slept %v after the cancel, want no pause", slept)
			}
			if !reflect.DeepEqual(r.calls, tc.wantCalls) {
				t.Fatalf("calls = %q, want %q", r.calls, tc.wantCalls)
			}
		})
	}
}

// TestVMStartRunError: a failed container start makes Start return an error
// naming that command, after both VM starts ran, and the repairs still read
// their state.
func TestVMStartRunError(t *testing.T) {
	t.Parallel()
	exit := errors.New("exit status 1")
	r := &queueRunner{queues: startQueues(map[string][]reply{
		cmdKey(startArgv("gt-sbx")):     {{}},
		cmdKey(startArgv("gt-control")): {{}},
		cmdKey(containersArgv):          {{err: exit}},
	})}
	report, err := Start(context.Background(), r.run, func(time.Duration) {}, repairClock, "/home/test")
	if err == nil {
		t.Fatalf("Start returned nil over a failed container start: %s", report)
	}
	if !strings.Contains(err.Error(), strings.Join(containersArgv, " ")) {
		t.Fatalf("error = %q, want it to name %q", err, strings.Join(containersArgv, " "))
	}
	want := append([][]string{startArgv("gt-sbx"), startArgv("gt-control"), containersArgv}, repairReads...)
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
}

// TestVMStartRepairsOrder: after the container start the repairs read the
// sandbox VM's DNS, then the control VM's, then the registry port's owner, in
// that order.
func TestVMStartRepairsOrder(t *testing.T) {
	t.Parallel()
	r := &queueRunner{queues: startQueues(startStarts())}
	if _, err := Start(context.Background(), r.run, func(time.Duration) {}, repairClock, "/home/test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(r.calls) != 3+len(repairReads) {
		t.Fatalf("calls = %q, want the three starts and then %q", r.calls, repairReads)
	}
	if got := r.calls[3:]; !reflect.DeepEqual(got, repairReads) {
		t.Fatalf("calls after the container start = %q, want %q", got, repairReads)
	}
}

// TestVMStartRepairsReport: a failing lookup in gt-control records the DNS
// repair's steps under gt-control while the answering gt-sbx records none, and
// a repair that fails is recorded and makes Start return an error without
// stopping the registry repair.
func TestVMStartRepairsReport(t *testing.T) {
	t.Parallel()
	exit := errors.New("exit status 1")
	// dnsQueues queues the starts, a sandbox lookup that answers and a control
	// lookup that does not, with the named reply to the repair's first command.
	dnsQueues := func(backup reply) map[string][]reply {
		queues := startQueues(map[string][]reply{
			cmdKey(startArgv("gt-sbx")):     {{}},
			cmdKey(startArgv("gt-control")): {{}},
			cmdKey(containersArgv):          {{}},
			cmdKey(readArgv("gt-sbx")):      {{out: answeredRead}},
			cmdKey(readArgv("gt-control")):  {{out: testResolvConf, err: exit}},
		})
		queues[cmdKey(backupArgv("gt-control"))] = []reply{backup}
		queues[cmdKey(writeArgv("gt-control"))] = []reply{{}}
		queues[cmdKey(corednsRestartArgv)] = []reply{{}}
		return queues
	}

	t.Run("failing lookup in gt-control", func(t *testing.T) {
		t.Parallel()
		r := &queueRunner{queues: dnsQueues(reply{})}
		report, err := Start(context.Background(), r.run, func(time.Duration) {}, repairClock, repairHome)
		if err != nil {
			t.Fatalf("Start over a repair it ran cleanly: %v", err)
		}
		wantSteps := []string{cmdKey(backupArgv("gt-control")), cmdKey(writeArgv("gt-control")), cmdKey(corednsRestartArgv)}
		got := reportCommands(report)
		if len(got) != 3+len(wantSteps) {
			t.Fatalf("report = %s, want the three starts and the gt-control repair steps", report)
		}
		if !reflect.DeepEqual(got[3:], wantSteps) {
			t.Fatalf("repair steps = %q, want %q", got[3:], wantSteps)
		}
		for _, row := range report[3:] {
			if row.Profile != profileControl {
				t.Fatalf("step row = %+v, want it reported for %s", row, profileControl)
			}
		}
		for _, command := range reportCommands(report) {
			if command == cmdKey(backupArgv("gt-sbx")) {
				t.Fatalf("report = %s, want no repair step for the answering gt-sbx", report)
			}
		}
	})

	t.Run("a failed DNS repair", func(t *testing.T) {
		t.Parallel()
		r := &queueRunner{queues: dnsQueues(reply{err: exit})}
		report, err := Start(context.Background(), r.run, func(time.Duration) {}, repairClock, repairHome)
		if err == nil {
			t.Fatalf("Start returned nil over a failed DNS repair: %s", report)
		}
		if !strings.Contains(err.Error(), "repair DNS in "+profileControl) {
			t.Fatalf("error = %q, want it to name the %s repair", err, profileControl)
		}
		if n := len(report); n == 0 || report[n-1].Command != dnsRepairRow || report[n-1].Err == nil {
			t.Fatalf("report = %s, want the failed %s repair recorded", report, dnsRepairRow)
		}
		if got := r.calls[len(r.calls)-1]; !reflect.DeepEqual(got, registryCurlArgv) {
			t.Fatalf("last call = %q, want the registry read %q after the failed DNS repair", got, registryCurlArgv)
		}
	})

	t.Run("a failed registry repair", func(t *testing.T) {
		t.Parallel()
		queues := startQueues(startStarts())
		queues[cmdKey(registryLsofArgv)] = []reply{{err: errors.New("exit status 2")}}
		r := &queueRunner{queues: queues}
		report, err := Start(context.Background(), r.run, func(time.Duration) {}, repairClock, repairHome)
		if err == nil {
			t.Fatalf("Start returned nil over a failed registry repair: %s", report)
		}
		if !strings.Contains(err.Error(), registryRepairRow) {
			t.Fatalf("error = %q, want it to name %q", err, registryRepairRow)
		}
		if n := len(report); n == 0 || report[n-1].Command != registryRepairRow || report[n-1].Err == nil {
			t.Fatalf("report = %s, want the failed %s repair recorded", report, registryRepairRow)
		}
	})
}

// TestVMStartRunRefuses: a nil runner or a nil sleep is refused before any
// call, because either would panic mid-run with no report of what had run.
func TestVMStartRunRefuses(t *testing.T) {
	t.Parallel()
	calls := 0
	run := func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	sleep := func(time.Duration) {}
	cases := map[string]func() (Report, error){
		"nil runner": func() (Report, error) {
			return Start(context.Background(), nil, sleep, repairClock, "/home/test")
		},
		"nil sleep": func() (Report, error) {
			return Start(context.Background(), run, nil, repairClock, "/home/test")
		},
	}
	for name, start := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := start(); err == nil {
				t.Fatalf("Start accepted a %s", name)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("refused calls made %d runs", calls)
	}
}

// TestVMStartReportString: the report prints one line per row, with the
// profile, the command, and "ok" or the error.
func TestVMStartReportString(t *testing.T) {
	t.Parallel()
	report := Report{
		{Profile: "gt-sbx", Command: "colima start -p gt-sbx --activate=false"},
		{Profile: "gt-sbx", Command: "colima start -p gt-sbx --activate=false", Err: errors.New("exit status 1")},
	}
	got := report.String()
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("String() = %q, want two lines", got)
	}
	if want := "gt-sbx  colima start -p gt-sbx --activate=false  ok"; lines[0] != want {
		t.Fatalf("first line = %q, want %q", lines[0], want)
	}
	if want := "gt-sbx  colima start -p gt-sbx --activate=false  error: exit status 1"; lines[1] != want {
		t.Fatalf("second line = %q, want %q", lines[1], want)
	}
}
