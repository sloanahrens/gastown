// Command forgejo-provision provisions the Forgejo bots, their tokens and the
// branch protection the landing path needs (gt-tpg0p.26). It exits 0 on
// success, 1 when the run fails and 2 when the command line is refused.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is the process minus os.Exit: a test hands it a command line, an
// environment seam and the streams, and reads the code the process would have
// exited with.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	opts, err := forgejoprov.Parse(args, getenv)
	switch {
	case errors.Is(err, forgejoprov.ErrHelp):
		fmt.Fprintf(stdout, "%s", usage)
		return forgejoprov.ExitOK
	case err != nil:
		var refused *forgejoprov.UsageError
		if errors.As(err, &refused) {
			fmt.Fprintf(stderr, "forgejo-provision: %s\n", refused.Message)
			fmt.Fprintln(stderr, "usage: forgejo-provision --help")
			return forgejoprov.ExitUsage
		}
		fmt.Fprintf(stderr, "forgejo-provision: %v\n", err)
		return forgejoprov.ExitFailure
	}
	// The run that spends these options is the series' last bead
	// (gt-tpg0p.35); until it lands, a command line that parses changes nothing.
	fmt.Fprintf(stderr, "forgejo-provision: the provisioning run is not implemented yet; nothing was changed (roles %s, api %s)\n",
		strings.Join(opts.Roles, " "), opts.APIURL)
	return forgejoprov.ExitFailure
}

// usage is the text -h prints. It lives here rather than in the shell script's
// header, which the port deletes with the script (gt-tpg0p.52).
const usage = `Usage: forgejo-provision [options]
  --api-url URL          API root (default $FORGEJO_API_URL,
                         else http://127.0.0.1:3000/api/v1)
  --web-url URL          web root written to the token files (default
                         $FORGEJO_URL, else --api-url less /api/v1)
  --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
                         $FORGEJO_ADMIN_ENV, else no file is read)
  --role NAME            bot role to provision; repeatable (default: polecat
                         landing registry; known: polecat landing registry
                         viewer)
  --bot-prefix P         bot username prefix (default: bot-)
  --bot-email-domain D   email domain for new bots (default: bots.invalid)
  --config-dir DIR       where the role token files go (default
                         $XDG_CONFIG_HOME/gt, else $HOME/.config/gt)
  --token-name NAME      name of the minted token (default: gt-provision)
  --repo OWNER/NAME      repository to protect; repeatable
  --landing-bot LOGIN    login on the merge whitelist (default: --bot-prefix
                         followed by landing)
  --gate-context CTX     required commit-status context; repeatable
                         (default: "ci / gate (push)" and "om / review")
  --main-branch NAME     protected landing target (default: main)
  --land-branch PATTERN  candidate branch pattern, which the run makes sure is
                         unprotected (default: land/*)
  --rotate               mint a new token even where a token file exists, and
                         revoke the previous token of the same name
  --dry-run              read but never write
  -h, --help             this text
`
