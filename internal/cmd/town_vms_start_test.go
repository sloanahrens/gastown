package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

const (
	// townVMsStartHome is the home directory the fake user home resolves to, so
	// no test reads the host's home.
	townVMsStartHome = "/Users/operator"
	// townVMsStartLogLine is the log line the fake tail prints: a forward that
	// names its port, the shape the registry repair reads.
	townVMsStartLogLine = "debug1: remote forward success for: listen 5001, connect 127.0.0.1:5001 (-p 45678 127.0.0.1)\n"
)

// townVMsStartNow is the clock the fake env returns, so the DNS backup stamp in
// a repair is fixed.
var townVMsStartNow = time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)

// townVMsStartCall is one command the fake runner was asked to run.
type townVMsStartCall struct {
	name string
	args []string
}

func (c townVMsStartCall) String() string {
	return strings.Join(append([]string{c.name}, c.args...), " ")
}

// townVMsStartHost is a fake Mac: it answers the reads Start makes as a host
// with both VMs up, DNS answering and the sandbox VM's forward owning the
// registry, and records every command. fail, when set, answers a call with an
// error instead.
type townVMsStartHost struct {
	calls []townVMsStartCall
	fail  func(townVMsStartCall) error
	// listens is what lsof prints; listening false answers its exit 1 instead.
	listens       bool
	listenerOwner string
}

// newTownVMsStartHost is a host whose VMs are up and whose sandbox forward owns
// a healthy registry: Start runs three steps and no repair.
func newTownVMsStartHost() *townVMsStartHost {
	return &townVMsStartHost{listens: true, listenerOwner: "colima-gt-sbx"}
}

func (h *townVMsStartHost) runner(_ context.Context, name string, args ...string) ([]byte, error) {
	call := townVMsStartCall{name: name, args: args}
	h.calls = append(h.calls, call)
	if h.fail != nil {
		if err := h.fail(call); err != nil {
			return nil, err
		}
	}
	switch name {
	case "colima":
		if len(args) > 0 && args[0] == "ssh" {
			return []byte("nameserver 192.168.5.2\ngetent: github.com\n"), nil
		}
	case "lsof":
		if !h.listens {
			return nil, townVMsStartExitError{code: 1}
		}
		return []byte("p4242\n"), nil
	case "ps":
		return []byte("ssh -S " + filepath.Join(townVMsStartHome, ".colima/_lima", h.listenerOwner, "ssh.sock") + " 127.0.0.1\n"), nil
	case "curl":
		return []byte("200"), nil
	case "tail":
		return []byte(townVMsStartLogLine), nil
	}
	return nil, nil
}

// ran reports whether the runner ran a command whose line equals call.
func (h *townVMsStartHost) ran(call string) bool {
	for _, c := range h.calls {
		if c.String() == call {
			return true
		}
	}
	return false
}

// townVMsStartExitError is a command's non-zero exit, the shape the Runner
// contract gives one.
type townVMsStartExitError struct{ code int }

func (e townVMsStartExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e townVMsStartExitError) ExitCode() int { return e.code }

// townVMsStartFakeEnv is the env the tests start from: the fake host, the fake
// user home and a clock and wait that do nothing.
func townVMsStartFakeEnv(host *townVMsStartHost) townVMsStartEnv {
	return townVMsStartEnv{
		getenv: func(string) string { return "" },
		home:   func() (string, error) { return townVMsStartHome, nil },
		runner: host.runner,
		sleep:  func(time.Duration) {},
		now:    func() time.Time { return townVMsStartNow },
	}
}

// execTownVMsStart runs the production `start` command with env and args,
// returning its stdout, stderr and error. out, when set, replaces stdout.
func execTownVMsStart(env townVMsStartEnv, out io.Writer, args []string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	c := newTownVMsStartCmd(env)
	if out == nil {
		out = &stdout
	}
	c.SetOut(out)
	c.SetErr(&stderr)
	c.SetArgs(args)
	err := c.Execute()
	return stdout.String(), stderr.String(), err
}

// errorWriter fails every write, the shape of a closed pipe.
type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("pipe is closed") }

