// Command forgejo-provision provisions the Forgejo bots, their tokens and the
// branch protection the landing path needs (gt-tpg0p.26). It exits 0 on
// success, 1 when the run fails and 2 when the command line is refused.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// parseOptions is the seam runParse reads its command line through: Parse in
// the process, a stub in a test that drives the branch a parse failure which is
// not a usage error takes.
type parseOptions func(args []string, getenv func(string) string) (*forgejoprov.Options, error)

// run is the process minus os.Exit: a test hands it a command line, an
// environment seam and the streams, and reads the code the process would have
// exited with.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	return runParse(args, getenv, stdout, stderr, forgejoprov.Parse)
}

// runParse turns what Parse returned into the script's exit codes: the help
// request is the flag list and success, a refused command line is its message
// and the usage hint, and any other error is a run that failed. A command line
// that parses is handed to forgejoprov.Run.
func runParse(args []string, getenv func(string) string, stdout, stderr io.Writer, parse parseOptions) int {
	opts, err := parse(args, getenv)
	switch {
	case errors.Is(err, forgejoprov.ErrHelp):
		fmt.Fprintf(stdout, "%s", usage)
		return forgejoprov.ExitOK
	case err != nil:
		var refused *forgejoprov.UsageError
		if !errors.As(err, &refused) {
			fmt.Fprintf(stderr, "forgejo-provision: %v\n", err)
			return forgejoprov.ExitFailure
		}
		fmt.Fprintf(stderr, "forgejo-provision: %s\n", refused.Message)
		fmt.Fprintln(stderr, "usage: forgejo-provision --help")
		return forgejoprov.ExitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return forgejoprov.Run(ctx, opts, forgejoprov.Env{Getenv: getenv, Log: stderr})
}

// usage is the text -h prints. It lives here rather than in the shell script's
// header, which the port deletes with the script (gt-tpg0p.52), and every
// default it names is read from the constant that holds it.
var usage = fmt.Sprintf(`Usage: forgejo-provision [options]
  --api-url URL          API root (default $FORGEJO_API_URL,
                         else %s)
  --web-url URL          web root written to the token files (default
                         $FORGEJO_URL, else --api-url less /api/v1)
  --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
                         $FORGEJO_ADMIN_ENV, else no file is read)
  --role NAME            bot role to provision; repeatable (default: %s;
                         known: %s)
  --bot-prefix P         bot username prefix (default: %s)
  --bot-email-domain D   email domain for new bots (default: %s)
  --config-dir DIR       where the role token files go (default
                         $XDG_CONFIG_HOME/gt, else $HOME/.config/gt)
  --token-name NAME      name of the minted token (default: %s)
  --repo OWNER/NAME      repository to protect; repeatable
  --landing-bot LOGIN    login on the merge whitelist (default: --bot-prefix
                         followed by landing)
  --gate-context CTX     required commit-status context; repeatable
                         (default: %s)
  --main-branch NAME     protected landing target (default: %s)
  --land-branch PATTERN  candidate branch pattern, which the run makes sure is
                         unprotected (default: %s)
  --rotate               mint a new token even where a token file exists, and
                         revoke the previous token of the same name
  --dry-run              read but never write
  -h, --help             this text
`,
	forgejoprov.DefaultAPIURL,
	strings.Join(forgejoprov.DefaultRoles(), " "),
	forgejoprov.KnownRoles(),
	forgejoprov.DefaultBotPrefix,
	forgejoprov.DefaultBotEmailDomain,
	forgejoprov.DefaultTokenName,
	quotedAndList(forgejoprov.DefaultGateContexts()),
	forgejoprov.DefaultMainBranch,
	forgejoprov.DefaultLandBranch,
)

// quotedAndList joins the default gate contexts the way the usage text names
// them: each one quoted, the last joined with "and".
func quotedAndList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = strconv.Quote(v)
	}
	return strings.Join(quoted, " and ")
}
