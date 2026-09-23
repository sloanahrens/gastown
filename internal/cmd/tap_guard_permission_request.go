package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/workspace"
)

// tap_guard_permission_request.go implements `gt tap guard permission-request` (gt-8stz).
//
// Claude Code answers some Bash shapes with a prompt rather than an allow: a
// compound command that changes directory and writes ("Compound command
// contains cd with write operation — manual approval required to prevent path
// resolution bypass"), and a removal whose target the analyzer cannot resolve
// (an rm over a glob after a cd). The message says those asks cannot be
// auto-allowed by permission rules, and the rm class is bypass-immune, so
// --dangerously-skip-permissions does not clear it either.
//
// A prompt is only meaningful when someone can answer it. A polecat or dog
// runs alone in a tmux pane and the harness counts that pane as an interactive
// surface, so the dialog is raised and never answered: the session parks while
// still reading as running. Registered as a PermissionRequest hook for those
// roles, this guard answers the ask with a deny whose message carries the safe
// formulation, so the model gets a failure it can act on (gt-8stz). Interactive
// roles get no PermissionRequest entry (internal/hooks), so their prompts keep
// reaching a person.
const (
	// promptEscalationTimeout bounds the mail that records the park. A hung
	// Dolt must not hold the decision past the hook's budget.
	promptEscalationTimeout = 5 * time.Second
	// promptEscalationMaxDispatch caps the command text quoted into the mail.
	promptEscalationMaxDispatch = 500
)

var tapGuardPermissionRequestCmd = &cobra.Command{
	Use:   "permission-request",
	Short: "Deny a permission prompt that an unattended session cannot answer",
	Long: `Answer a Claude Code PermissionRequest hook so an unattended session fails instead of parking.

Registered for the roles that run with nobody at the pane (polecat, dog), this
guard denies the request and returns, as the model-facing message, the reason
and a formulation that will not ask again. Interactive roles carry no such
hook, so their prompts stay with the person.

Exit status is always 0: this event is answered through the decision object on
stdout, and exit 2 is not honored for it.`,
	SilenceUsage: true,
	RunE:         runTapGuardPermissionRequest,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardPermissionRequestCmd)
}

// permissionRequestInput is the subset of the hook payload this guard reads.
type permissionRequestInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// permissionRequestOutput is the decision the harness reads off stdout.
type permissionRequestOutput struct {
	HookSpecificOutput struct {
		HookEventName string `json:"hookEventName"`
		Decision      struct {
			Behavior string `json:"behavior"`
			Message  string `json:"message"`
		} `json:"decision"`
	} `json:"hookSpecificOutput"`
}

func runTapGuardPermissionRequest(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil || len(input) == 0 {
		// Nothing to judge. The harness's own prompt flow proceeds, which is
		// what an empty payload leaves us no grounds to change.
		return nil
	}
	var hook permissionRequestInput
	if err := json.Unmarshal(input, &hook); err != nil {
		return nil
	}
	if !unattendedPromptSession(hook.Cwd) {
		return nil
	}

	escalation := escalateParkedPromptOnce(hook)
	writePermissionRequestDenial(hook, escalation)
	return nil
}

// unattendedPromptSession reports whether the requesting session runs with
// nobody at the pane. Only the polecat and dog roles qualify: the town spawns
// them into a tmux pane and leaves them alone, and their settings files are
// the only ones carrying this hook. Crew, mayor, witness, refinery and deacon
// sessions keep their prompts, so the check is what stops a copied entry from
// silencing a person's dialog.
//
// A role in the environment decides on its own: a crew member working inside a
// polecat worktree still has a person at the pane, and a stale polecat marker
// from a parent process must not deny their prompt.
func unattendedPromptSession(cwd string) bool {
	if role := strings.TrimSpace(os.Getenv("GT_ROLE")); role != "" {
		return unattendedPromptRole(role)
	}
	if os.Getenv("GT_POLECAT") != "" {
		return true
	}
	return cwd != "" && strings.Contains(cwd, "/polecats/")
}

