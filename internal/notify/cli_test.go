package notify

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// recordedRun records every command CLI would run and answers each with the
// scripted output and error.
type recordedRun struct {
	mu    sync.Mutex
	calls []command
	out   []byte
	err   error
}

func (r *recordedRun) run(_ context.Context, c command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
	return r.out, r.err
}

func (r *recordedRun) only(t *testing.T) command {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 1 {
		t.Fatalf("ran %d commands, want 1: %+v", len(r.calls), r.calls)
	}
	return r.calls[0]
}

func newTestCLI(r *recordedRun) *CLI {
	return &CLI{Bin: "/opt/gt", Dir: "/town", run: r.run}
}

func TestCLIMailSendArgv(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	if err := newTestCLI(r).MailSend(t.Context(), "gastown/witness", "GUPP_VIOLATION: x", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	got := r.only(t)
	want := command{
		Name: "/opt/gt",
		Args: []string{"mail", "send", "-s", "GUPP_VIOLATION: x", "-m", "line one\nline two", "--", "gastown/witness"},
		Dir:  "/town",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %+v\nwant      %+v", got, want)
	}
}

func TestCLIMailSendOptions(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	if err := newTestCLI(r).MailSend(t.Context(), "overseer", "Convoy landed", "done", From("convoy/hq-cv1"), NoNotify()); err != nil {
		t.Fatal(err)
	}
	want := []string{"mail", "send", "-s", "Convoy landed", "-m", "done", "--from", "convoy/hq-cv1", "--no-notify", "--", "overseer"}
	if got := r.only(t).Args; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q\nwant   %q", got, want)
	}
}

