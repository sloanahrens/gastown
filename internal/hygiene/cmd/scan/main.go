// Command scan lists the findings the hygiene scanner reports over the tracked
// files of the work tree it runs in, one location per line:
//
//	go run ./internal/hygiene/cmd/scan
//
// A finding prints as path:line and its rule, or as path and its rule when the
// rule reports the path itself. The text of a matching line is never printed,
// so the list can be pasted anywhere.
//
// The scan exempts the two identities the town writes in its own tree: the
// forge account its repository URLs name, and its town root. A default run
// derives both, the account from the origin remote URL and the town root from
// the directory the command runs in; -forge-owner and -town-root name either
// one instead.
//
// The exit code is 1 when a finding prints, 0 when none does, and 2 when the
// scan could not look: a git answer it could not use, a tracked file or allow
// list it could not read, a -dir prefix no tracked path sits under, an origin
// remote whose account is not a usable name, or a town root the workspace
// finder failed to report. A run that could not look never exits 0.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hygiene"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Exit codes. A finding is what the command exists to report, so it exits 1; a
// run that could not look exits 2, never 0.
const (
	exitClean    = 0
	exitFindings = 1
	exitError    = 2
)

// allowPath is the committed excuse list, relative to the work tree top. The
// lister reads it from the tree it scans, so it lists exactly the findings the
// repository has not already excused.
const allowPath = "internal/hygiene/allow.json"

// gitRunner runs one git subcommand in dir and returns its standard output.
// main passes runGit; a unit test passes canned answers, since the unit tier
// starts no process.
type gitRunner func(dir string, args ...string) ([]byte, error)

// workTreeFS returns the file system rooted at the work tree top.
type workTreeFS func(top string) fs.FS

// blockedLoader returns the blocklist the scan applies. main passes the
// shipped one, hygiene.BlockedWords.
type blockedLoader func() (map[string]struct{}, error)

// townRootFinder returns the town root the command runs in, or "" when it runs
// outside a town. main passes workspace.FindFromCwd, the finder gt's commands
// use; a unit test passes a fake, since the unit tier reads no live town tree.
type townRootFinder func() (string, error)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, runGit, os.DirFS, hygiene.BlockedWords, workspace.FindFromCwd))
}

// run scans the tracked files and prints each finding's location. See the
// package comment for the exit codes.
func run(args []string, stdout, stderr io.Writer, git gitRunner, openFS workTreeFS, loadBlocked blockedLoader, findTown townRootFinder) int {
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	forgeOwner := flags.String("forge-owner", "", "the forge account whose repository URLs name the town rather than a person; its owner segment is exempted; derived from the origin remote URL when unset")
	townRoot := flags.String("town-root", "", "the town root path to exempt, matched as a whole path; derived from the working directory when unset")
	var dirs prefixList
	flags.Var(&dirs, "dir", "keep only findings under this tracked path prefix, relative to the work tree top; repeat for more")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitClean
		}
		return exitError
	}

	top, err := workTreeTop(git)
	if err != nil {
		return fail(stderr, "scan: %v", err)
	}
	paths, err := trackedFiles(git, top)
	if err != nil {
		return fail(stderr, "scan: %v", err)
	}
	// A prefix no tracked path sits under is a typo, not a clean answer: fail
	// rather than print nothing and exit 0.
	for _, dir := range dirs {
		if !anyUnder(paths, dir) {
			return fail(stderr, "scan: -dir %s: no tracked path is under it", dir)
		}
	}
	if len(dirs) > 0 {
		paths = keepUnder(paths, dirs)
	}

	// The identities the scan exempts are the town's, so a default run derives
	// them rather than requiring the caller to name them. The account is read
	// from the origin remote of the work tree at top, which is why both are
	// settled only once the top is known.
	owner, err := resolveForgeOwner(git, top, *forgeOwner)
	if err != nil {
		return fail(stderr, "scan: %v", err)
	}
	root, err := resolveTownRoot(findTown, *townRoot)
	if err != nil {
		return fail(stderr, "scan: %v", err)
	}

	blocked, err := loadBlocked()
	if err != nil {
		return fail(stderr, "scan: load blocklist: %v", err)
	}
	tree := openFS(top)
	allowData, err := fs.ReadFile(tree, allowPath)
	if err != nil {
		return fail(stderr, "scan: read allow list %s: %v", allowPath, err)
	}
	allow, err := hygiene.LoadAllow(bytes.NewReader(allowData))
	if err != nil {
		return fail(stderr, "scan: %v", err)
	}

	// The excused forbidden paths are not printed: the lister reports
	// findings, and an excused path is not one.
	findings, _, err := hygiene.Scan(tree, paths, hygiene.DefaultRules(), blocked, allow,
		hygiene.ExemptIdentities(owner, root))
	if err != nil {
		return fail(stderr, "scan: %v", err)
	}
	if len(findings) == 0 {
		return exitClean
	}
	var out strings.Builder
	for _, finding := range findings {
		out.WriteString(location(finding))
		out.WriteByte('\n')
	}
	if _, err := io.WriteString(stdout, out.String()); err != nil {
		return fail(stderr, "scan: write findings: %v", err)
	}
	return exitFindings
}

// workTreeTop names the top of the work tree the command runs in. git runs in
// the command's working directory, which may be a subdirectory of the tree or a
// linked worktree, so the top comes from git rather than from the directory.
func workTreeTop(git gitRunner) (string, error) {
	out, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", errors.New("git rev-parse --show-toplevel printed nothing")
	}
	return top, nil
}

