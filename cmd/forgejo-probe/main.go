// Command forgejo-probe proves a rig's gate runs green in the real Forgejo
// runner before the rig is cut over (gt-tpg0p.85).
//
// It exits 0 for -h, 1 when the run cannot start (git is not on PATH), 2 for a
// refused command line, and 70 while the probe run itself is not wired
// (gt-tpg0p.92).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprobe"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, exec.LookPath, os.Stdout, os.Stderr))
}

// parseOptions is the seam runParse reads its command line through: Parse in
// the process, a stub in a test that drives the branch a parse failure which is
// not a usage error takes.
type parseOptions func(args []string, getenv func(string) string) (*forgejoprobe.Options, error)

// lookPath is the seam the PATH check reads through: exec.LookPath in the
// process, a stub in a test that models a host without git.
type lookPath func(file string) (string, error)

// run is the process minus os.Exit: a test hands it a command line, an
// environment seam, a PATH seam and the streams, and reads the code the process
// would have exited with.
func run(args []string, getenv func(string) string, look lookPath, stdout, stderr io.Writer) int {
	return runParse(args, getenv, look, stdout, stderr, forgejoprobe.Parse)
}

// runParse turns what Parse returned into the script's exit codes: the help
// request is the flag list and success, a refused command line is its message
// and the usage hint, and any other error is a run that failed.
func runParse(args []string, getenv func(string) string, look lookPath, stdout, stderr io.Writer, parse parseOptions) int {
	_, err := parse(args, getenv)
	switch {
	case errors.Is(err, forgejoprobe.ErrHelp):
		fmt.Fprintf(stdout, "%s", usage)
		return forgejoprobe.ExitOK
	case err != nil:
		var refused *forgejoprobe.UsageError
		if !errors.As(err, &refused) {
			fmt.Fprintf(stderr, "forgejo-probe: %v\n", err)
			return forgejoprobe.ExitFailure
		}
		fmt.Fprintf(stderr, "forgejo-probe: %s\n", refused.Message)
		fmt.Fprintln(stderr, "usage: forgejo-probe --help")
		return forgejoprobe.ExitUsage
	}
	if _, err := look("git"); err != nil {
		fmt.Fprintln(stderr, "forgejo-probe: git is not on PATH")
		return forgejoprobe.ExitFailure
	}
	// The run that probes is a later bead (gt-tpg0p.92); until it lands, a
	// command line that parses must not exit 0 as though the gate had passed.
	fmt.Fprintln(stderr, notWired)
	return forgejoprobe.ExitNotWired
}

// notWired is what a command line that parses ends with until the run lands.
const notWired = "forgejo-probe: not wired yet (gt-tpg0p.92)"

// usage is the text -h prints. Every default it names is read from the constant
// that holds it, so a default the text no longer names fails the help test.
var usage = fmt.Sprintf(`Usage: forgejo-probe <rig> --repo OWNER/NAME [options]
  --api-url URL          API root (default $FORGEJO_API_URL,
                         else %s)
  --web-url URL          Forgejo web root, the base of the git URL (default
                         $FORGEJO_URL, else --api-url less /api/v1)
  --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
                         $FORGEJO_ADMIN_ENV, else no file is read)
  --repo OWNER/NAME      the rig's Forgejo repository (required)
  --town-root DIR        town root holding <rig>, whose repository the probe
                         pushes from (default $GT_TOWN_ROOT, else $HOME/gt)
  --github-url URL       repository to clone the copy from (default
                         https://github.com/OWNER/NAME.git)
  --git-url URL          Forgejo git remote to push to (default
                         --web-url/OWNER/NAME.git)
  --main-branch NAME     branch the copy is taken from (default: %s)
  --timeout SECONDS      how long to wait for the gate (default: %d)
  --poll-interval SEC    wait between status reads (default: %d)
  --dry-run              read but never write: no push, no branch deletion
  -h, --help             this text
`,
	forgejoprobe.DefaultAPIURL,
	forgejoprobe.DefaultMainBranch,
	int64(forgejoprobe.DefaultTimeout/time.Second),
	int64(forgejoprobe.DefaultPollInterval/time.Second),
)
