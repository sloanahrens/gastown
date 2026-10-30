// Command shelltier runs the post-land shell tier: each shell test in the
// manifest, in order, as `bash <path>` (gt-u560q.4).
//
//	go run ./internal/shelltier/cmd/shelltier --all
//
// Before each script it writes `tier-sweep: shell <path> ...` to stderr, and it
// writes the summary line there last. It exits with the tier's verdict: 0 when
// green, 1 when red, 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/shelltier"
)

// usage is the line a usage error prints, the one a caller greps for.
const usage = "usage: shelltier [--all] [--root dir] [--logdir dir] [--budget duration]"

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
	}))
}

// Main parses args, runs the entries and returns the exit code. Help goes to
// out; the script lines, the summary and every error go to env.Err.
func Main(args []string, out io.Writer, env Env) int {
	fs := flag.NewFlagSet("shelltier", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	// The flag package would print its own defaults list; the usage line is
	// printed below, to out for -h and to env.Err for a usage error.
	fs.Usage = func() {}
	// --all is accepted but not read yet: the trigger that picks entries is
	// gt-u560q.7, so every run covers the whole manifest until then.
	fs.Bool("all", false, "run every manifest entry")
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

	dir := *logdir
	if dir == "" {
		made, err := env.MkdirTemp(env.TempDir, "tier-sweep.")
		if err != nil {
			fmt.Fprintf(env.Err, "shelltier: make a log dir: %v\n", err)
			return 2
		}
		dir = made
	}

	result := shelltier.Run(context.Background(), shelltier.Config{
		Entries:     env.Entries,
		Root:        *root,
		LogDir:      dir,
		Budget:      *budget,
		Exec:        announce(env.Err, env.Exec),
		Now:         env.Now,
		WithTimeout: env.WithTimeout,
	})
	fmt.Fprintln(env.Err, shelltier.Summary(result, dir))
	return int(result.Exit())
}

// announce wraps exec so each script's line is written to w before the script
// runs, the way scripts/tier-sweep.sh names each script it starts.
func announce(w io.Writer, exec shelltier.Exec) shelltier.Exec {
	return func(ctx context.Context, c shelltier.Command) (int, error) {
		fmt.Fprintf(w, "tier-sweep: shell %s ...\n", c.Argv[len(c.Argv)-1])
		return exec(ctx, c)
	}
}
