// Package deploysource refuses to install a gt built from unmerged code
// (gt-tpg0p.64). The installed gt is the one the daemon and every town
// session run, so only code merged to main may reach it; this is the Go port
// of scripts/check-deploy-source.sh, the precondition of make install-local
// (gt-o848l).
package deploysource

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// DefaultMainRef is the ref the check deploys from when DEPLOY_MAIN_REF is
// unset.
const DefaultMainRef = "origin/main"

// The switches a run reads. An empty value counts as unset, as the script's
// ${VAR:-default} and [[ -n $VAR ]] tests did.
const (
	MainRefEnv         = "DEPLOY_MAIN_REF"
	SkipUpdateCheckEnv = "SKIP_UPDATE_CHECK"
	AllowUnmergedEnv   = "ALLOW_UNMERGED"
)

// Options are the switches one run of the check reads.
type Options struct {
	// MainRef is the ref the tree must be merged into; empty is DefaultMainRef.
	MainRef string
	// SkipUpdateCheck fetches nothing, so a main that moved during a long
	// build cannot fail an install of the commit that was synced (gt-9jax).
	SkipUpdateCheck bool
	// AllowUnmerged installs the tree anyway and says so; it is for an
	// operator hot-fixing the town, not for a routine install.
	AllowUnmerged bool
}

// OptionsFromEnv reads the switches through getenv, the environment lookup.
func OptionsFromEnv(getenv func(string) string) Options {
	return Options{
		MainRef:         getenv(MainRefEnv),
		SkipUpdateCheck: getenv(SkipUpdateCheckEnv) != "",
		AllowUnmerged:   getenv(AllowUnmergedEnv) != "",
	}
}

// Git is the seam over the git questions the check asks; ExecGit answers them
// by running git, and a fake answers them in a unit test.
type Git interface {
	// Fetch updates remote/branch; the caller treats a failure as a warning.
	Fetch(remote, branch string) error
	// ShortHead is HEAD's abbreviated commit id.
	ShortHead() (string, error)
	// Resolve reports whether ref names a commit.
	Resolve(ref string) (bool, error)
	// IsAncestor reports whether commit is an ancestor of ref, or ref itself.
	IsAncestor(commit, ref string) (bool, error)
	// ModifiedTracked returns the tracked files the repository reports as
	// modified; untracked files are not among them.
	ModifiedTracked() ([]string, error)
	// CountBehind is how many commits ref has that HEAD does not.
	CountBehind(ref string) (int, error)
}

// Refusal is the verdict that the tree must not be installed. Run writes the
// reason and the two hints to its writer before returning it.
type Refusal struct {
	// Reason is the one-line cause, without the ERROR: prefix Run prints.
	Reason string
}

// Error returns the refusal's reason.
func (r *Refusal) Error() string { return r.Reason }

// Run checks the repository behind g and writes what it finds to w. It
// returns a *Refusal when the tree must not be installed, and any other error
// when the check itself could not be made: a check that could not run must
// not pass as one that did.
func Run(g Git, opts Options, w io.Writer) error {
	mainRef := opts.MainRef
	if mainRef == "" {
		mainRef = DefaultMainRef
	}
	remote, branch, found := strings.Cut(mainRef, "/")
	if !found {
		remote, branch = mainRef, mainRef
	}
	// git reads a remote or branch that begins with '-' as an option, so the
	// ref is refused before any git question is asked of it.
	if strings.HasPrefix(remote, "-") || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("%s %q has a remote or branch starting with '-', which git reads as an option", MainRefEnv, mainRef)
	}

	if !opts.SkipUpdateCheck {
		if err := g.Fetch(remote, branch); err != nil {
			fmt.Fprintf(w, "Warning: could not fetch %s; checking against the local copy\n", mainRef)
		}
	}

	head, err := g.ShortHead()
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}

	if opts.AllowUnmerged {
		fmt.Fprintf(w, "WARNING: %s=1 — installing %s without checking that it is merged to %s.\n", AllowUnmergedEnv, head, mainRef)
		fmt.Fprintf(w, "WARNING: every town session will run this UNMERGED build until the next install from main.\n")
		return nil
	}

	resolved, err := g.Resolve(mainRef)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", mainRef, err)
	}
	if !resolved {
		return refuse(w, fmt.Sprintf("cannot resolve %s, so cannot verify that %s is merged", mainRef, head))
	}

	ancestor, err := g.IsAncestor("HEAD", mainRef)
	if err != nil {
		return fmt.Errorf("check whether HEAD is merged into %s: %w", mainRef, err)
	}
	if !ancestor {
		return refuse(w, fmt.Sprintf("HEAD %s is not merged into %s", head, mainRef))
	}

	modified, err := g.ModifiedTracked()
	if err != nil {
		return fmt.Errorf("read the tracked modifications: %w", err)
	}
	for _, path := range modified {
		if !underBeads(path) {
			return refuse(w, "tracked files outside .beads/ are modified; the build would embed unmerged changes")
		}
	}

	// Behind main is a warning, so a count that cannot be read warns too: the
	// merged check above has already passed.
	behind, err := g.CountBehind(mainRef)
	if err != nil {
		fmt.Fprintf(w, "Warning: could not count the commits behind %s: %v\n", mainRef, err)
		return nil
	}
	if behind > 0 {
		fmt.Fprintf(w, "Warning: HEAD %s is %d commit(s) behind %s; installing an older main build.\n", head, behind, mainRef)
	}
	return nil
}

