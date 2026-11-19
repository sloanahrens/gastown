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

// TestVMStartRunOrder: the sandbox VM starts first, then the control VM, then
// the containers, and a run where every step passes sleeps nowhere.
func TestVMStartRunOrder(t *testing.T) {
	t.Parallel()
	r := &queueRunner{queues: map[string][]reply{
		cmdKey(startArgv("gt-sbx")):     {{}},
		cmdKey(startArgv("gt-control")): {{}},
		cmdKey(containersArgv):          {{}},
	}}
	var slept []time.Duration
	report, err := Start(context.Background(), r.run, recordingSleep(&slept), time.Now, "/home/test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := [][]string{startArgv("gt-sbx"), startArgv("gt-control"), containersArgv}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
	if len(slept) != 0 {
		t.Fatalf("slept %v, want no sleep", slept)
	}
	if len(report) != 3 {
		t.Fatalf("report = %s, want one row per step", report)
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
			r := &queueRunner{queues: map[string][]reply{
				cmdKey(startArgv("gt-sbx")):     tc.sbxReplies,
				cmdKey(startArgv("gt-control")): {{}},
				cmdKey(containersArgv):          {{}},
			}}
			var slept []time.Duration
			report, err := Start(context.Background(), r.run, recordingSleep(&slept), time.Now, "/home/test")
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
			want := [][]string{startArgv("gt-sbx"), startArgv("gt-sbx"), startArgv("gt-sbx"),
				startArgv("gt-control"), containersArgv}
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

// TestVMStartRunError: a failed container start makes Start return an error
// naming that command, after both VM starts ran.
func TestVMStartRunError(t *testing.T) {
	t.Parallel()
	exit := errors.New("exit status 1")
	r := &queueRunner{queues: map[string][]reply{
		cmdKey(startArgv("gt-sbx")):     {{}},
		cmdKey(startArgv("gt-control")): {{}},
		cmdKey(containersArgv):          {{err: exit}},
	}}
	report, err := Start(context.Background(), r.run, func(time.Duration) {}, time.Now, "/home/test")
	if err == nil {
		t.Fatalf("Start returned nil over a failed container start: %s", report)
	}
	if !strings.Contains(err.Error(), strings.Join(containersArgv, " ")) {
		t.Fatalf("error = %q, want it to name %q", err, strings.Join(containersArgv, " "))
	}
	if len(r.calls) != 3 {
		t.Fatalf("calls = %q, want both starts and the container start", r.calls)
	}
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
			return Start(context.Background(), nil, sleep, time.Now, "/home/test")
		},
		"nil sleep": func() (Report, error) {
			return Start(context.Background(), run, nil, time.Now, "/home/test")
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