func TestCLINudgeArgv(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	if err := newTestCLI(r).Nudge(t.Context(), "mayor/", "MERGED: mr-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"nudge", "--", "mayor/", "MERGED: mr-1"}
	if got := r.only(t).Args; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

// TestCLIEscalateArgv pins the mapping onto gt escalate's flags. The reason
// travels on stdin: it may be multi-line go-test output, which an argv flag
// would carry but bd's title would not.
func TestCLIEscalateArgv(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	err := newTestCLI(r).Escalate(t.Context(), Escalation{
		Severity:    "HIGH",
		Description: "jsonl_git_backup: spike detected",
		Reason:      "hq 1952 -> 932\nsecond line",
		Source:      "jsonl_git_backup",
		Fingerprint: "jsonl-spike",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.only(t)
	wantArgs := []string{"escalate", "-s", "HIGH", "--source", "jsonl_git_backup", "--fingerprint", "jsonl-spike", "--stdin", "--", "jsonl_git_backup: spike detected"}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Fatalf("args = %q\nwant   %q", got.Args, wantArgs)
	}
	if got.Stdin != "hq 1952 -> 932\nsecond line" {
		t.Fatalf("stdin = %q", got.Stdin)
	}
}

func TestCLIEscalateOmitsUnsetFlags(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	if err := newTestCLI(r).Escalate(t.Context(), Escalation{Description: "stuck"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"escalate", "--stdin", "--", "stuck"}
	if got := r.only(t).Args; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestCLIClearEscalationsArgv(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	if err := newTestCLI(r).ClearEscalations(t.Context(), "backup cycle clean", "k1", "", "k2"); err != nil {
		t.Fatal(err)
	}
	want := []string{"escalate", "clear", "--reason", "backup cycle clean", "--fingerprint", "k1", "--fingerprint", "k2"}
	if got := r.only(t).Args; !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestCLIDefaultsBinaryToGt(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	c := &CLI{run: r.run}
	if err := c.Nudge(t.Context(), "deacon", "hi"); err != nil {
		t.Fatal(err)
	}
	if got := r.only(t); got.Name != "gt" || got.Dir != "" || got.Env != nil {
		t.Fatalf("command = %+v, want gt with inherited dir and env", got)
	}
}

func TestCLIPassesEnv(t *testing.T) {
	t.Parallel()
	r := &recordedRun{}
	c := &CLI{run: r.run, Env: func() []string { return []string{"BD_ACTOR=daemon"} }}
	if err := c.Nudge(t.Context(), "deacon", "hi"); err != nil {
		t.Fatal(err)
	}
	if got := r.only(t).Env; !reflect.DeepEqual(got, []string{"BD_ACTOR=daemon"}) {
		t.Fatalf("env = %q", got)
	}
}

// TestCLIErrorCarriesOutput: "exit status 1" alone says nothing about why a
// send failed, so the error carries what gt printed.
func TestCLIErrorCarriesOutput(t *testing.T) {
	t.Parallel()
	exitErr := errors.New("exit status 1")
	r := &recordedRun{out: []byte("Error: session \"gt-x\" not found\n"), err: exitErr}
	err := newTestCLI(r).Nudge(t.Context(), "gt-x", "hi")
	if !errors.Is(err, exitErr) {
		t.Fatalf("err = %v, want it to wrap the exec error", err)
	}
	if !strings.Contains(err.Error(), `session "gt-x" not found`) {
		t.Fatalf("err = %v, want gt's output in it", err)
	}
}

func TestCLIErrorWithoutOutput(t *testing.T) {
	t.Parallel()
	exitErr := errors.New("signal: killed")
	r := &recordedRun{err: exitErr}
	err := newTestCLI(r).MailSend(t.Context(), "mayor/", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "<no output>") {
		t.Fatalf("err = %v, want an explicit <no output>", err)
	}
}

// TestCLIDeadlineIsReported: a caller that retries on timeout (the daemon's
// escalateAlertErr) must be able to tell a timeout from a refusal.
func TestCLIDeadlineIsReported(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	r := &recordedRun{}
	r.err = errors.New("signal: killed")
	c := &CLI{run: func(ctx context.Context, cmd command) ([]byte, error) {
		cancel() // the deadline fires while gt is running
		return r.run(ctx, cmd)
	}}
	err := c.Escalate(ctx, Escalation{Description: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
}

func TestCLIRefusesInvalidRequestsWithoutRunning(t *testing.T) {
	t.Parallel()
	cases := map[string]func(c *CLI) error{
		"mail without recipient": func(c *CLI) error { return c.MailSend(t.Context(), " ", "s", "b") },
		"mail dash subject":      func(c *CLI) error { return c.MailSend(t.Context(), "mayor/", "-s", "b") },
		"nudge without target":   func(c *CLI) error { return c.Nudge(t.Context(), "", "m") },
		"nudge without message":  func(c *CLI) error { return c.Nudge(t.Context(), "mayor", "  ") },
		"escalate without title": func(c *CLI) error { return c.Escalate(t.Context(), Escalation{Severity: "high"}) },
		"escalate multi-line":    func(c *CLI) error { return c.Escalate(t.Context(), Escalation{Description: "a\nb"}) },
		"escalate bad severity":  func(c *CLI) error { return c.Escalate(t.Context(), Escalation{Severity: "urgent", Description: "x"}) },
		"clear without keys":     func(c *CLI) error { return c.ClearEscalations(t.Context(), "r", "", " ") },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recordedRun{}
			if err := call(newTestCLI(r)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("ran %+v for an invalid request", r.calls)
			}
		})
	}
}

func TestCLIRefusesDoneContextWithoutRunning(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &recordedRun{}
	if err := newTestCLI(r).MailSend(ctx, "mayor/", "s", "b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("ran %+v on a done context", r.calls)
	}
}

// TestCLIKeepsDashLeadingValuesPositional: a recipient, target, message or
// description that begins with "-" must reach gt as a positional argument,
// not be parsed as a flag, so every positional follows a "--" separator.
func TestCLIKeepsDashLeadingValuesPositional(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		call func(c *CLI) error
		want []string
	}{
		"mail recipient": {
			call: func(c *CLI) error { return c.MailSend(t.Context(), "-to", "subject", "-body") },
			want: []string{"mail", "send", "-s", "subject", "-m", "-body", "--", "-to"},
		},
		"nudge target and message": {
			call: func(c *CLI) error { return c.Nudge(t.Context(), "-target", "--force") },
			want: []string{"nudge", "--", "-target", "--force"},
		},
		"escalation description": {
			call: func(c *CLI) error {
				return c.Escalate(t.Context(), Escalation{Severity: "low", Description: "--dry-run"})
			},
			want: []string{"escalate", "-s", "low", "--stdin", "--", "--dry-run"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recordedRun{}
			if err := tc.call(newTestCLI(r)); err != nil {
				t.Fatal(err)
			}
			if got := r.only(t).Args; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args = %q\nwant   %q", got, tc.want)
			}
		})
	}
}
