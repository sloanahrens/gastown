package cmd

import (
	"encoding/json"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// tap_nudge_drain.go implements `gt tap nudge-drain`: a PostToolUse hook that
// hands a running polecat the nudges queued for its session (gt-drwph).
//
// The overrun guard queues its "write a split proposal" nudge rather than
// waiting for an idle prompt (waiting overran the daemon's 30s context and
// counted a failure every tick, gt-t61ft). The queue is otherwise drained only
// by the UserPromptSubmit hook, which a polecat running autonomous tool turns
// never reaches, so the nudge expired unseen. PostToolUse fires after every
// tool call, a boundary the seat passes through whatever it is doing.

var tapNudgeDrainCmd = &cobra.Command{
	Use:   "nudge-drain",
	Short: "Surface queued nudges to a running polecat after a tool call",
	Long: `Drain this polecat session's queued nudges after a tool call.

Registered as a PostToolUse hook on the polecat role. The queue is otherwise
drained by the UserPromptSubmit hook, which does not fire while a polecat runs
tool turns, so a nudge queued for a working seat (the overrun guard's split
prompt) would sit until it expired.

Queued nudges go to the model as hook additionalContext. A drained nudge leaves
the queue, so it is shown once. An empty queue prints nothing.

Exit status is always 0: a failed drain must never fail the tool call it follows.`,
	SilenceUsage: true,
	RunE:         runTapNudgeDrain,
}

func init() {
	tapCmd.AddCommand(tapNudgeDrainCmd)
}

// nudgeDrainOutput is the PostToolUse result the harness reads off stdout.
type nudgeDrainOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func runTapNudgeDrain(cmd *cobra.Command, args []string) error {
	if os.Getenv("GT_POLECAT") == "" {
		return nil // the hook is registered for polecats; any other session is a no-op
	}
	// GT_SESSION spares a tmux round trip on every tool call.
	session := os.Getenv("GT_SESSION")
	if session == "" {
		session = tmux.CurrentSessionName()
	}
	townRoot, _, _ := workspace.FindFromCwdWithFallback()
	if townRoot == "" {
		townRoot = workspace.TownRootFromEnv(os.Getenv)
	}
	if townRoot == "" {
		return nil
	}
	return tapNudgeDrain(os.Stdout, os.Stderr, townRoot, session)
}

// tapNudgeDrain drains session's queue and writes the nudges to stdout as
// PostToolUse additionalContext, or nothing when the queue is empty.
func tapNudgeDrain(stdout, errOut io.Writer, townRoot, session string) error {
	text := nudge.FormatForInjection(drainNudgesFor(errOut, townRoot, session))
	if text == "" {
		return nil
	}
	var out nudgeDrainOutput
	out.HookSpecificOutput.HookEventName = "PostToolUse"
	out.HookSpecificOutput.AdditionalContext = text
	return json.NewEncoder(stdout).Encode(out)
}
