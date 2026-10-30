package forgejoprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/land"
)

// bodyTailBytes bounds the response body an API error message carries.
const bodyTailBytes = 300

// Streams are the probe's two outputs. Progress and verdict lines go to Stderr;
// only the failing job's log tail goes to Stdout, so it can be paged or captured
// without the progress around it.
type Streams struct {
	Stdout io.Writer
	Stderr io.Writer
}

// Deps are the seams a run reaches the world through. SystemDeps puts the
// operating system's behind them; a test puts fakes.
type Deps struct {
	Git       Git
	LookPath  func(file string) (string, error)
	ReadFile  func(name string) ([]byte, error)
	HTTP      *http.Client                           // nil means the default client
	Clock     Clock                                  // the wait's and the record's time
	FS        FS                                     // where the green record is written
	Stat      func(name string) (fs.FileInfo, error) // nil means os.Stat
	MkdirTemp func(dir, pattern string) (string, error)
	RemoveAll func(path string) error
}

// API is the part of the Forgejo API a run calls: the repository read, the
// status, run and log reads the wait and the tail make, and the branch deletion.
// forgejo.Client satisfies it.
type API interface {
	Statuses
	Repository(ctx context.Context, owner, repo string) (*forgejo.Repository, error)
	DeleteBranch(ctx context.Context, owner, repo, branch string) error
}

var _ API = (*forgejo.Client)(nil)

// Run is the probe's whole run and returns the process's exit code. The order is
// the contract: every refusal happens before anything is pushed, and every exit
// once the work has begun goes through Finish, so no stale record survives it.
func Run(ctx context.Context, args []string, getenv func(string) string, s Streams, d Deps) int {
	opts, err := Parse(args, getenv)
	switch {
	case errors.Is(err, ErrHelp):
		fmt.Fprintf(s.Stdout, "%s", Usage)
		return ExitOK
	case err != nil:
		var refused *UsageError
		if !errors.As(err, &refused) {
			fmt.Fprintf(s.Stderr, "forgejo-probe: %v\n", err)
			return ExitFailure
		}
		fmt.Fprintf(s.Stderr, "forgejo-probe: %s\n", refused.Message)
		fmt.Fprintln(s.Stderr, "usage: forgejo-probe --help")
		return ExitUsage
	}
	if _, err := d.LookPath("git"); err != nil {
		fmt.Fprintln(s.Stderr, "forgejo-probe: git is not on PATH")
		return ExitFailure
	}
	p := &probeRun{ctx: ctx, opts: opts, getenv: getenv, s: s, d: d}
	return p.run()
}

// SystemDeps is the production Deps: the process's git, PATH, files, clock and
// directories.
func SystemDeps() Deps {
	return Deps{
		Git:       NewGit(),
		LookPath:  exec.LookPath,
		ReadFile:  os.ReadFile,
		Clock:     systemClock{},
		FS:        osFS{},
		Stat:      os.Stat,
		MkdirTemp: os.MkdirTemp,
		RemoveAll: os.RemoveAll,
	}
}

// probeRun is one run: its options, its seams, the state it builds up, and the
// Finisher its exit is taken through.
type probeRun struct {
	ctx    context.Context
	opts   *Options
	getenv func(string) string
	s      Streams
	d      Deps

	api           API
	work          string // the probe's temporary directory
	copyDir       string // the full clone of the rig's GitHub repository
	gate          land.GateWorkflow
	sha           string // the commit the probe pushes
	pusher        *Pusher
	branchCreated bool
}

// run takes the run from the admin token to the exit.
func (p *probeRun) run() int {
	tok, err := ResolveToken(p.getenv, p.d.ReadFile, p.opts.AdminTokenFile)
	if err != nil {
		return p.fail(err)
	}
	client, err := NewClient(p.opts.APIURL, tok, p.d.HTTP)
	if err != nil {
		return p.fail(err)
	}
	p.api = client
	if err := p.readRepository(); err != nil {
		return p.fail(err)
	}
	if err := p.cloneAndGate(); err != nil {
		return p.fail(err)
	}
	rigRepo, err := p.rigRepository()
	if err != nil {
		return p.fail(err)
	}
	if p.opts.DryRun {
		return p.dryRun(rigRepo)
	}
	return p.pushAndWait()
}

// readRepository asks the instance for the repository. A 404 names the two causes
// it can have, since a token that cannot see a repository looks the same as a
// missing one; any other status is unexpected; a transport failure is the error.
func (p *probeRun) readRepository() error {
	_, err := p.api.Repository(p.ctx, p.opts.Owner, p.opts.Name)
	var api *forgejo.APIError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &api) && api.StatusCode == http.StatusNotFound:
		return fmt.Errorf("GET /repos/%s returned 404: the repository is not on %s, or the admin token cannot see it%s",
			p.opts.Repo, p.opts.APIURL, bodyTail(api.Body))
	case errors.As(err, &api):
		return fmt.Errorf("GET /repos/%s: unexpected HTTP %d%s", p.opts.Repo, api.StatusCode, bodyTail(api.Body))
	default:
		return err
	}
}

// bodyTail renders the start of an error body for a message, with its newlines as
// spaces, after an em dash. An empty body adds nothing.
func bodyTail(body string) string {
	if body == "" {
		return ""
	}
	if len(body) > bodyTailBytes {
		body = body[:bodyTailBytes]
	}
	return " — " + strings.ReplaceAll(body, "\n", " ")
}

