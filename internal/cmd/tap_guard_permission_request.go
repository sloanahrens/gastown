package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/workspace"
)

// tap_guard_permission_request.go implements `gt tap guard permission-request`:
// a PermissionRequest hook that denies, rather than leaves standing, a prompt
// an unattended session (polecat, dog) has nobody to answer — the denial names
// a retry formulation instead of parking the session silently (gt-8stz).
// Interactive roles carry no PermissionRequest entry, so their prompts still
// reach a person.
const (
	// promptEscalationTimeout bounds the escalation bead that records the
	// park. A hung Dolt must not hold the decision past the hook's budget.
	promptEscalationTimeout = 5 * time.Second
	// promptEscalationMaxDispatch caps the command text quoted into the
	// escalation.
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
	return tapGuardPermissionRequest(os.Stdin, os.Stdout, realGuardProcess(), defaultParkedPromptEscalator)
}

// tapGuardPermissionRequest is the permission-request guard: it reads the
// hook payload from stdin and the session from proc, escalates through
// escalate, and writes its decision to stdout.
func tapGuardPermissionRequest(stdin io.Reader, stdout io.Writer, proc guardProcess, escalate parkedPromptEscalator) error {
	input, err := io.ReadAll(stdin)
	if err != nil || len(input) == 0 {
		// Nothing to judge. The harness's own prompt flow proceeds, which is
		// what an empty payload leaves us no grounds to change.
		return nil
	}
	var hook permissionRequestInput
	if err := json.Unmarshal(input, &hook); err != nil {
		return nil
	}
	if !unattendedPromptSession(hook.Cwd, proc.getenv) {
		return nil
	}

	escalation := escalateParkedPromptOnce(hook, proc.tempDir(), escalate)
	writePermissionRequestDenial(stdout, hook, escalation)
	return nil
}

// unattendedPromptSession reports whether the requesting session runs with
// nobody at the pane. Only the polecat and dog roles qualify: the town spawns
// them into a tmux pane and leaves them alone, and their settings files are
// the only ones carrying this hook. Crew, witness, refinery and deacon
// sessions keep their prompts, so the check is what stops a copied entry from
// silencing a person's dialog.
//
// A role in the environment decides on its own: a crew member working inside a
// polecat worktree still has a person at the pane, and a stale polecat marker
// from a parent process must not deny their prompt. getenv reads the session's
// environment.
func unattendedPromptSession(cwd string, getenv func(string) string) bool {
	if role := strings.TrimSpace(getenv("GT_ROLE")); role != "" {
		return unattendedPromptRole(role)
	}
	if getenv("GT_POLECAT") != "" {
		return true
	}
	return cwd != "" && strings.Contains(cwd, "/polecats/")
}

// unattendedPromptRole reports whether a role name belongs to a session the
// town leaves alone: a polecat, addressed as <rig>/polecats/<name> or by the
// bare role.
func unattendedPromptRole(role string) bool {
	return role == constants.RolePolecat || strings.Contains(role, "/polecats/")
}

// isShellTool reports whether a tool carries a shell command in
// tool_input.command: Bash, and Monitor, which runs the same command shape as
// a background watch. It mirrors shellExecutingToolMatcher in internal/hooks,
// the registration this guard answers for (gt-nol0q).
func isShellTool(name string) bool {
	return name == "Bash" || name == "Monitor"
}

