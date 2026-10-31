package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// gt git-credential-forgejo is a git credential helper (gitcredentials(7)):
// git runs it as `gt git-credential-forgejo --host H --token-file F get` and
// reads username= and password= lines from its output. It exists so a daemon in
// a container can push to a Forgejo with the landing bot's token, as the host's
// shell helper does for the host daemon (gt-dhjz6).
//
// It answers only `get`, only for http or https and only for a host on the
// --host allowlist, and reads the token only from a mode-600 file. The token is
// never in argv, gitconfig, a log or an error: argv carries the file's path.

// gitCredentialForgejoFlags are the values the command's flags parsed.
type gitCredentialForgejoFlags struct {
	hosts     []string // --host, repeatable: host:port allowlist
	tokenFile string   // --token-file: absolute path of the bot's env file
}

// newGitCredentialForgejoCmd builds the command reading stdin and writing
// stdout through in and out, so a test drives the real flags and the real
// protocol without a process.
func newGitCredentialForgejoCmd(in io.Reader, out io.Writer, readCredential func(path string) (user, token string, err error)) *cobra.Command {
	var flags gitCredentialForgejoFlags
	cmd := &cobra.Command{
		Use:   "git-credential-forgejo <operation>",
		Short: "Git credential helper for a Forgejo (internal)",
		Long: `Answer git's credential protocol for a Forgejo from a bot's token file.

Git runs this command with the operation (get, store or erase) as the last
argument, and the request on standard input as key=value lines. Only "get"
answers: it prints username= and password= for an http or https request whose
host (with its port) is one of the --host values, and prints nothing for any
other, so git goes on to its other helpers. "store" and "erase" do nothing.

The token comes from --token-file, which must be a regular file readable by its
owner alone. The token is never printed to standard error and is never an
argument of this command.`,
		Hidden:        true,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGitCredentialForgejo(args[0], flags, in, out, readCredential)
		},
	}
	cmd.Flags().StringArrayVar(&flags.hosts, "host", nil,
		"A host:port this helper answers for (repeatable); any other host gets no answer")
	cmd.Flags().StringVar(&flags.tokenFile, "token-file", "",
		"Absolute path of the bot's token file (FORGEJO_USER and FORGEJO_TOKEN lines, mode 600)")
	return cmd
}

var gitCredentialForgejoCmd = newGitCredentialForgejoCmd(os.Stdin, os.Stdout, forgejo.ReadCredentialFile)

func init() {
	rootCmd.AddCommand(gitCredentialForgejoCmd)
}

// runGitCredentialForgejo answers one git credential request. Only "get" reads
// the request and the token file; "store" and "erase" return before either, and
// so does an operation git does not define.
func runGitCredentialForgejo(op string, flags gitCredentialForgejoFlags, in io.Reader, out io.Writer,
	readCredential func(path string) (user, token string, err error)) error {
	if op != "get" {
		return nil
	}
	hosts, err := normalizeCredentialHosts(flags.hosts)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(flags.tokenFile) {
		return fmt.Errorf("--token-file %q must be an absolute path", flags.tokenFile)
	}
	request, err := readCredentialRequest(in)
	if err != nil {
		return err
	}
	if proto := request["protocol"]; proto != "http" && proto != "https" {
		return nil
	}
	if !hosts[strings.ToLower(request["host"])] {
		return nil
	}
	user, token, err := readCredential(flags.tokenFile)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "username=%s\npassword=%s\n", user, token)
	return err
}

// normalizeCredentialHosts validates the allowlist and returns it as a set of
// lower-case host:port values. An empty allowlist is refused: a helper that
// answers for every host would hand the bot's token to any remote.
func normalizeCredentialHosts(hosts []string) (map[string]bool, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("--host is required: name each host:port this helper may answer for")
	}
	set := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		host := strings.TrimSpace(h)
		switch {
		case host == "":
			return nil, fmt.Errorf("--host is empty")
		case strings.HasPrefix(host, "-"):
			return nil, fmt.Errorf("--host %q begins with a dash, so it names a flag rather than a host", host)
		case strings.ContainsAny(host, "/@ \t"):
			return nil, fmt.Errorf("--host %q must be a bare host:port, with no scheme, path or login", host)
		}
		set[strings.ToLower(host)] = true
	}
	return set, nil
}

// readCredentialRequest reads git's key=value lines up to a blank line or the
// end of input. A line without "=" is ignored, as git's own helpers do.
func readCredentialRequest(in io.Reader) (map[string]string, error) {
	request := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			request[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read the credential request: %w", err)
	}
	return request, nil
}
