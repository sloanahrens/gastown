// Command scan lists the findings the hygiene scanner reports over the tracked
// files of the work tree it runs in, one location per line:
//
//	go run ./internal/hygiene/cmd/scan
//
// A finding prints as path:line and its rule, or as path and its rule when the
// rule reports the path itself. The text of a matching line is never printed,
// so the list can be pasted anywhere.
//
// The exit code is 1 when a finding prints, 0 when none does, and 2 when the
// scan could not look: a git answer it could not use, a tracked file or allow
// list it could not read, or a -dir prefix no tracked path sits under. A run
// that could not look never exits 0.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/hygiene"
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, runGit, os.DirFS, hygiene.BlockedWords))
}

// run scans the tracked files and prints each finding's location. See the
// package comment for the exit codes.
func run(args []string, stdout, stderr io.Writer, git gitRunner, openFS workTreeFS, loadBlocked blockedLoader) int {
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	forgeOwner := flags.String("forge-owner", "", "the forge account whose repository URLs name the town rather than a person; its owner segment is exempted")
	townRoot := flags.String("town-root", "", "the town root path to exempt, matched as a whole path")
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
		hygiene.ExemptIdentities(*forgeOwner, *townRoot))
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
