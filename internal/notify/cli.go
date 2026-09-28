package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// CLI is the production Notifier: it runs `gt mail send`, `gt nudge` and
// `gt escalate`, exactly as the call sites that now take a Notifier used to.
//
// A zero CLI runs "gt" from PATH in the caller's working directory with the
// caller's environment. Each call runs in its own process group; its only
// deadline is ctx's, so callers bound it the way they always did.
type CLI struct {
	// Bin is the gt binary; empty means "gt" resolved on PATH at call time.
	Bin string
	// Dir is the working directory gt runs in; empty means the caller's.
	// gt resolves the town and the sender from it.
	Dir string
	// Env returns the complete environment for one call; nil inherits the
	// caller's. It is a function so each call sees the environment as it is
	// at that moment, as exec.Command's callers always did.
	Env func() []string

	run runFunc // nil means realRun
}

// command is one gt invocation.
type command struct {
	Name  string
	Args  []string
	Dir   string
	Env   []string
	Stdin string
}

// runFunc runs c and returns its combined output.
type runFunc func(ctx context.Context, c command) ([]byte, error)

// pipeGrace bounds how long a finished gt may leave its output pipe held open
// by a grandchild (a bd or git helper it spawned). Without it, Wait blocks
// until that grandchild exits, which wedged a piped tool call for hours once
// (orphaned credential helper).
const pipeGrace = 5 * time.Second

func realRun(ctx context.Context, c command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // G204: argv built by this package from typed fields
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	cmd.WaitDelay = pipeGrace
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// gt succeeded; only a grandchild kept the pipe open.
		err = nil
	}
	return out, err
}

// MailSend runs `gt mail send`.
func (c *CLI) MailSend(ctx context.Context, to, subject, body string, opts ...MailOption) error {
	if err := ValidateMail(to); err != nil {
		return err
	}
	o := ApplyMailOptions(opts)
	args := []string{"mail", "send", "-s", subject, "-m", body}
	if o.From != "" {
		args = append(args, "--from", o.From)
	}
	if o.NoNotify {
		args = append(args, "--no-notify")
	}
	args = append(args, "--", to)
	return c.exec(ctx, "", args...)
}

// Nudge runs `gt nudge` in its default (wait-idle) mode.
func (c *CLI) Nudge(ctx context.Context, target, message string) error {
	if err := ValidateNudge(target, message); err != nil {
		return err
	}
	return c.exec(ctx, "", "nudge", "--", target, message)
}

// Escalate runs `gt escalate`, passing the reason on stdin.
func (c *CLI) Escalate(ctx context.Context, e Escalation) error {
	if err := e.Validate(); err != nil {
		return err
	}
	args := []string{"escalate"}
	if e.Severity != "" {
		args = append(args, "-s", e.Severity)
	}
	if e.Source != "" {
		args = append(args, "--source", e.Source)
	}
	if e.Fingerprint != "" {
		args = append(args, "--fingerprint", e.Fingerprint)
	}
	args = append(args, "--stdin", "--", e.Description)
	return c.exec(ctx, e.Reason, args...)
}

// ClearEscalations runs `gt escalate clear` once for every key.
func (c *CLI) ClearEscalations(ctx context.Context, reason string, fingerprints ...string) error {
	if err := ValidateClear(fingerprints); err != nil {
		return err
	}
	args := []string{"escalate", "clear"}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	for _, fp := range fingerprints {
		if strings.TrimSpace(fp) != "" {
			args = append(args, "--fingerprint", fp)
		}
	}
	return c.exec(ctx, "", args...)
}

func (c *CLI) exec(ctx context.Context, stdin string, args ...string) error {
	if err := contextErr(ctx); err != nil {
		return fmt.Errorf("gt %s: %w", args[0], err)
	}
	name := c.Bin
	if name == "" {
		name = "gt"
	}
	cmd := command{Name: name, Args: args, Dir: c.Dir, Stdin: stdin}
	if c.Env != nil {
		cmd.Env = c.Env()
	}
	run := c.run
	if run == nil {
		run = realRun
	}
	out, err := run(ctx, cmd)
	if err == nil {
		return nil
	}
	if ctxErr := contextErr(ctx); ctxErr != nil {
		return fmt.Errorf("gt %s: %w (%v)", args[0], ctxErr, err)
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = "<no output>"
	}
	return fmt.Errorf("gt %s: %w (%s)", args[0], err, detail)
}

var _ Notifier = (*CLI)(nil)