// unattendedPromptRole reports whether a role name belongs to a session the
// town leaves alone: a polecat, or a dog. Both are addressed as
// <scope>/<role>/<name> paths, and a dog's GT_ROLE is the bare "dog".
func unattendedPromptRole(role string) bool {
	if role == constants.RoleDog || role == constants.RolePolecat {
		return true
	}
	return strings.Contains(role, "/polecats/") || strings.Contains(role, "/dogs/")
}

// promptRequestSummary names what asked, in the form the model needs to
// recognize the call it made.
func promptRequestSummary(hook permissionRequestInput) string {
	if hook.ToolName == "Bash" && hook.ToolInput.Command != "" {
		return fmt.Sprintf("the Bash command %q", hook.ToolInput.Command)
	}
	if path := hook.ToolInput.FilePath; path != "" {
		return fmt.Sprintf("%s on %s", hook.ToolName, path)
	}
	if path := hook.ToolInput.NotebookPath; path != "" {
		return fmt.Sprintf("%s on %s", hook.ToolName, path)
	}
	return hook.ToolName
}

// promptDeniedMessage composes the model-facing denial: why the session cannot
// answer, what was denied, the formulation that needs no approval, and the
// escalation outcome. The first retry succeeds only if it names that
// formulation, so the guidance is the load-bearing half (gt-8stz).
func promptDeniedMessage(hook permissionRequestInput, escalation string) string {
	return fmt.Sprintf(
		"Denied by the unattended-prompt guard: this session has nobody at the pane, so a permission prompt would park it with no way to answer (gt-8stz). Denied: %s. %s%s",
		promptRequestSummary(hook), promptRetryGuidance(hook), escalation)
}

// promptRetryGuidance names a formulation that raises no prompt, chosen from
// the shape that raised this one.
func promptRetryGuidance(hook permissionRequestInput) string {
	if hook.ToolName != "Bash" {
		return "Retry in a form that needs no approval, or escalate if this one is required."
	}
	segments := commandSegments(hook.ToolInput.Command)
	if segmentsChangeDirAndWrite(segments) {
		return `Retry without cd, naming the directory absolutely — "rm -rf \"$dir\"/*" or "touch /abs/path/f" rather than "cd $dir && ...".`
	}
	if segmentsGlobRemoval(segments) {
		return "Retry with the paths named explicitly — \"rm -rf /tmp/work/one /tmp/work/two\" rather than a glob."
	}
	return "Retry with absolute paths and no cd in a compound command, or escalate if this call is genuinely required."
}

// commandSegments splits a command into its per-segment token runs, the unit
// Claude Code's own Bash permission rules are judged in.
func commandSegments(command string) [][]string {
	tokens := shellTokenize(stripHeredocBodies(strings.TrimSpace(command)))
	var segments [][]string
	var segment []string
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			if len(segment) > 0 {
				segments = append(segments, segment)
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	if len(segment) > 0 {
		segments = append(segments, segment)
	}
	return segments
}

// segmentsChangeDirAndWrite reports the shape the analyzer asks about as
// cd-compound-write: one segment changing directory, another writing to a path
// whose resolution depends on that change.
func segmentsChangeDirAndWrite(segments [][]string) bool {
	changedDir, wrote := false, false
	for _, segment := range segments {
		word := segment[0]
		if word == "cd" {
			changedDir = true
		}
		if writeCommands[word] || hasWriteRedirection(segment) {
			wrote = true
		}
	}
	return changedDir && wrote
}

// hasWriteRedirection reports a redirection that creates or truncates a file,
// which the analyzer counts as a write the same way it counts a write command.
func hasWriteRedirection(segment []string) bool {
	for _, tok := range segment {
		if tok == ">" || tok == ">>" || strings.HasPrefix(tok, ">") && !strings.HasPrefix(tok, ">&") {
			return true
		}
	}
	return false
}

// segmentsGlobRemoval reports the shape of the second live park (gt-8stz): a
// removal whose target is a glob the analyzer cannot resolve to a directory.
func segmentsGlobRemoval(segments [][]string) bool {
	for _, segment := range segments {
		if segment[0] != "rm" && segment[0] != "rmdir" {
			continue
		}
		for _, tok := range segment[1:] {
			if strings.HasPrefix(tok, "-") {
				continue
			}
			if strings.Contains(tok, "*") || strings.Contains(tok, "?") {
				return true
			}
		}
	}
	return false
}

// writePermissionRequestDenial writes the deny decision, the last thing the
// guard does. Escalation runs first so the message can state its real outcome;
// that escalation is bounded, so a slow Dolt cannot cost the model its failure
// signal.
func writePermissionRequestDenial(hook permissionRequestInput, escalation string) {
	var out permissionRequestOutput
	out.HookSpecificOutput.HookEventName = "PermissionRequest"
	out.HookSpecificOutput.Decision.Behavior = "deny"
	out.HookSpecificOutput.Decision.Message = promptDeniedMessage(hook, escalation)
	encoded, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stdout, string(encoded))
}

