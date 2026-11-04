package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

// claudeExecSeams are the operations claude-exec reaches the host through. The
// real ones are wired in daemonClaudeExecCmd; a test passes fakes, so no file is
// read and no process starts.
type claudeExecSeams struct {
	getenv   func(name string) string
	environ  func() []string
	readFile func(path string) (string, error)
	run      func(argv, env []string) (int, error)
	exit     func(code int) // ends the process with claude's status; os.Exit in production
}

// claudeExec starts claude with args and the om token read from tokenFile in
// its environment only, and returns claude's exit status. The token is read
// here because om withholds it from the reviewer's environment by design, so
// the command must name the credential itself. Every check runs before claude
// starts, so a refusal starts nothing and never names the token.
func claudeExec(s claudeExecSeams, tokenFile string, args []string) (int, error) {
	if tokenFile == "" {
		return 0, fmt.Errorf("--%s is required; claude was not started", config.OMTokenFileFlag)
	}
	// The token file and the API-credential refusal are the container
	// entrypoint's own (gt-qpeei.36), so the two commands read one token the
	// same way instead of holding copies that can drift.
	e := containerEntrypoint{getenv: s.getenv, readFile: s.readFile, omTokenFile: tokenFile}
	if err := e.refuseAPICredentials(); err != nil {
		return 0, err
	}
	env, err := e.omTokenEnv()
	if err != nil {
		return 0, err
	}
	argv := append([]string{"claude"}, args...)
	code, err := s.run(argv, mergeEnv(s.environ(), env))
	if err != nil {
		return 0, fmt.Errorf("start claude: %w", err)
	}
	return code, nil
}

// claudeChildStatus is the answer for the error a child's Run returned: a child
// that ran and exited non-zero gives its status and no error, and any other
// failure gives no status and the error. A test passes an error, so no process
// starts.
func claudeChildStatus(runErr error) (int, error) {
	var exited interface{ ExitCode() int } // exec.ExitError, and a test's stand-in
	if errors.As(runErr, &exited) {
		return exited.ExitCode(), nil
	}
	return 0, runErr
}

// runClaudeChild runs argv with env and the process's standard streams, and
// returns the child's exit status. A child that exits non-zero is not an error
// here: its status is the answer.
func runClaudeChild(argv, env []string) (int, error) {
	c := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv[0] is the fixed name claude; the rest are the arguments the operator wrote after --
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return claudeChildStatus(c.Run())
}

// newDaemonClaudeExecCmd builds the command over seams, so a test drives the
// cobra parse and the call into claudeExec with fakes.
func newDaemonClaudeExecCmd(seams claudeExecSeams) *cobra.Command {
	var tokenFile string
	cmd := &cobra.Command{
		Use:   "claude-exec --" + config.OMTokenFileFlag + " <path> -- <claude args...>",
		Short: "Run claude with the om subscription token from a file (internal)",
		Long: `Run claude with the om subscription token loaded from a file.

The om reviewer backend runs this command in place of claude. It reads the
file, removes its space, tab, carriage return and line feed characters, and
starts claude with the result as CLAUDE_CODE_OAUTH_TOKEN in claude's
environment only. The arguments after -- go to claude unchanged; claude's
stdin, stdout and stderr are this process's, and its exit status is this
command's.

It refuses when the file is unreadable or empty, and when ANTHROPIC_API_KEY or
ANTHROPIC_AUTH_TOKEN is set, since either outranks the token and bills API
rates. Every check runs before claude starts.`,
		Hidden:       true,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			code, err := claudeExec(seams, tokenFile, args)
			if err != nil {
				return err
			}
			if code != 0 {
				seams.exit(code)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&tokenFile, config.OMTokenFileFlag, "",
		"Read the om subscription token from this file")
	return cmd
}

// daemonClaudeExecCmd is gt daemon claude-exec, the om reviewer backend's
// command; the real seams are the host's.
var daemonClaudeExecCmd = newDaemonClaudeExecCmd(claudeExecSeams{
	getenv:   os.Getenv,
	environ:  os.Environ,
	readFile: readTextFile,
	run:      runClaudeChild,
	exit:     os.Exit,
})

func init() {
	daemonCmd.AddCommand(daemonClaudeExecCmd)
}