// promptRequestSummary names what asked, in the form the model needs to
// recognize the call it made.
func promptRequestSummary(hook permissionRequestInput) string {
	if isShellTool(hook.ToolName) && hook.ToolInput.Command != "" {
		return fmt.Sprintf("the %s command %q", hook.ToolName, hook.ToolInput.Command)
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
	if !isShellTool(hook.ToolName) {
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
func writePermissionRequestDenial(stdout io.Writer, hook permissionRequestInput, escalation string) {
	var out permissionRequestOutput
	out.HookSpecificOutput.HookEventName = "PermissionRequest"
	out.HookSpecificOutput.Decision.Behavior = "deny"
	out.HookSpecificOutput.Decision.Message = promptDeniedMessage(hook, escalation)
	encoded, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Fprintln(stdout, string(encoded))
}

// escalateParkedPromptOnce files an escalation bead that a prompt parked this
// session, and returns the sentence the denial carries about it. The marker is
// written before the escalation is attempted, so a retry loop cannot write a
// Dolt commit per attempt. Each failure path returns its own sentence rather
// than "" so a silent bookkeeping or delivery failure is never
// indistinguishable from a deny that carries no escalation note at all
// (gt-8stz review, finding 4ce05cf3cf09). The marker lives in tempDir;
// escalate files the bead.
func escalateParkedPromptOnce(hook permissionRequestInput, tempDir string, escalate parkedPromptEscalator) string {
	shape := promptShape(hook)
	marker := promptEscalationMarker(hook, shape, tempDir)
	if info, err := os.Stat(marker); err == nil {
		// A marker left by a FAILED escalation suppresses retries only for
		// parkedPromptRetryAfter, so one Dolt hiccup does not silence the
		// shape for the rest of the session (gt-pb77k). A success is final.
		if !markerRecordsFailure(marker) || time.Since(info.ModTime()) < parkedPromptRetryAfter {
			return " This call was already reported once this session."
		}
	}
	if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return " Escalation bookkeeping failed, so a retry of this call may report it again."
	}
	subject := fmt.Sprintf("Parked prompt denied: %s", shape)
	body := promptEscalationBody(hook, shape)
	if !escalate(subject, body) {
		_ = os.WriteFile(marker, []byte(parkedPromptFailedMarker+"\n"), 0o600)
		return " Recording the escalation failed; mail the witness if this call is required."
	}
	return " The escalation was recorded as a bead."
}

// parkedPromptFailedMarker is the marker content that records a failed
// escalation; parkedPromptRetryAfter is how long that failure suppresses a retry.
const (
	parkedPromptFailedMarker = "escalation-failed"
	parkedPromptRetryAfter   = 5 * time.Minute
)

func markerRecordsFailure(marker string) bool {
	data, err := os.ReadFile(marker)
	return err == nil && strings.HasPrefix(string(data), parkedPromptFailedMarker)
}

// promptShape labels the denied call for the mail subject and the rate-limit
// key: two denials of the same shape in one session share a marker.
func promptShape(hook permissionRequestInput) string {
	if !isShellTool(hook.ToolName) {
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
//
// A missing session_id must not fall back to a bare empty string: every
// session missing it would then share one marker, so the first one to park
// would silently suppress every later, unrelated session's report of the same
// shape (gt-8stz review, finding 8de95c69660d). The cwd is stable for one
// session and distinct across polecats, so it is the first fallback; a
// process-local key is the last resort when even that is empty, which trades
// per-session dedup for never colliding with another session. The marker lives
// in tempDir (the session's $TMPDIR).
func promptEscalationMarker(hook permissionRequestInput, shape, tempDir string) string {
	key := hook.SessionID
	if key == "" {
		if hook.Cwd != "" {
			key = "cwd:" + hook.Cwd
		} else {
			key = fmt.Sprintf("pid:%d", os.Getpid())
		}
	}
	sum := sha256.Sum256([]byte(key + "\x00" + hook.ToolName + "\x00" + shape))
	return filepath.Join(tempDir, fmt.Sprintf("gt-parked-prompt-%x", sum[:8]))
}

// secretLikeToken matches a run of base64/hex/url-safe characters long enough
// to be a credential rather than an ordinary path segment or flag value.
var secretLikeToken = regexp.MustCompile(`[A-Za-z0-9_\-+/=]{20,}`)

// redactSecrets masks token-shaped substrings before a command reaches the
// escalation bead a person reads. It is deliberately over-eager — a false
// positive over a long non-secret string costs nothing, a leaked token is a
// real credential exposure — so this must run on any command text this guard
// files, never the raw string (gt-8stz review, finding 36adb619b71a).
func redactSecrets(s string) string {
	return secretLikeToken.ReplaceAllString(s, "[REDACTED]")
}

// cwdClass buckets a session's cwd into the shape a person needs to judge the
// report, without repeating the full path into the escalation the way the
// denied command no longer is (gt-8stz review, finding 36adb619b71a).
func cwdClass(cwd string) string {
	switch {
	case cwd == "":
		return "(unknown)"
	case strings.Contains(cwd, "/polecats/"):
		return "polecat worktree"
	case strings.Contains(cwd, "/dogs/"):
		return "dog worktree"
	default:
		return "other"
	}
}

// promptEscalationBody carries the rule name, session, cwd class, and a
// secret-redacted preview of the call plus its full hash — enough for a person
// to judge whether the deny is right or the shape should be steered away from,
// without the raw command (which may carry a credential) ever landing in the
// escalation (gt-8stz review, finding 36adb619b71a).
func promptEscalationBody(hook permissionRequestInput, shape string) string {
	dispatch := hook.ToolInput.Command
	if dispatch == "" {
		dispatch = hook.ToolInput.FilePath
	}
	preview := redactSecrets(dispatch)
	if len(preview) > promptEscalationMaxDispatch {
		preview = preview[:promptEscalationMaxDispatch] + " [truncated]"
	}
	hash := sha256.Sum256([]byte(dispatch))
	session := hook.SessionID
	if session == "" {
		session = "(none)"
	}
	return fmt.Sprintf(`A permission prompt was denied instead of parking an unattended session.

Rule: %s
Session: %s
Cwd class: %s
Denied call (secret-redacted): %s
Call hash: %x

Claude Code raised a permission prompt that nobody can answer at this pane, so
the guard answered it with a deny and a retry formulation. If the call was
legitimate, steer the worker to a formulation that does not ask.`,
		shape, session, cwdClass(hook.Cwd), preview, hash[:8])
}

// parkedPromptEscalator files the escalation and reports whether it was
// recorded. A parameter so tests can pass a fake and verify
// escalateParkedPromptOnce reaches the escalate call without ever touching the
// town's beads (gt-8stz review, finding d1058e444298).
type parkedPromptEscalator func(subject, body string) bool

// defaultParkedPromptEscalator raises the parked prompt as an escalation
// bead — escalations are bead-only (gt-rwp7z.6) — bounded so a slow or hung
// Dolt cannot hold the hook open.
func defaultParkedPromptEscalator(subject, body string) bool {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return false
	}
	cfg, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return false
	}
	done := make(chan error, 1)
	go func() {
		_, err := notify.Raise(notify.EscalationRequest{
			TownRoot:    townRoot,
			Severity:    config.SeverityHigh,
			Description: subject,
			Reason:      body,
			Source:      "tap-guard:parked-prompt",
			EscalatedBy: detectSender(),
			Prefixes:    townRegistry(),
		}, cfg)
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(promptEscalationTimeout):
		return false
	}
}
