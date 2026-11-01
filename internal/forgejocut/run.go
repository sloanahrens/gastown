package forgejocut

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejoresync"
)

// KeyFiles is the filesystem the promote key step reads and writes: a private
// key, its public half and the directory holding them, each with a mode. It is a
// seam of its own rather than Files because that interface is the town's config
// reader and writer (no directory creation and no chmod), and the private half's
// mode is the step's whole point. A test injects an in-memory one; the command
// runs on the operating system's.
type KeyFiles interface {
	Stat(path string) (fs.FileInfo, error)
	MkdirAll(path string, perm fs.FileMode) error
	Chmod(path string, perm fs.FileMode) error
	WriteFile(path string, data []byte, perm fs.FileMode) error
	Rename(oldpath, newpath string) error
}

// osKeyFiles is KeyFiles on the operating system.
type osKeyFiles struct{}

func (osKeyFiles) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
func (osKeyFiles) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}
func (osKeyFiles) Chmod(path string, perm fs.FileMode) error { return os.Chmod(path, perm) }
func (osKeyFiles) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (osKeyFiles) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

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
	// Keys is the filesystem the promote key and its public half go through; nil is
	// the operating system's.
	Keys KeyFiles
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
// with no token sends no request and runs no probe. The value is not kept — the
// provisioning run resolves it again through the same seam — so this run holds no
// copy of it.
func Run(ctx context.Context, opts *Options, env RunEnv) int {
	stdout, stderr := env.stdout(), env.stderr()
	if opts == nil {
		fmt.Fprintln(stderr, "forgejo-cutover: the cutover has no options")
		return ExitFailure
	}
	if env.Runner == nil {
		fmt.Fprintln(stderr, "forgejo-cutover: the exec seam is not wired: no runner for the landing queue, the probe, ssh-keygen or the daemon restart")
		return ExitFailure
	}
	if env.ProvisionEnv.Getenv == nil {
		fmt.Fprintf(stderr, "forgejo-cutover: %v\n", errProvisionEnvUnwired)
		return ExitFailure
	}
	keys := env.Keys
	if keys == nil {
		keys = osKeyFiles{}
	}
	c := &Cutover{
		Opts:         opts,
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
	if err := c.checkAdminToken(); err != nil {
		fmt.Fprintf(stderr, "forgejo-cutover: %v\n", err)
		return ExitFailure
	}
	c.logf("cutting %s over to %s (GitHub %s, promotion to %s)",
		opts.Rig, printedURL(opts.ForgejoURL), printedURL(opts.GitHubURL), printedURL(opts.PromoteTarget))

	log := func(line string) { c.logf("%s", line) }
	steps := []runStep{
		{"step 1 (refusing a landing in flight)", func() error {
			return RefuseIfLandingInFlight(env.Runner, opts, log)
		}},
		{"step 1 (the probe)", func() error {
			return ProbeGreen(env.Runner, opts, log)
		}},
		{"step 2 (the ref import)", func() error { return c.ImportRefs() }},
		{"step 3 (provisioning)", func() error { return c.Provision() }},
		{"step 4 (the promote key)", func() error {
			if err := c.ensurePromoteKey(keys); err != nil {
				return err
			}
			for _, line := range promoteKeyNotes(opts) {
				fmt.Fprintln(stdout, line)
			}
			return nil
		}},
		{"step 5 (repointing the remotes)", func() error { return c.RepointRemotes() }},
		{"step 5 (the town's git_url)", func() error { return c.SetTownGitURL() }},
		{"step 6 (the settings)", func() error { return c.WriteSettings() }},
		{"step 7 (the daemon restart)", func() error { return c.RestartDaemonIfQuiet() }},
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
			fmt.Fprintf(stderr, "forgejo-cutover: the run was cancelled before %s: %v\n", step.name, err)
			return ExitFailure
		}
		if err := step.run(); err != nil {
			fmt.Fprintf(stderr, "forgejo-cutover: %s failed: %v\n", step.name, err)
			return ExitFailure
		}
	}
	c.logf("cutover of %s complete", opts.Rig)
	return ExitOK
}

// checkAdminToken resolves the admin token the way the script's
// resolve_admin_token did, before any step runs, and discards the value. A run
// with no token is refused with the cutover's own broker command; a token that
// was found and refused keeps the provisioner's message, which names the rule and
// never the value.
func (c *Cutover) checkAdminToken() error {
	readFile := c.ProvisionEnv.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	_, err := forgejoprov.ResolveAdminToken(c.ProvisionEnv.Getenv, readFile, c.Opts.AdminTokenFile)
	if errors.Is(err, forgejoprov.ErrNoAdminToken) {
		return cutoverNoAdminTokenError(c.Opts.AdminTokenFile)
	}
	return err
}

// cutoverNoAdminTokenError is the script's refusal when no source holds a token,
// naming this command rather than the provisioner it calls.
func cutoverNoAdminTokenError(tokenFile string) error {
	standIn := ""
	if tokenFile != "" {
		standIn = fmt.Sprintf(" (or FORGEJO_ADMIN_TOKEN= in %s)", tokenFile)
	}
	return fmt.Errorf("no admin token: run this command through the auth broker (sudo -u gt-forge gt-approve run forgejo-cutover -- <its arguments>), or set FORGEJO_ADMIN_TOKEN%s for a stand-in", standIn)
}