// TestTownVMsStartOK: a host whose VMs and registry are healthy prints one
// line per step Start ran and exits 0.
func TestTownVMsStartOK(t *testing.T) {
	t.Parallel()
	host := newTownVMsStartHost()
	stdout, _, err := execTownVMsStart(townVMsStartFakeEnv(host), nil, nil)
	if err != nil {
		t.Fatalf("start on a healthy host = %v, want exits 0", err)
	}
	want := "gt-sbx  colima start -p gt-sbx --activate=false  ok\n" +
		"gt-control  colima start -p gt-control --activate=false  ok\n" +
		"gt-sbx  docker --context colima-gt-sbx start gt-sbx-control-plane gt-sbx-worker gt-sbx-worker2 gt-registry  ok\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestTownVMsStartError: a failed step makes the command exit 1 after the
// report lines, so the output says which step failed.
func TestTownVMsStartError(t *testing.T) {
	t.Parallel()
	host := newTownVMsStartHost()
	host.fail = func(c townVMsStartCall) error {
		if c.name == "docker" {
			return townVMsStartExitError{code: 1}
		}
		return nil
	}
	stdout, _, err := execTownVMsStart(townVMsStartFakeEnv(host), nil, nil)
	if err == nil {
		t.Fatal("start with a failed docker call = nil error, want an error")
	}
	if code := exitCodeForError(err); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), "docker --context colima-gt-sbx start") {
		t.Errorf("error = %v, want it to name the failed command", err)
	}
	wantPrefix := "gt-sbx  colima start -p gt-sbx --activate=false  ok\n" +
		"gt-control  colima start -p gt-control --activate=false  ok\n" +
		"gt-sbx  docker --context colima-gt-sbx start gt-sbx-control-plane gt-sbx-worker gt-sbx-worker2 gt-registry  error: "
	if !strings.HasPrefix(stdout, wantPrefix) {
		t.Errorf("stdout = %q, want the report lines before the error, starting %q", stdout, wantPrefix)
	}
}

// TestTownVMsStartInPod: in a pod the command refuses, names the host command
// set and starts no process.
func TestTownVMsStartInPod(t *testing.T) {
	t.Parallel()
	host := newTownVMsStartHost()
	env := townVMsStartFakeEnv(host)
	env.getenv = func(key string) string {
		if key == podEnv {
			return "10.0.0.1"
		}
		return ""
	}
	stdout, _, err := execTownVMsStart(env, nil, nil)
	if err == nil {
		t.Fatal("start in a pod = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), gtwrap.GTHostCommandList()) {
		t.Errorf("refusal = %v, want it to name the host command set %q", err, gtwrap.GTHostCommandList())
	}
	if strings.Contains(stdout, "colima start") {
		t.Errorf("stdout = %q, want no step lines from a run that never started", stdout)
	}
	if len(host.calls) != 0 {
		t.Errorf("start ran %d commands in a pod, want none: %v", len(host.calls), host.calls)
	}
}

// TestTownVMsStartReportWrite: a report that cannot be written is an error, not
// an exit 0, since the launchd job's log would lose the steps it ran.
func TestTownVMsStartReportWrite(t *testing.T) {
	t.Parallel()
	host := newTownVMsStartHost()
	_, _, err := execTownVMsStart(townVMsStartFakeEnv(host), errorWriter{}, nil)
	if err == nil || !strings.Contains(err.Error(), "writing the start report") {
		t.Fatalf("start with a failing writer = %v, want the report-write error", err)
	}
}

// TestTownVMsStartHome: the home the registry repair reads is the user's home
// unless --home names one, a home lookup that fails refuses before any
// command runs, and a relative --home is refused.
func TestTownVMsStartHome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		home      func() (string, error)
		args      []string
		wantErr   string
		wantStart bool
	}{
		{
			name:      "the user home reaches the registry repair",
			home:      func() (string, error) { return townVMsStartHome, nil },
			wantStart: true,
		},
		{
			name:    "a home lookup that fails refuses",
			home:    func() (string, error) { return "", errors.New("no home") },
			wantErr: "user's home directory cannot be found",
		},
		{
			name:    "a relative --home is refused",
			home:    func() (string, error) { return townVMsStartHome, nil },
			args:    []string{"--home", "relative/home"},
			wantErr: "not an absolute path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The host's registry has no known owner, so the repair runs and
			// shows which home it reads.
			host := newTownVMsStartHost()
			host.listens = false
			env := townVMsStartFakeEnv(host)
			env.home = tt.home
			_, _, err := execTownVMsStart(env, nil, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("start = %v, want an error holding %q", err, tt.wantErr)
				}
				if len(host.calls) != 0 {
					t.Errorf("start ran %d commands before refusing, want none", len(host.calls))
				}
				return
			}
			if err != nil {
				t.Fatalf("start = %v, want success", err)
			}
			want := "ssh -S " + filepath.Join(townVMsStartHome, ".colima/_lima/colima-gt-control/ssh.sock") +
				" -O cancel -L 127.0.0.1:5001:127.0.0.1:5001 -p 45678 127.0.0.1"
			if !host.ran(want) {
				t.Errorf("the repair did not run %q; ran %v", want, host.calls)
			}
		})
	}
}

// TestTownVMsStartFlag: the registered command is `gt town vms start` with the
// --home flag, so the plist bead can point it at the Mac user's home.
func TestTownVMsStartFlag(t *testing.T) {
	t.Parallel()
	start, _, err := townCmd.Find([]string{"vms", "start"})
	if err != nil {
		t.Fatalf("town vms start does not resolve: %v", err)
	}
	if start.Flags().Lookup("home") == nil {
		t.Error("town vms start has no --home flag")
	}
}
