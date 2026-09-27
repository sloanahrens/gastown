package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// tap_guard_question_tool.go implements `gt tap guard question-tool`: a
// PreToolUse hook that denies the interactive question tool (AskUserQuestion)
// in a session with nobody at the pane (polecat, dog).
//
// Two incidents, same shape. gt-z83 (2026-09-08, polecat pyrite) and gt-163k8
// (2026-09-23, polecat garnet, 4h26m with 0 commits) both parked on a
// degenerate placeholder AskUserQuestion — question 'n/a', options a/b — that
// the polecat raised to wait on something. A dialog raised in an unattended
// session has nobody to answer it, and the session still reads as running while
// it waits, so nothing inside the town treats it as broken.
//
// gt-z83 built the recovery path: the witness sweeps polecat panes, recognizes
// the selection dialog, sends Escape and nudges the agent on
// (tmux.containsBlockingQuestionDialog, witness.recoverDialogBlockedPolecat).
// That is a net, not a cure. It fires only when a patrol pass captures the
// pane, and every gate in front of it is another way for the park to happen
// again: a polecat under an operator pause or a dispatch hold is skipped by
// design, a witness that is itself down runs no patrol at all, and gt-163k8
// recorded 4h26m of the net not catching.
//
// A question dialog is a category error for an unattended session, so this
// guard removes the tool rather than recovering from it. gt-8stz set the same
// rule for PermissionRequest — an ask with nobody to answer it becomes a denial
// naming a way forward (see tap_guard_permission_request.go). This applies that
// rule to the tool the model calls on its own initiative, which the
// PermissionRequest event never sees: that event answers a prompt the harness
// raises, and a model-initiated dialog raises none.
//
// The denial has to carry a way forward, because it removes the polecat's only
// interactive channel. A message that only says "no" would trade a 4h26m park
// for a dead end. Both halves of the guidance are what the witness's own dialog
// recovery already nudges a parked polecat toward (gt-z83): decide it
// autonomously, or escalate the decision to a human through the channel that
// does reach one.
const askUserQuestionTool = "AskUserQuestion"

var tapGuardQuestionToolCmd = &cobra.Command{
	Use:   "question-tool",
	Short: "Deny AskUserQuestion in a session with nobody at the pane",
	Long: `Block the interactive question tool for unattended Gas Town sessions.

Registered for the roles that run with nobody at the pane (polecat, dog) on the
AskUserQuestion tool matcher. A question raised in such a session parks it on a
dialog nobody can answer, while the session still reads as running, so no health
check in the town treats it as broken — a polecat sat 4h26m this way (gt-163k8).

Interactive roles carry no such hook, so their questions still reach a person.

Exit codes:
  0 - Operation allowed (not an unattended session, or not the question tool)
  2 - Operation BLOCKED (one-line reason on stderr, so the model self-corrects)`,
	// The block reason is the whole message: a usage dump after it buries the
	// line the model needs to see.
	SilenceUsage: true,
	RunE:         runTapGuardQuestionTool,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardQuestionToolCmd)
}

// questionToolInput is the subset of the Claude Code PreToolUse payload this
// guard reads. Only the tool name and the session's working directory matter —
// the guard denies the call by name, not by argument.
type questionToolInput struct {
	Cwd      string `json:"cwd"`
	ToolName string `json:"tool_name"`
}

func runTapGuardQuestionTool(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil || len(input) == 0 {
		// Nothing to judge. Some harness wrappers drain stdin before invoking a
		// guard, and denying on an empty payload would wedge the session — the
		// guard reports nothing it can substantiate rather than blocking blind
		// (same reasoning as tap_guard_polecat_paths.go).
		return nil
	}
	var hook questionToolInput
	if err := json.Unmarshal(input, &hook); err != nil {
		return nil
	}
	if hook.ToolName != askUserQuestionTool {
		return nil // Another tool — outside this guard's remit.
	}
	if !unattendedPromptSession(hook.Cwd) {
		return nil // A person is at the pane; the question reaches them.
	}

	fmt.Fprintf(os.Stderr, "question-tool: %s\n", questionToolDenial())
	return NewSilentExit(2)
}

// questionToolDenial composes the model-facing reason. It names what was
// denied, why this session cannot answer it, and the two ways forward — decide
// autonomously, or escalate a decision that genuinely needs a human.
func questionToolDenial() string {
	return fmt.Sprintf(
		"Denied by the unattended-question guard: this session has nobody at the "+
			"pane, so %s would park it on a dialog no one can answer while the "+
			"session still reads as running, and nothing in the town treats that as "+
			"broken (gt-163k8). Do not retry it. Decide it yourself and continue: "+
			"pick the option you judge best, say why, and keep working. If a human "+
			"decision is genuinely required, escalate through the channel that does "+
			"reach one: gt escalate -s medium %q",
		askUserQuestionTool, "<your question>")
}