// trackedFiles lists the files the work tree at top tracks, relative to top,
// NUL-separated so no name is split. A listing that names no file is an error,
// not a clean tree.
func trackedFiles(git gitRunner, top string) ([]string, error) {
	out, err := git(top, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", top, err)
	}
	var paths []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("git ls-files in %s listed no file", top)
	}
	return paths, nil
}

// resolveForgeOwner answers the account the scan exempts: the -forge-owner
// flag, or else the account the work tree at top is hosted under. It refuses an
// account that is not a usable name, since the exemption is spliced into every
// line it checks and a name holding a separator would excuse text that is not
// one account.
func resolveForgeOwner(git gitRunner, top, flagOwner string) (string, error) {
	owner, source := flagOwner, "-forge-owner"
	if owner == "" {
		derived, err := originOwner(git, top)
		if err != nil {
			return "", err
		}
		owner, source = derived, "the origin remote URL"
	}
	if owner == "" {
		return "", nil
	}
	if err := config.ValidForgeOwner(owner); err != nil {
		return "", fmt.Errorf("forge owner from %s: %w (pass -forge-owner)", source, err)
	}
	return owner, nil
}

// originOwner returns the account the work tree at top is hosted under: the
// first path element of its origin remote URL. A work tree with no origin
// remote names no account, which is an answer rather than a failure, because an
// exemption only ever removes a finding: exempting nothing prints every finding
// the run without the derivation would print, so an absent remote hides none. A
// git that cannot list the remotes, or cannot read the URL of the origin it
// listed, is a look that failed and is returned.
func originOwner(git gitRunner, top string) (string, error) {
	out, err := git(top, "remote")
	if err != nil {
		return "", fmt.Errorf("git remote in %s: %w", top, err)
	}
	if !listsRemote(out, "origin") {
		return "", nil
	}
	out, err = git(top, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin in %s: %w", top, err)
	}
	return remoteOwner(strings.TrimSpace(string(out))), nil
}

// listsRemote reports whether git remote's output lists name as a whole remote.
func listsRemote(out []byte, name string) bool {
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

// remoteOwner returns the first path element of a git remote URL, for the forms
// git accepts: scheme://host/path and user@host:path read their path, and a
// local path reads as itself. A URL that does not parse names no account, which
// exempts nothing and so prints every finding rather than hiding one.
func remoteOwner(remote string) string {
	path := remote
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		path = parsed.Path
	} else if colon := strings.Index(remote, ":"); colon >= 0 && !strings.Contains(remote[:colon], "/") {
		// user@host:path puts the path after the colon; a colon after a
		// separator belongs to a local path, which stays whole.
		path = remote[colon+1:]
	}
	path = strings.TrimPrefix(path, "/")
	if separator := strings.IndexByte(path, '/'); separator >= 0 {
		path = path[:separator]
	}
	return path
}

// resolveTownRoot answers the path the scan exempts: the -town-root flag, or
// else the town root the workspace finder reports for the directory the command
// runs in. Running outside a town names no root, which like an absent origin
// only adds findings to the list, so it is not a failed look.
func resolveTownRoot(find townRootFinder, flagRoot string) (string, error) {
	if flagRoot != "" {
		return flagRoot, nil
	}
	root, err := find()
	if err != nil {
		return "", fmt.Errorf("find the town root: %w", err)
	}
	return root, nil
}

// location renders one finding as its path and line and its rule, or as its
// path and rule when the rule reports the path itself. The matched line and its
// hash are never printed.
func location(finding hygiene.Finding) string {
	if finding.Line == 0 {
		return finding.Path + " " + finding.Rule
	}
	return fmt.Sprintf("%s:%d %s", finding.Path, finding.Line, finding.Rule)
}

// fail prints the reason on stderr and returns the exit code of a run that
// could not look.
func fail(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, format+"\n", args...)
	return exitError
}

// prefixList collects the -dir flags.
type prefixList []string

func (p *prefixList) String() string { return strings.Join(*p, ",") }

// Set records one prefix, without a trailing separator. An empty prefix keeps
// every path, so it is refused rather than read as the whole tree.
func (p *prefixList) Set(value string) error {
	prefix := strings.TrimSuffix(strings.TrimSpace(value), "/")
	if prefix == "" {
		return errors.New("empty prefix")
	}
	*p = append(*p, prefix)
	return nil
}

// under reports whether path is the prefix itself or sits inside it. A bare
// prefix match is not enough: internal/daemon keeps internal/daemon/x.go and
// never internal/daemonx/y.go.
func under(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// anyUnder reports whether any path is under the prefix.
func anyUnder(paths []string, prefix string) bool {
	for _, path := range paths {
		if under(path, prefix) {
			return true
		}
	}
	return false
}

// keepUnder returns the paths under any of the prefixes, in their own order.
func keepUnder(paths, prefixes []string) []string {
	var kept []string
	for _, path := range paths {
		for _, prefix := range prefixes {
			if under(path, prefix) {
				kept = append(kept, path)
				break
			}
		}
	}
	return kept
}

// runGit runs one git subcommand in dir and returns git's answer. Its error is
// exec's own and never git's captured output, which could hold a path from the
// operator's checkout; the caller names the subcommand it ran.
func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Output()
}
