package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// The daemon pod a Jev command reads the logs in: the control cluster's
// kubectl context, the namespace the daemon Deployment lands in, and the
// Deployment whose pod carries the logs. kubectl resolves TYPE/NAME to the
// Deployment's one Running pod, so the refusal names no pod hash that a
// restart would invalidate.
const (
	jevPodContext      = "colima-gt-control"
	jevPodNamespace    = k8sdaemon.DefaultNamespace
	jevPodContainer    = k8sdaemon.Name
	jevPodExecResource = "deploy/" + k8sdaemon.Name
)

// jevPodHelp is the paragraph every command that reads the log directory
// carries in its help. They share one refusal, so they share one statement of
// it. It names no host, install, date or build, so it cannot go stale.
const jevPodHelp = `
The Jev logs live in the daemon pod. Outside it this command refuses before
reading any file and prints the kubectl exec line that runs it in the pod.`

// jevRequirePod refuses a Jev command that reads the town's log directory when
// it runs outside the daemon pod. The pod is the only place the logs are
// written, so a copy anywhere else can be stale and a command that read one
// would print stale numbers as if they were live (gt-8mjyqc).
//
// getenv is the environment lookup; the commands pass os.Getenv and a test
// passes a lookup of its own. A nil lookup is unknown, and an unknown
// environment refuses: a guard whose input is not wired must never read a copy.
func jevRequirePod(getenv func(string) string, cmd *cobra.Command) error {
	if getenv != nil && getenv(podEnv) != "" {
		return nil
	}
	return fmt.Errorf("the Jev logs live in the daemon pod, so this command refuses to read them outside it. Run it in the pod:\n\n  %s",
		jevPodCommand(cmd))
}

// jevPodCommand is the kubectl line the refusal prints: an exec into the daemon
// pod running the same gt command.
func jevPodCommand(cmd *cobra.Command) string {
	argv := []string{
		"kubectl",
		"--context", jevPodContext,
		"-n", jevPodNamespace,
		"exec", jevPodExecResource,
		"-c", jevPodContainer,
		"--",
	}
	return strings.Join(append(argv, jevPodArgv(cmd)...), " ")
}

// jevPodArgv is the gt command the reader runs in the pod: gt jev, the
// subcommand, and the flags the caller gave. Only the flags the caller set are
// named, since a default is what the pod's command takes anyway. The flags
// come in name order, so the same invocation always prints the same line.
func jevPodArgv(cmd *cobra.Command) []string {
	argv := []string{"gt", "jev", cmd.Name()}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		argv = append(argv, jevPodFlag(f))
	})
	return argv
}

// jevPodFlag renders one changed flag as the caller would type it: a true bool
// as the bare name, anything else as name=value with the value shell-quoted so
// the line pastes as one command.
func jevPodFlag(f *pflag.Flag) string {
	if f.Value.Type() == "bool" && f.Value.String() == "true" {
		return "--" + f.Name
	}
	return "--" + f.Name + "=" + jevShellQuote(f.Value.String())
}

// jevShellQuote renders s as one POSIX shell word when a bare word would not
// reach the command unchanged: when it is empty or holds a character the shell
// reads. A single quote inside the value is closed, escaped and reopened.
func jevShellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\!*?[]{}();&|<>#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
