// Command shelltier runs the post-land shell tier: each shell test in the
// manifest, in order, as `bash <path>` (gt-u560q.4).
//
//	go run ./internal/shelltier/cmd/shelltier --all
//	go run ./internal/shelltier/cmd/shelltier --since-green --town <dir>
//
// Before each script it writes `tier-sweep: shell <path> ...` to stderr, and it
// writes the summary line there last. It exits with the tier's verdict: 0 when
// green, 1 when red, 2 on a usage error or when the log dir cannot be made.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/shelltier"
	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// usage is the line a usage error prints, the one a caller greps for.
const usage = "usage: shelltier [--all] [--since-green --town dir] [--root dir] [--logdir dir] [--budget duration]"

// Env is one run's inputs. main fills it with the real manifest, process and
// clock; tests fill every field with fakes, so no case runs a script or reads
// the host.
type Env struct {
	// Err is the error stream: the script lines, the summary and the usage message.
	Err io.Writer
	// Entries is the list of shell tests to run.
	Entries     []shelltier.Entry
	Exec        shelltier.Exec
	Now         func() time.Time
	WithTimeout func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
	// TempDir is the directory a default --logdir is made under: $TMPDIR.
	TempDir   string
	MkdirTemp func(dir, pattern string) (string, error)
	// Files reads and writes the shell state a --since-green run keeps.
	Files tiertrigger.Files
	// Checkout is the repository at root that --since-green reads HEAD and the
	// change since each record from.
	Checkout func(root string) shelltier.Checkout
	// Zone is the zone the daily window is read in.
	Zone func() (*time.Location, error)
}

func main() {
	os.Exit(Main(os.Args[1:], os.Stdout, Env{
		Err:         os.Stderr,
		Entries:     shelltier.Manifest(),
		Exec:        shelltier.OSExec,
		Now:         time.Now,
		WithTimeout: context.WithTimeout,
		TempDir:     os.TempDir(),
		MkdirTemp:   os.MkdirTemp,
		Files:       tiertrigger.OSFiles{},
		Checkout:    gitCheckout,
		Zone:        tiertrigger.DailyZoneLocation,
	}))
}

// gitCheckout is the repository at root, read through git.
func gitCheckout(root string) shelltier.Checkout {
	r := git.NewGit(root)
	return shelltier.Checkout{
		Head: func() (string, error) { return r.Rev("HEAD") },
		Git:  worktreeGit{diff: r.DiffNameOnly, status: r.Status},
	}
}

// worktreeGit answers the due decision's reads from a git repository, the way
// the container tier's command does: the commits since a record, and the
// checkout's uncommitted paths. The two reads are fields so a test can fail them.
type worktreeGit struct {
	diff   func(base, head string) ([]string, error)
	status func() (*git.GitStatus, error)
}

func (g worktreeGit) DiffNameOnly(base, head string) ([]string, error) {
	return g.diff(base, head)
}

func (g worktreeGit) Worktree() ([]string, error) {
	status, err := g.status()
	if err != nil {
		return nil, err
	}
	return worktreePaths(status), nil
}

// worktreePaths is every path the checkout has uncommitted changes in, tracked
// or not: an untracked test file is code the run compiles.
func worktreePaths(status *git.GitStatus) []string {
	var paths []string
	for _, group := range [][]string{status.Modified, status.Added, status.Deleted, status.Unmerged, status.Untracked} {
		paths = append(paths, group...)
	}
	return paths
}

// Main parses args, runs the entries and returns the exit code. Help goes to
// out; the script lines, the summary and every error go to env.Err.
func Main(args []string, out io.Writer, env Env) int {
	fs := flag.NewFlagSet("shelltier", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	// The flag package would print its own defaults list; the usage line is
	// printed below, to out for -h and to env.Err for a usage error.
	fs.Usage = func() {}
	// --all runs every entry and ignores the state, even with --since-green.
	all := fs.Bool("all", false, "run every manifest entry and ignore the shell state")
	sinceGreen := fs.Bool("since-green", false, "run only the entries whose triggers changed since their last green run")
	town := fs.String("town", "", "the town root whose shell state --since-green reads and writes")
	root := fs.String("root", ".", "the directory each script runs in")
	logdir := fs.String("logdir", "", "the directory for each script's log and TMPDIR (default: a new temp dir)")
	budget := fs.Duration("budget", 10*time.Minute, "the time limit for each script")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(out, usage)
			return 0
		}
		fmt.Fprintln(env.Err, usage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(env.Err, usage)
		return 2
	}
	if *sinceGreen && !*all && *town == "" {
		fmt.Fprintln(env.Err, usage)
		return 2
	}

	dir := *logdir
	if dir == "" {
		made, err := env.MkdirTemp(env.TempDir, "tier-sweep.")
		if err != nil {
			fmt.Fprintf(env.Err, "shelltier: make a log dir: %v\n", err)
			return 2
		}
		dir = made
	}

	plan := shelltier.AllEntries(env.Entries)
	if *sinceGreen && !*all {
		plan = shelltier.PlanSinceGreen(env.Entries, *town, env.Files, env.Checkout(*root), env.Zone, env.Now(), env.Err)
	}

	result := shelltier.Run(context.Background(), shelltier.Config{
		Entries:     plan.Run,
		Root:        *root,
		LogDir:      dir,
		Budget:      *budget,
		Exec:        announce(env.Err, env.Exec),
		Now:         env.Now,
		WithTimeout: env.WithTimeout,
	})
	result.Skipped = plan.Skipped
	plan.Finish(result, env.Now(), env.Err)
	fmt.Fprintln(env.Err, shelltier.Summary(result, dir))
	return int(result.Exit())
}

// announce wraps exec so each script's line is written to w before the script
// runs, the way the tier sweep names each script it starts.
func announce(w io.Writer, exec shelltier.Exec) shelltier.Exec {
	return func(ctx context.Context, c shelltier.Command) (int, error) {
		fmt.Fprintf(w, "tier-sweep: shell %s ...\n", scriptPath(c))
		return exec(ctx, c)
	}
}

// scriptPath is the script a command runs: the last element of its argv, or
// empty for a command with no argv, which the exec then refuses to run.
func scriptPath(c shelltier.Command) string {
	if len(c.Argv) == 0 {
		return ""
	}
	return c.Argv[len(c.Argv)-1]
}
