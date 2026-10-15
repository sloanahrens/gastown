package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/notify"
)

var notifyCmd = &cobra.Command{
	Use:     "notify",
	GroupID: GroupComm,
	Short:   "Push a message to the human through a configured command",
	Long: `Push messages to the human outside the terminal through the commands
named under notify.commands in the escalation config (settings/escalation.json,
or the "escalation" section of settings/config.json).

A command is an argv array run without a shell. The message arrives on its
stdin, the first line a short title. An escalation route reaches one with the
action exec:<name>. See docs/design/escalation.md.`,
	RunE: requireSubcommand,
}

var notifyTestCmd = &cobra.Command{
	Use:   "test <name>",
	Short: "Send 'gastown notify test' through the named notify command",
	Long: `Run notify.commands.<name> once with the message 'gastown notify test' and
print a verdict line: NOTIFY OK, or NOTIFY FAILED: <reason>. The reason never
includes the command's argv. A failure exits 1.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		err := runNotifyTest(cmd.OutOrStdout(), notify.Push, args[0])
		if _, ok := IsSilentExit(err); ok {
			// The verdict line is the answer; cobra must not repeat it as an error.
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
		}
		return err
	},
}

// runNotifyTest sends the test message through push and prints the verdict.
func runNotifyTest(out io.Writer, push func(name, title, body string) error, name string) error {
	if err := push(name, "gastown notify test", ""); err != nil {
		fmt.Fprintf(out, "NOTIFY FAILED: %v\n", err)
		return NewSilentExit(1)
	}
	fmt.Fprintln(out, "NOTIFY OK")
	return nil
}

func init() {
	notifyCmd.AddCommand(notifyTestCmd)
	rootCmd.AddCommand(notifyCmd)
}
