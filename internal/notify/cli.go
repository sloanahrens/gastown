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
	if err := ValidateMail(to, subject); err != nil {
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
//
// Every method puts its flags first and its positional values after a "--",
// so a recipient, target, message or description that begins with "-"
// reaches gt as that value rather than being parsed as a flag. Flag values
// (subject, body, source, fingerprint) cannot be misread: pflag takes the
// argument after a string flag as its value whatever it starts with.
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

// ClearEscalations runs one `gt escalate clear` that names every non-blank key
// with its own --fingerprint flag.
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
	return fmt.Errorf("gt %s: %w (%s)", args[0], err, firstOutputLine(string(out)))
}

// firstOutputLine returns the first non-blank line of a failed gt's combined
// output, or "<no output>" when it printed nothing.
//
// A cobra command prints its error and then its entire usage text, and carrying
// all of it in the error put that block into every caller's log line: the
// patrol watchdog's nudge failure wrote gt nudge's usage into daemon.log on
// every cycle, for every parked rig, until gt tail was unreadable (gt-7g14a).
// The first line is the error the reader needs — "exit status 1 (Error: session
// hm-witness not found)" — and the rest is boilerplate no log wants.
func firstOutputLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return "<no output>"
}

var _ Notifier = (*CLI)(nil)