// cloneAndGate makes the work directory, clones the rig's GitHub repository in
// full and reads the gate's identity and commit from the copy. Nothing is pushed
// before it: a copy without the gate file is refused here.
func (p *probeRun) cloneAndGate() error {
	work, err := p.d.MkdirTemp("", "forgejo-probe.")
	if err != nil {
		return fmt.Errorf("could not create the probe's work directory: %w", err)
	}
	p.work = work
	p.copyDir = filepath.Join(work, "copy")
	if err := CloneCopy(p.d.Git, p.opts.GitHubURL, p.copyDir); err != nil {
		return err
	}
	gate, sha, err := GateIdentity(p.d.Git, p.copyDir, p.opts.MainBranch)
	if err != nil {
		return err
	}
	p.gate, p.sha = gate, sha
	return nil
}

// rigRepository reports whether the rig's own repository is present, which decides
// whether the push rides a worktree of it (and so runs the rig's hooks).
func (p *probeRun) rigRepository() (bool, error) {
	stat := p.d.Stat
	if stat == nil {
		stat = os.Stat
	}
	bare := p.opts.Bare()
	_, err := stat(bare)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("could not stat the rig repository %s: %w", bare, err)
	}
}

// dryRun writes the lines a real run would act on, and does none of those acts.
// It removes the work directory it made, and leaves any record where it was.
func (p *probeRun) dryRun(rigRepo bool) int {
	plan := DryRunPlan{
		Main:    p.opts.MainBranch,
		SHA:     p.sha,
		GitURL:  p.opts.GitURL,
		Branch:  p.opts.Branch(),
		Bare:    p.opts.Bare(),
		RigRepo: rigRepo,
		Repo:    p.opts.Repo,
		Context: p.gate.Context(),
		Timeout: p.opts.Timeout,
	}
	for _, line := range DryRunLines(plan) {
		p.Logf("%s", line)
	}
	removeWorkDir(p)
	return ExitOK
}

// pushAndWait pushes the candidate, waits for the gate's verdict on it and ends
// the run through Finish. A red, timed-out or unreadable verdict prints the failing
// job's log tail first, so the reader has the reason before the exit.
func (p *probeRun) pushAndWait() int {
	opts := p.opts
	p.pusher = &Pusher{
		Git:        p.d.Git,
		Bare:       opts.Bare(),
		Copy:       p.copyDir,
		Work:       p.work,
		GitURL:     opts.GitURL,
		MainBranch: opts.MainBranch,
		Branch:     opts.Branch(),
		Stat:       p.d.Stat,
		Log:        func(msg string) { p.Logf("%s", msg) },
	}
	mode, err := p.pusher.Push(p.sha)
	if err != nil {
		return p.fail(err)
	}
	p.branchCreated = true
	p.Logf("pushed %s (%s) to %s as %s; waiting up to %ds for %s",
		opts.MainBranch, p.sha, opts.Repo, opts.Branch(), int64(opts.Timeout/time.Second), p.gate.Context())

	verdict := Wait(p.ctx, p.api, p.d.Clock, opts.Owner, opts.Name, p.sha, p.gate.Context(), opts.Timeout, opts.PollInterval)
	green := verdict.Outcome == Green
	p.Logf("%s", verdict.Line(opts.Rig, p.gate.Context(), p.sha, opts.Timeout))
	rc := ExitOK
	if !green {
		rc = ExitFailure
		PrintTail(p.ctx, p.api, p.s.Stdout, p.Logf, opts.Owner, opts.Name, p.sha, p.gate.Job)
	}
	return Finish(p, green, mode, rc)
}

// fail reports a run that cannot go on and ends it through Finish, so the probe
// branch, the worktree and any stale record are dealt with as on every other exit.
func (p *probeRun) fail(err error) int {
	p.Logf("%v", err)
	return Finish(p, false, PushedFromClone, ExitFailure)
}

// Logf writes one line of progress to stderr, prefixed with the command's name.
func (p *probeRun) Logf(format string, args ...any) {
	fmt.Fprintf(p.s.Stderr, "forgejo-probe: %s\n", fmt.Sprintf(format, args...))
}

// The Finisher methods: what the run's exit touches.

// Cleanup removes the worktree the push added from the rig's repository, if any.
func (p *probeRun) Cleanup() error {
	if p.pusher == nil {
		return nil
	}
	return p.pusher.Cleanup()
}

// BranchCreated reports whether the push created the probe branch.
func (p *probeRun) BranchCreated() bool { return p.branchCreated }

// Branch is the probe branch, and Repo the Forgejo copy it lives on.
func (p *probeRun) Branch() string { return p.opts.Branch() }
func (p *probeRun) Repo() string   { return p.opts.Repo }

// DeleteBranch deletes the probe branch on the Forgejo copy.
func (p *probeRun) DeleteBranch() error {
	return p.api.DeleteBranch(p.ctx, p.opts.Owner, p.opts.Name, p.opts.Branch())
}

// Bare is the rig's own repository, named when the hooks did not run.
func (p *probeRun) Bare() string { return p.opts.Bare() }

// Record writes the green record of the probed commit and returns its path.
func (p *probeRun) Record() (string, error) {
	return WriteRecord(p.d.FS, p.d.Clock, p.opts.ConfigDir, p.opts.Rig, p.opts.Repo, p.sha, p.gate.Context())
}

// RecordPath is the path of the rig's record, named when it cannot be removed.
func (p *probeRun) RecordPath() string { return p.opts.Record() }

// RemoveRecord removes the rig's record an earlier run left.
func (p *probeRun) RemoveRecord() error {
	return RemoveRecord(p.d.FS, p.opts.ConfigDir, p.opts.Rig)
}

// RemoveWorkDir removes the probe's temporary directory, if one was made.
func (p *probeRun) RemoveWorkDir() error {
	if p.work == "" {
		return nil
	}
	return p.d.RemoveAll(p.work)
}

// WorkDir is the probe's temporary directory, named when it cannot be removed.
func (p *probeRun) WorkDir() string { return p.work }
