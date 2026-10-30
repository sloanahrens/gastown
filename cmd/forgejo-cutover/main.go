// Command forgejo-cutover cuts a rig over to Forgejo-primary landing (gt-fn9e6.12).
// Until the run is ported, a valid command line exits 1 and says so, and
// scripts/forgejo-cutover.sh still performs the cutover. It exits 0 for help, 1
// when the run fails, and 2 when the command line is refused.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/forgejocut"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workspace"
)

func main() {
	os.Exit(run(os.Args[1:], liveEnv(), os.Stdout, os.Stderr))
}

// parseOptions is the seam runParse reads its command line through: Parse in the
// process, a stub in a test that drives the branch a parse failure which is not a
// usage error takes.
type parseOptions func(args []string, env forgejocut.Env) (*forgejocut.Options, error)

// run is the process minus os.Exit: a test hands it a command line, the seam set
// the parse reads through and the streams, and reads the code the process would
// have exited with.
func run(args []string, env forgejocut.Env, stdout, stderr io.Writer) int {
	return runParse(args, env, stdout, stderr, forgejocut.Parse)
}

// runParse turns what Parse returned into the script's exit codes: the help
// request is the usage text and success, a refused command line is its message and
// the usage hint, and any other error is a run that failed. A command line that
// parses reaches the run, which is not ported yet.
func runParse(args []string, env forgejocut.Env, stdout, stderr io.Writer, parse parseOptions) int {
	opts, err := parse(args, env)
	switch {
	case errors.Is(err, forgejocut.ErrHelp):
		fmt.Fprintf(stdout, "%s", usage)
		return forgejocut.ExitOK
	case err != nil:
		var refused *forgejocut.UsageError
		if !errors.As(err, &refused) {
			fmt.Fprintf(stderr, "forgejo-cutover: %v\n", err)
			return forgejocut.ExitFailure
		}
		fmt.Fprintf(stderr, "forgejo-cutover: %s\n", refused.Message)
		fmt.Fprintln(stderr, "usage: forgejo-cutover --help")
		return forgejocut.ExitUsage
	}
	if opts == nil {
		fmt.Fprintln(stderr, "forgejo-cutover: the command line parsed to no options")
		return forgejocut.ExitFailure
	}
	fmt.Fprintf(stderr, "forgejo-cutover: the cutover run is not ported yet; scripts/forgejo-cutover.sh still performs it for %s\n", opts.Rig)
	return forgejocut.ExitFailure
}

// liveEnv is the seam set the process reads: the environment, the filesystem, the
// town the working directory is in, and the git binary.
func liveEnv() forgejocut.Env {
	return forgejocut.Env{
		Getenv: os.Getenv,
		IsDir: func(path string) bool {
			fi, err := os.Stat(path)
			return err == nil && fi.IsDir()
		},
		IsFile: func(path string) bool {
			fi, err := os.Stat(path)
			return err == nil && fi.Mode().IsRegular()
		},
		Town: townFromCwd,
		Git:  bareGit{},
	}
}

// townFromCwd is the town root the working directory is in, or "" when there is
// none: the helper the other commands find their town through.
func townFromCwd() string {
	root, err := workspace.FindFromCwd()
	if err != nil {
		return ""
	}
	return root
}

// bareGit is forgejocut.Git backed by internal/git, reading the bare repository
// as its git directory.
type bareGit struct{}

// OriginURL reads the origin URL of the bare repository.
func (bareGit) OriginURL(bareRepo string) (string, error) {
	return git.NewGitWithDir(bareRepo, "").RemoteURL("origin")
}

// usage is the text -h prints. Every default it names is read from the constant
// that holds it, so the text cannot drift from the value the command uses.
var usage = fmt.Sprintf(`Usage: forgejo-cutover <rig> --repo OWNER/NAME [options]
  --town-root DIR        town root holding <rig> (default $GT_TOWN_ROOT, else
                         the town the working directory is in, else $HOME/gt)
  --repo OWNER/NAME      the rig's Forgejo repository (required)
  --github-url URL       GitHub remote imported into and connected to (default:
                         the rig bare repository's origin URL)
  --forgejo-url URL      Forgejo git URL written to the rig (default
                         WEB_URL/OWNER/NAME.git)
  --accept-diverged      skip a ref whose two sides have diverged, instead of
                         stopping the run
  --main-branch NAME     protected landing target (default: %s)
  --gate-workflow NAME   gate workflow file basename (default: %s)
  --bot-prefix P         bot login prefix, one bot per role (default: %s)
  --api-url URL          API root (default $FORGEJO_API_URL,
                         else %s)
  --web-url URL          web root, the base of the git URL (default
                         $FORGEJO_URL, else --api-url less /api/v1)
  --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
                         $FORGEJO_ADMIN_ENV, else no file is read)
  --probe PATH           probe run before the cutover, from the gastown repo
                         (default: %s)
  --gt PATH              gt binary for the daemon restart (default: %s)
  --dry-run              read and print, never write
  -h, --help             this text
`,
	forgejocut.DefaultMainBranch,
	forgejocut.DefaultGateWorkflow,
	forgejocut.DefaultBotPrefix,
	forgejocut.DefaultAPIURL,
	forgejocut.DefaultProbe,
	forgejocut.DefaultGT,
)
