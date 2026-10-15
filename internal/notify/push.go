package notify

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify/pushcmd"
	"github.com/steveyegge/gastown/internal/workspace"
)

// commandRunner runs one notify command: its argv and the message for stdin.
// Tests replace it so no unit test starts a process.
type commandRunner func(argv []string, stdin string) error

// runNotifyCommand is the production commandRunner.
func runNotifyCommand(argv []string, stdin string) error {
	return pushcmd.Run(argv, stdin, pushcmd.Timeout)
}

// Push sends a message to the human through the command notify.commands.<name>
// names in the town's escalation config. The command reads the message on
// stdin: title on the first line, body after it. It finds the town from the
// working directory, as the gt commands do.
//
// The returned error never carries the command's argv; callers log it with
// the name.
func Push(name, title, body string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	return PushIn(townRoot, name, title, body)
}

// PushIn is Push for a caller that already holds the town root, as the daemon
// does: its working directory is not the town's to rely on.
func PushIn(townRoot, name, title, body string) error {
	cfg, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading escalation config: %w", err)
	}
	return pushThrough(runNotifyCommand, cfg, name, title, body)
}

// pushThrough runs the command cfg configures under name with the message on
// stdin.
func pushThrough(run commandRunner, cfg *config.EscalationConfig, name, title, body string) error {
	argv, ok := cfg.Notify.Commands[name]
	if !ok {
		return fmt.Errorf("notify command %q is not configured (notify.commands.%s)", name, name)
	}
	return run(argv, pushMessage(title, body))
}

// pushMessage is the text a notify command reads: the title as one line, then
// the body.
func pushMessage(title, body string) string {
	title = strings.Join(strings.Fields(title), " ")
	if body == "" {
		return title + "\n"
	}
	return title + "\n" + strings.TrimRight(body, "\n") + "\n"
}

// sendEscalationExec pushes one escalation through the notify command a route's
// exec:<name> action names.
func sendEscalationExec(run commandRunner, cfg *config.EscalationConfig, name, beadID, severity, description string) error {
	title := fmt.Sprintf("[Gas Town %s] %s", strings.ToUpper(severity), description)
	body := fmt.Sprintf("Bead: %s\nSeverity: %s\nAcknowledge: gt escalate ack %s", beadID, strings.ToUpper(severity), beadID)
	return pushThrough(run, cfg, name, title, body)
}