// escalateParkedPromptOnce mails the mayor that a prompt parked this session,
// and returns the sentence the denial carries about it. The marker is written
// before the mail is attempted, so a retry loop cannot write a Dolt commit per
// attempt.
func escalateParkedPromptOnce(hook permissionRequestInput) string {
	shape := promptShape(hook)
	marker := promptEscalationMarker(hook, shape)
	if _, err := os.Stat(marker); err == nil {
		return " This call was already reported once this session."
	}
	if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return ""
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return ""
	}
	subject := fmt.Sprintf("Parked prompt denied: %s", shape)
	body := promptEscalationBody(hook, shape)
	if !sendParkedPromptMail(townRoot, subject, body) {
		return " Escalating to the mayor failed; mail the witness if this call is required."
	}
	return " The mayor has been notified."
}

// promptShape labels the denied call for the mail subject and the rate-limit
// key: two denials of the same shape in one session share a marker.
func promptShape(hook permissionRequestInput) string {
	if hook.ToolName != "Bash" {
		return hook.ToolName
	}
	segments := commandSegments(hook.ToolInput.Command)
	switch {
	case segmentsChangeDirAndWrite(segments):
		return "bash cd-compound-write"
	case segmentsGlobRemoval(segments):
		return "bash rm-glob"
	default:
		return "bash"
	}
}

// promptEscalationMarker is the per-session, per-shape marker path that makes
// the escalation once-only.
func promptEscalationMarker(hook permissionRequestInput, shape string) string {
	sum := sha256.Sum256([]byte(hook.SessionID + "\x00" + hook.ToolName + "\x00" + shape))
	return filepath.Join(os.TempDir(), fmt.Sprintf("gt-parked-prompt-%x", sum[:8]))
}

// promptEscalationBody carries the command and the reason, which is what the
// mayor needs to judge whether the deny is the right call or the shape should
// be steered away from (gt-8stz).
func promptEscalationBody(hook permissionRequestInput, shape string) string {
	dispatch := hook.ToolInput.Command
	if dispatch == "" {
		dispatch = hook.ToolInput.FilePath
	}
	if len(dispatch) > promptEscalationMaxDispatch {
		dispatch = dispatch[:promptEscalationMaxDispatch] + " [truncated]"
	}
	return fmt.Sprintf(`A permission prompt was denied instead of parking an unattended session.

Session: %s
Cwd: %s
Shape: %s
Denied call: %s

Claude Code raised a permission prompt that nobody can answer at this pane, so
the guard answered it with a deny and a retry formulation. If the call was
legitimate, steer the worker to a formulation that does not ask.`,
		hook.SessionID, hook.Cwd, shape, dispatch)
}

// sendParkedPromptMail delivers the escalation, bounded so a slow or hung Dolt
// cannot hold the hook open.
func sendParkedPromptMail(townRoot, subject, body string) bool {
	done := make(chan error, 1)
	go func() {
		router := mail.NewRouter(townRoot)
		done <- router.Send(&mail.Message{
			From:     detectSender(),
			To:       "mayor/",
			Subject:  subject,
			Body:     body,
			Type:     mail.TypeEscalation,
			Priority: mail.PriorityHigh,
		})
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(promptEscalationTimeout):
		return false
	}
}
