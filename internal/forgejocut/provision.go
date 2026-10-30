package forgejocut

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// ProvisionFunc is one provisioning run: forgejoprov.Run, and a test's stand-in
// for it, which counts the call or forces its exit code.
type ProvisionFunc func(ctx context.Context, opts *forgejoprov.Options, env forgejoprov.Env) int

// Provision is step 3: each role's bot and token, the repository's access, the
// dashboard's viewer read grant and the protected main branch, provisioned in this
// process.
//
// The provisioning command the script ran as a child is a package call here, so the
// admin token is never an argv word and never leaves the process: the provisioner
// resolves it through the same environment seam the cutover was parsed against, and
// only the Authorization header carries its value.
//
// A dry run passes the provisioner's own --dry-run along, so it reads the server,
// reports every change it would make, and writes nothing.
func (c *Cutover) Provision() error {
	opts, err := forgejoprov.Parse(c.provisionArgs(), c.ProvisionEnv.Getenv)
	if err != nil {
		// The cutover's own parse validated every value the two commands share, so a
		// refusal here is a wiring fault rather than an operator mistake.
		return fmt.Errorf("the provisioning command line was refused: %w", err)
	}
	c.logf("provisioning %s: bots, access and branch protection", c.Opts.Repo)
	// The cutover has no context of its own; each request is bounded by the
	// provisioner's client timeout.
	if code := c.provisionRun()(context.Background(), opts, c.provisionEnv()); code != forgejoprov.ExitOK {
		// The run's own failure line already reached the log, so this message names
		// the step and its exit status and repeats neither the cause nor a value.
		return fmt.Errorf("the provisioning run of %s did not finish (exit %d)", c.Opts.Repo, code)
	}
	return nil
}

// provisionArgs is the command line step 3 hands the provisioner, in process: the
// repository, the protected main branch, the API root, the operator's admin token
// file when one was named, and --dry-run on a dry run. It is exactly the argv the
// script passed, so the provisioner's defaults resolve the same way.
func (c *Cutover) provisionArgs() []string {
	o := c.Opts
	args := []string{"--repo", o.Repo, "--main-branch", o.MainBranch, "--api-url", o.APIURL}
	if o.AdminTokenFile != "" {
		args = append(args, "--admin-token-file", o.AdminTokenFile)
	}
	if o.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

// provisionRun is the provisioning run Provision calls: the injected one, else
// forgejoprov.Run in this process.
func (c *Cutover) provisionRun() ProvisionFunc {
	if c.ProvisionRun != nil {
		return c.ProvisionRun
	}
	return forgejoprov.Run
}

// provisionEnv is the world the provisioning run reads: the seams the cutover was
// given, with its own line logger attached, so the provisioner's progress reaches
// the operator the way every other step's lines do.
func (c *Cutover) provisionEnv() forgejoprov.Env {
	env := c.ProvisionEnv
	env.Log = provisionLog{line: func(text string) { c.logf("%s", text) }}
	return env
}

// provisionLog is the run's line logger as the io.Writer the provisioner logs to.
// The provisioner writes whole lines, each ending in "\n".
type provisionLog struct {
	line func(string)
}

func (w provisionLog) Write(p []byte) (int, error) {
	text := strings.TrimSuffix(string(p), "\n")
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			w.line(line)
		}
	}
	return len(p), nil
}