// promotePubPath is the public half's path: the private half's name with .pub
// appended, as the script derived it.
func promotePubPath(o *Options) string { return o.PromoteKey + ".pub" }

// ensurePromoteKey is step 4's default-mode half (the script's
// ensure_promote_key): a rig with no promote key gets an ed25519 keypair, and a
// rig that has one keeps it, because a second cutover must not rotate a key
// GitHub already trusts. The private half is created mode 600 in a directory
// created mode 700, and this process never reads it, prints it or logs it; the
// public half lands beside it as <key>.pub. A dry run prints the commands and
// runs nothing.
//
// The run refuses a cutover with no Runner before this step, so ssh-keygen always
// has a seam to run through.
func (c *Cutover) ensurePromoteKey(keys KeyFiles) error {
	o := c.Opts
	key, pub := o.PromoteKey, promotePubPath(o)
	_, err := keys.Stat(key)
	switch {
	case err == nil:
		c.logf("reusing the promote key %s", key)
		return c.derivePromotePublicKey(keys, key, pub)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("could not look at the promote key %s: %w", key, err)
	}
	if o.DryRun {
		c.logf("+ mkdir -p %s (mode 700, when absent)", filepath.Dir(key))
		c.logf("+ ssh-keygen -q -t ed25519 -N '' -C promote-%s -f %s", o.Rig, key)
		c.logf("(dry run: no promote key was created)")
		return nil
	}
	if err := keys.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		return fmt.Errorf("could not make the promote key's directory %s: %w", filepath.Dir(key), err)
	}
	argv := []string{"ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "promote-" + o.Rig, "-f", key}
	if err := c.Runner.Run(argv); err != nil {
		// The child's own output reached the operator; err is its exit status.
		return fmt.Errorf("the promote key could not be created at %s: %w", key, err)
	}
	if err := keys.Chmod(key, 0o600); err != nil {
		return fmt.Errorf("could not set the promote key %s to mode 600: %w", key, err)
	}
	if err := keys.Chmod(pub, 0o644); err != nil {
		return fmt.Errorf("could not set the public half %s to mode 644: %w", pub, err)
	}
	c.logf("created the promote key %s (public half %s)", key, pub)
	return nil
}

// derivePromotePublicKey is the existing-key half of step 4: a key whose public
// half is already there is reused as it is, and a key missing its public half has
// one derived from it rather than a new keypair minted, so GitHub keeps trusting
// the key it already has. The derived text is ssh-keygen's public half only: it is
// written to the sibling and never logged.
func (c *Cutover) derivePromotePublicKey(keys KeyFiles, key, pub string) error {
	if _, err := keys.Stat(pub); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("could not look at the public half %s: %w", pub, err)
	}
	if c.Opts.DryRun {
		c.logf("+ ssh-keygen -y -f %s > %s (dry run: not run)", key, pub)
		return nil
	}
	if err := keys.Chmod(key, 0o600); err != nil {
		return fmt.Errorf("could not set the promote key %s to mode 600: %w", key, err)
	}
	out, err := c.Runner.Output([]string{"ssh-keygen", "-y", "-f", key})
	if err != nil {
		// The output is not echoed: the private key is what ssh-keygen read.
		return fmt.Errorf("could not derive the public half of %s; remove it and re-run to mint a new keypair: %w", key, err)
	}
	tmp := pub + ".tmp"
	if err := keys.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("could not write the public half %s: %w", tmp, err)
	}
	if err := keys.Chmod(tmp, 0o644); err != nil {
		return fmt.Errorf("could not set the public half %s to mode 644: %w", tmp, err)
	}
	if err := keys.Rename(tmp, pub); err != nil {
		return fmt.Errorf("could not move the public half into %s (the text is left in %s): %w", pub, tmp, err)
	}
	c.logf("derived %s from the existing promote key", pub)
	return nil
}

// promoteKeyNotes is step 4's note to the operator (the script's
// gh_promote_key_commands): the gh command that adds the key's PUBLIC half to
// GitHub with write access, so a green main verdict can advance GitHub main. Only
// the public half's path is named. Each word that a shell does not read literally
// is single-quoted, which the script did not do.
func promoteKeyNotes(o *Options) []string {
	return []string{
		"",
		"Add the promote key's PUBLIC half to GitHub as a write deploy key:",
		"",
		"  gh repo deploy-key add " + shellWord(promotePubPath(o)) +
			" --repo " + shellWord(o.GitHubSlug) +
			" --title " + shellWord("promote-"+o.Rig) +
			" --allow-write",
	}
}

// shellWord renders one word of a printed shell command: the word unchanged when
// a shell reads every character of it literally, single-quoted otherwise, with
// each embedded quote written the way a shell escapes it.
func shellWord(word string) string {
	if word != "" && !strings.ContainsFunc(word, shellUnsafe) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// shellUnsafe reports whether r is a character a shell would read as syntax inside
// an unquoted word.
func shellUnsafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("._-/:@%+,=", r)
}
