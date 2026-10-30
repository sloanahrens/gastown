// Command forgejo-resync makes a Forgejo repository's refs match GitHub's and
// restores main's branch protection (gt-fn9e6.29). It exits 0 for help and for a
// resync that converged, 1 when the run fails, and 2 when the command line is
// refused.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/steveyegge/gastown/internal/forgejoresync"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.ReadFile, os.Stdout, os.Stderr))
}

// parseOptions is the seam runParse reads its command line through: Parse in
// the process, a stub in a test that drives the branch a parse failure which is
// not a usage error takes.
type parseOptions func(args []string, getenv func(string) string) (*forgejoresync.Options, error)

// run is the process minus os.Exit: a test hands it a command line, the
// environment and file seams and the streams, and reads the code the process
// would have exited with.
func run(args []string, getenv func(string) string, readFile func(string) ([]byte, error), stdout, stderr io.Writer) int {
	return runParse(args, getenv, readFile, stdout, stderr, forgejoresync.Parse)
}

// notifySignals is signal.NotifyContext behind a seam, so a test hands the run one
// that records the signals caught instead of installing a process-wide handler.
type notifySignals func(ctx context.Context, signals ...os.Signal) (context.Context, context.CancelFunc)

// runParse turns what Parse returned into the script's exit codes: the help
// request is the usage text and success, a refused command line is its message
// and the usage hint, and any other error is a run that failed. A command line
// that parses is run, and its exit code is the run's.
func runParse(args []string, getenv func(string) string, readFile func(string) ([]byte, error), stdout, stderr io.Writer, parse parseOptions) int {
	return runNotify(args, getenv, readFile, stdout, stderr, parse, signal.NotifyContext)
}

// runNotify is runParse with the signal source injected.
func runNotify(args []string, getenv func(string) string, readFile func(string) ([]byte, error), stdout, stderr io.Writer, parse parseOptions, notify notifySignals) int {
	opts, err := parse(args, getenv)
	switch {
	case errors.Is(err, forgejoresync.ErrHelp):
		fmt.Fprintf(stdout, "%s", usage)
		return forgejoresync.ExitOK
	case err != nil:
		var refused *forgejoresync.UsageError
		if !errors.As(err, &refused) {
			fmt.Fprintf(stderr, "forgejo-resync: %v\n", err)
			return forgejoresync.ExitFailure
		}
		fmt.Fprintf(stderr, "forgejo-resync: %s\n", refused.Message)
		fmt.Fprintln(stderr, "usage: forgejo-resync --help")
		return forgejoresync.ExitUsage
	}
	if opts == nil {
		fmt.Fprintln(stderr, "forgejo-resync: the command line parsed to no options")
		return forgejoresync.ExitFailure
	}
	// The script's EXIT trap ran the restore for an interrupt, a termination and a
	// hangup alike, so all three cancel the run here: the git calls stop at their
	// next cancel, and the protection restore still runs.
	ctx, stop := notify(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// The first signal ends the run and starts the restore, which can take up to
	// RestoreTimeout. Releasing the handler then means a second signal during the
	// restore has the default effect (the process dies) instead of being swallowed
	// until the run returns.
	context.AfterFunc(ctx, stop)
	resync := &forgejoresync.Resync{
		Opts:     opts,
		Context:  ctx,
		Getenv:   getenv,
		ReadFile: readFile,
		Log:      func(line string) { fmt.Fprintf(stderr, "forgejo-resync: %s\n", line) },
	}
	return resync.Run()
}

// usage is the text -h prints. Every default it names is read from the constant
// that holds it, so the text cannot drift from the value the command uses.
var usage = fmt.Sprintf(`Usage: forgejo-resync [options] OWNER/NAME
  --api-url URL          API root (default $FORGEJO_API_URL,
                         else %s)
  --web-url URL          web root the git remote is derived from (default
                         $FORGEJO_URL, else --api-url less /api/v1)
  --github-url URL       git remote to import from (default
                         https://github.com/OWNER/NAME.git)
  --forgejo-url URL      git remote to push to (default
                         WEB_URL/OWNER/NAME.git)
  --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
                         $FORGEJO_ADMIN_ENV, else no file is read)
  --main-branch NAME     protected landing target to lift and restore
                         (default: %s)
  --backup-dir DIR       where the lifted rule's dated .bak- copy goes
                         (default $FORGEJO_BACKUP_DIR, else
                         $HOME/forgejo/backups)
  --dry-run              read and print, never write
  -h, --help             this text
`,
	forgejoresync.DefaultAPIURL,
	forgejoresync.DefaultMainBranch,
)