// underBeads reports whether a modified tracked path is .beads state, which
// bd rewrites and the build does not embed.
func underBeads(path string) bool {
	return path == ".beads" || strings.HasPrefix(path, ".beads/")
}

// refuse prints a refusal and returns it; every refusal ends the run with the
// same two hints, so a caller reads only one shape of failure.
func refuse(w io.Writer, reason string) error {
	fmt.Fprintf(w, "ERROR: %s\n", reason)
	fmt.Fprintln(w, "  Deploy merged code only: build from an up-to-date main checkout.")
	fmt.Fprintf(w, "  To install unmerged code anyway (operator hot-fix): %s=1\n", AllowUnmergedEnv)
	return &Refusal{Reason: reason}
}

// CodeCannotStart is what a Runner reports for a command that never started:
// git is not on PATH, or exec refused it.
const CodeCannotStart = -1

// CodeKilled is what a Runner reports for a command that started and was ended
// by a signal, which has no exit code to report.
const CodeKilled = -2

// Runner runs one git command in dir and reports its stdout and exit code.
type Runner func(dir string, args ...string) (out string, code int)

// ExecGit answers the check's questions by running git in the repository
// being built.
type ExecGit struct {
	// Dir is the repository; "" is the process's working directory.
	Dir string
	// Run executes git; nil runs it on PATH. A unit test supplies a runner to
	// drive the failure branches without starting a process.
	Run Runner
}

// Fetch updates the local copy of remote/branch, and returns an error when that
// update did not happen.
func (g ExecGit) Fetch(remote, branch string) error {
	_, err := g.output("fetch", remote, branch, "--quiet")
	return err
}

// ShortHead returns HEAD's abbreviated commit id, or an error when git cannot
// read it.
func (g ExecGit) ShortHead() (string, error) {
	out, err := g.output("rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Resolve reports whether ref names a commit, and returns an error when git
// cannot tell, so a failed lookup is never read as a missing ref.
func (g ExecGit) Resolve(ref string) (bool, error) {
	_, code, err := g.run("rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("git rev-parse --verify --quiet %s: exit status %d", ref, code)
}

// IsAncestor reports whether commit is an ancestor of ref or ref itself, and
// returns an error when git cannot tell, so the merged check never passes on a
// failed question.
func (g ExecGit) IsAncestor(commit, ref string) (bool, error) {
	_, code, err := g.run("merge-base", "--is-ancestor", commit, ref)
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: exit status %d", commit, ref, code)
}

// ModifiedTracked returns the paths of the tracked files git reports as modified,
// with git's C-quoting undone; untracked files are left out because only a
// tracked modification can reach the build.
func (g ExecGit) ModifiedTracked() ([]string, error) {
	out, err := g.output("status", "--porcelain", "-z", "--untracked-files=no", "--", ".")
	if err != nil {
		return nil, err
	}
	return porcelainPaths(out), nil
}

// CountBehind returns how many commits ref has that HEAD does not, or an error
// when the count cannot be read.
func (g ExecGit) CountBehind(ref string) (int, error) {
	out, err := g.output("rev-list", "--count", "HEAD.."+ref)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		// The parse error quotes the output, so the message states the defect
		// without it: that text can carry a credential.
		return 0, fmt.Errorf("git rev-list --count HEAD..%s: the count is not a number", ref)
	}
	return n, nil
}

// run executes one git command and reports what it printed and its exit code.
// The error names the command and not what git printed: captured output can
// carry a credential.
func (g ExecGit) run(args ...string) (string, int, error) {
	run := g.Run
	if run == nil {
		run = runGit
	}
	out, code := run(g.Dir, args...)
	switch code {
	case CodeCannotStart:
		return "", code, fmt.Errorf("git %s: the command did not start", strings.Join(args, " "))
	case CodeKilled:
		return "", code, fmt.Errorf("git %s: the command was killed by a signal", strings.Join(args, " "))
	}
	return out, code, nil
}

// output is run for a caller that accepts nothing but a command which exited
// 0: a non-zero exit becomes an error that names the code.
func (g ExecGit) output(args ...string) (string, error) {
	out, code, err := g.run(args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("git %s: exit status %d", strings.Join(args, " "), code)
	}
	return out, nil
}

// runGit starts git on PATH and classifies what it did, so the only path a
// unit test cannot take — starting the process — holds no decision.
func runGit(dir string, args ...string) (string, int) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), commandCode(err)
}

// commandCode is a finished git command's exit code, CodeKilled when a signal
// ended it, or CodeCannotStart when it never ran.
func commandCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		// ExitCode is -1 for a signal, which must not read as "did not start".
		return CodeKilled
	}
	return CodeCannotStart
}

// porcelainPaths returns the paths a NUL-terminated porcelain-v1 status names:
// the path after the two status characters, and for a rename or copy the
// source, which git writes as the following NUL-terminated entry.
func porcelainPaths(out string) []string {
	var paths []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		paths = append(paths, entry[3:])
		if x := entry[0]; (x == 'R' || x == 'C') && i+1 < len(entries) {
			i++
			paths = append(paths, entries[i])
		}
	}
	return paths
}
