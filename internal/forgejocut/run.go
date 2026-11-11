package forgejocut

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejoresync"
)

// RunEnv is the world one whole cutover reads and acts on: the seams every step
// goes through, and the two streams the script wrote its progress and its notes
// to. A nil seam takes its in-process default, except Runner and
// ProvisionEnv.Getenv, which the run refuses without: a cutover that cannot read
// its landing queue, start the probe or resolve the admin token must fail rather
// than skip those steps.
type RunEnv struct {
	// Git runs the cutover's git calls; nil runs the git binary.
	Git forgejoresync.Runner
	// Files is the filesystem the config edits and the repoint go through; nil is
	// the operating system's.
	Files Files
	// Now stamps the backup and replace names; nil is time.Now.
	Now func() time.Time
	// Runner runs the children that are not git: the landing-queue read, the probe,
	// ssh-keygen and the daemon restart. A test injects a fake.
	Runner Runner
	// MkdirTemp and RemoveAll make and remove the import's scratch directory; nil is
	// os.MkdirTemp and os.RemoveAll.
	MkdirTemp func(dir, pattern string) (string, error)
	RemoveAll func(path string) error
	// ProvisionRun is step 3's provisioning run; nil runs forgejoprov.Run in this
	// process. ProvisionEnv is what that run reads.
	ProvisionRun ProvisionFunc
	ProvisionEnv forgejoprov.Env
	// Stdout takes the operator notes, Stderr the progress and the failure; nil is
	// the process's own stream.
	Stdout io.Writer
	Stderr io.Writer
}

// stdout is the stream the operator notes go to.
func (e RunEnv) stdout() io.Writer {
	if e.Stdout != nil {
		return e.Stdout
	}
	return os.Stdout
}

// stderr is the stream the progress and the failure go to.
func (e RunEnv) stderr() io.Writer {
	if e.Stderr != nil {
		return e.Stderr
	}
	return os.Stderr
}

// runStep is one step of the cutover: the name its failure reports and what it
// does. Every step reads before it writes, so a second run over converged state
// changes nothing.
type runStep struct {
	name string
	run  func() error
}

// Run performs one cutover in the shell script's order and returns the process
// exit code: ExitOK for a run that finished (a dry run included), ExitFailure for
// a run that could not start or a step that failed. A command line the validation
// refused never reaches Run; the command exits ExitUsage for it.
//
// The operator notes go to env.Stdout and every progress and failure line to
// env.Stderr, as the script's note and log did. The admin token is resolved here,
// before the first step, exactly as the script resolved it before step 1: a run
// with no token sends no request and runs no probe. The value is kept only to
// hand to the probe when no token file was named — the probe resolves it from its
// environment — and the provisioning run resolves it again through the same seam.
func Run(ctx context.Context, opts *Options, env RunEnv) int {
	stdout, stderr := env.stdout(), env.stderr()
	if opts == nil {
		fmt.Fprintln(stderr, "forgejo-cutover: the cutover has no options")
		return ExitFailure
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if env.Runner == nil {
		fmt.Fprintln(stderr, "forgejo-cutover: the exec seam is not wired: no runner for the landing queue, the probe, ssh-keygen or the daemon restart")
		return ExitFailure
	}
	if env.ProvisionEnv.Getenv == nil {
		fmt.Fprintf(stderr, "forgejo-cutover: %v\n", errProvisionEnvUnwired)
		return ExitFailure
	}
	c := &Cutover{
		Opts:         opts,
		Ctx:          ctx,
		Git:          env.Git,
		Files:        env.Files,
		Now:          env.Now,
		Runner:       env.Runner,
		MkdirTemp:    env.MkdirTemp,
		RemoveAll:    env.RemoveAll,
		ProvisionRun: env.ProvisionRun,
		ProvisionEnv: env.ProvisionEnv,
		Log:          func(line string) { fmt.Fprintf(stderr, "forgejo-cutover: %s\n", line) },
	}
	token, err := c.checkAdminToken()
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-cutover: %v\n", err)
		return ExitFailure
	}
	c.logf("cutting %s over to %s (GitHub %s, promotion to %s)",
		opts.Rig, printedURL(opts.ForgejoURL), printedURL(opts.GitHubURL), printedURL(opts.PromoteTarget))

	log := func(line string) { c.logf("%s", line) }
	// restart is what step 7 did: the summary line reads it, so a restart that
	// was skipped for a landing in flight does not read as one that ran.
	restart := RestartNone
	steps := []runStep{
		{"step 1 (refusing a landing in flight)", func() error {
			return RefuseIfLandingInFlight(env.Runner, opts, log)
		}},
		{"step 1 (the probe)", func() error {
			return ProbeGreen(env.Runner, opts, token, log)
		}},
		{"step 2 (the ref import)", func() error { return c.ImportRefs() }},
		{"step 3 (provisioning)", func() error { return c.Provision() }},
		{"step 4 (the promote key)", func() error {
			if err := c.EnsurePromoteKey(); err != nil {
				return err
			}
			for _, line := range c.GhPromoteKeyCommands() {
				fmt.Fprintln(stdout, line)
			}
			return nil
		}},
		{"step 5 (repointing the remotes)", func() error { return c.RepointRemotes() }},
		{"step 5 (the town's git_url)", func() error { return c.SetTownGitURL() }},
		{"step 6 (the settings)", func() error { return c.WriteSettings() }},
		{"step 7 (the daemon restart)", func() error {
			outcome, err := c.RestartDaemonIfQuiet()
			restart = outcome
			return err
		}},
		{"step 8 (the operator steps)", func() error {
			fmt.Fprintln(stdout)
			for _, line := range c.OperatorSteps() {
				fmt.Fprintln(stdout, line)
			}
			return nil
		}},
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(stderr, "forgejo-cutover: the run was canceled before %s: %v\n", step.name, err)
			return ExitFailure
		}
		if err := step.run(); err != nil {
			fmt.Fprintf(stderr, "forgejo-cutover: %s failed: %v\n", step.name, err)
			return ExitFailure
		}
	}
	c.logf("cutover of %s complete; %s", opts.Rig, restart)
	return ExitOK
}

// checkAdminToken resolves the admin token the way the script's
// resolve_admin_token did, before any step runs. The value goes back to the run
// for one purpose: the probe, when no token file was named, resolves it from the
// environment the run hands that one child. A run with no token is refused with
// the cutover's own broker rule; a token that was found and refused keeps the
// provisioner's message, which names the rule and never the value.
func (c *Cutover) checkAdminToken() (forgejoprov.Secret, error) {
	readFile := c.ProvisionEnv.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	token, err := forgejoprov.ResolveAdminToken(c.ProvisionEnv.Getenv, readFile, c.Opts.AdminTokenFile)
	if errors.Is(err, forgejoprov.ErrNoAdminToken) {
		return forgejoprov.Secret{}, cutoverNoAdminTokenError(c.Opts.AdminTokenFile)
	}
	return token, err
}

// cutoverNoAdminTokenError is the script's refusal when no source holds a token,
// naming this command's broker rule rather than the provisioner's.
func cutoverNoAdminTokenError(tokenFile string) error {
	standIn := ""
	if tokenFile != "" {
		standIn = fmt.Sprintf(" (or FORGEJO_ADMIN_TOKEN= in %s)", tokenFile)
	}
	return fmt.Errorf("no admin token: run this command through the auth broker (sudo -u gt-forge gt-approve run %s -- <its arguments>), or set FORGEJO_ADMIN_TOKEN%s for a stand-in", forgejoprov.CutoverRule, standIn)
}
