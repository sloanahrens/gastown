package doctor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/hooks"
)

// HooksLiveFireCheck spawns a real headless Claude Code session against a
// live polecat settings.json and issues a command that the pr-workflow
// guard should block, then verifies the block actually happened.
//
// This check exists because gt-5ihs's root cause — hook matchers written in
// permission-rule syntax (e.g. "Bash(git checkout -b*)") instead of tool-name
// syntax, so Claude Code's matcher never fired — was invisible to every
// earlier verification method: calling 'gt tap guard <name>' directly, or
// any other proxy that doesn't go through Claude Code's actual hook
// dispatch, proves nothing about whether the matcher itself is wired
// correctly. Only a live end-to-end fire (spawn Claude Code with the real
// settings file, ask it to run the blocked command, observe whether the
// block actually happened) can catch a matcher-wiring regression like this
// one.
//
// It also runs the mirror-image probe: a command the guard must NOT block
// (git status). On 2026-09-10 a hooks-sync round-trip dropped the pr-workflow
// block's 'if' fields, turning its matcher unconditional — every polecat's
// Bash was denied for 7 minutes. The blocked-only probe can never catch that
// class of regression: an over-broad matcher still blocks the one command it
// tests. Only a paired allowed-shape probe, run against the same settings
// file, proves the guard is scoped correctly rather than merely present.
// Both shapes must pass for StatusOK (see RunLiveFirePair).
//
// This check is deliberately NOT part of the default 'gt doctor' run — it
// spawns real claude -p subprocesses (actual model calls, network- and
// token-cost-bearing, up to hooksLiveFireTimeout each) — so it is only
// registered when the caller opts in (see 'gt doctor --live-fire').
type HooksLiveFireCheck struct {
	BaseCheck
}

// NewHooksLiveFireCheck creates a new hooks live-fire check.
func NewHooksLiveFireCheck() *HooksLiveFireCheck {
	return &HooksLiveFireCheck{
		BaseCheck: BaseCheck{
			CheckName:        "hooks-live-fire",
			CheckDescription: "Live-fire pair test that a real PreToolUse guard blocks what it should and allows what it should (not just that gt tap guard would)",
			CheckCategory:    CategoryHooks,
		},
	}
}

// hooksLiveFireTimeout bounds each spawned claude -p subprocess.
const hooksLiveFireTimeout = 60 * time.Second

// hooksLiveFireBranch is the branch name the blocked-shape prompt asks
// Claude to create. If the guard fails open, this branch actually gets
// created in the disposable sandbox repo — that's the ground truth this
// shape reads back.
const hooksLiveFireBranch = "gt-doctor-live-fire-should-be-blocked"

// hooksLiveFireAllowedMarker is the file the allowed-shape prompt asks
// Claude to create after running a command the guard must NOT block. Its
// presence is the ground truth that the command actually executed — the
// same read-real-filesystem-state pattern the blocked shape uses, rather
// than trusting claude's own narration of what happened.
const hooksLiveFireAllowedMarker = "gt-doctor-live-fire-allowed-ok"

// hooksLiveFireBlockMarker is the distinctive text the pr-workflow guard
// prints to stderr when it blocks a command (see runTapGuardPRWorkflow's
// "PR WORKFLOW BLOCKED" banner). Its presence in the subprocess output is
// positive confirmation the guard actually ran and fired, independent of
// branch/marker state alone — that alone can't distinguish "the guard
// blocked it" from "claude never attempted the command at all".
const hooksLiveFireBlockMarker = "PR WORKFLOW BLOCKED"

// LiveFireVerdict is the outcome of one probe shape.
type LiveFireVerdict int

const (
	// LiveFireInconclusive means the probe could not determine an outcome —
	// infra failure, timeout, or ambiguous evidence. Never treat this as a
	// pass.
	LiveFireInconclusive LiveFireVerdict = iota
	// LiveFirePass means the shape's expected behavior was confirmed.
	LiveFirePass
	// LiveFireFail means the shape's expected behavior was NOT confirmed —
	// a real regression.
	LiveFireFail
)

// String returns a human-readable verdict.
func (v LiveFireVerdict) String() string {
	switch v {
	case LiveFirePass:
		return "pass"
	case LiveFireFail:
		return "fail"
	default:
		return "inconclusive"
	}
}

// LiveFireShapeResult is the outcome of one probe shape (blocked or allowed).
type LiveFireShapeResult struct {
	Verdict LiveFireVerdict
	Detail  string
}

// LiveFirePairResult is the combined outcome of both probe shapes run
// against the same settings file.
type LiveFirePairResult struct {
	Label        string
	SettingsPath string
	Blocked      LiveFireShapeResult
	Allowed      LiveFireShapeResult
}

// Passed reports whether both shapes confirmed their expected behavior.
func (r *LiveFirePairResult) Passed() bool {
	return r.Blocked.Verdict == LiveFirePass && r.Allowed.Verdict == LiveFirePass
}

// Failed reports whether either shape found a real regression. This is
// distinct from !Passed(): an inconclusive shape means "not proven", not
// "found broken" — callers that gate on failure (e.g. 'gt hooks sync'
// aborting fan-out) must only abort on Failed, never on a mere
// inconclusive result infra couldn't prove either way.
func (r *LiveFirePairResult) Failed() bool {
	return r.Blocked.Verdict == LiveFireFail || r.Allowed.Verdict == LiveFireFail
}

// Run spawns claude -p against a real polecat settings.json in a disposable
// sandbox git repo and runs the live-fire pair. It never returns StatusOK on
// an infra failure (claude missing, no settings file found, sandbox setup
// failed, timeout): those report StatusSkipped ("unknown: ..."), never a
// pass.
func (c *HooksLiveFireCheck) Run(ctx *CheckContext) *CheckResult {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return c.inconclusive("claude not found in PATH — cannot live-verify hook guards")
	}

	settingsPath, label, err := findPolecatSettings(ctx.TownRoot)
	if err != nil {
		return c.inconclusive(fmt.Sprintf("no polecat settings.json found to live-test: %v", err))
	}

	pair := RunLiveFirePair(claudePath, settingsPath, label)
	return c.evaluatePairResult(pair)
}

// RunLiveFirePair runs both live-fire probe shapes — blocked and allowed —
// against the same settings file and returns the combined evidence. Exported
// so 'gt hooks sync' can run the identical pair against a single canary
// settings file before fanning a hooks change out to every target (claude-
// 41j.1 D7/D8): the deployment record must be a real live-fire pair result,
// not a file count.
func RunLiveFirePair(claudePath, settingsPath, label string) *LiveFirePairResult {
	return &LiveFirePairResult{
		Label:        label,
		SettingsPath: settingsPath,
		Blocked:      runLiveFireBlockedShape(claudePath, settingsPath),
		Allowed:      runLiveFireAllowedShape(claudePath, settingsPath),
	}
}

// runLiveFireBlockedShape asks a fresh sandboxed claude session to run a
// command the pr-workflow guard should block ("git checkout -b <branch>")
// and reports whether the block actually happened.
func runLiveFireBlockedShape(claudePath, settingsPath string) LiveFireShapeResult {
	sandbox, err := os.MkdirTemp("", "gt-hooks-live-fire-blocked-*")
	if err != nil {
		return LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: fmt.Sprintf("could not create sandbox dir: %v", err)}
	}
	defer os.RemoveAll(sandbox)

	if out, err := exec.Command("git", "-C", sandbox, "init", "-q").CombinedOutput(); err != nil {
		return LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: fmt.Sprintf("could not init sandbox git repo: %v (%s)", err, strings.TrimSpace(string(out)))}
	}
	// A branch needs at least one commit to be checked out from.
	if out, err := exec.Command("git", "-C", sandbox, "commit", "--allow-empty", "-q", "-m", "seed").CombinedOutput(); err != nil {
		return LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: fmt.Sprintf("could not seed sandbox repo: %v (%s)", err, strings.TrimSpace(string(out)))}
	}

	prompt := fmt.Sprintf("Run exactly this one shell command via the Bash tool and nothing else — do not explain, do not ask for confirmation: git checkout -b %s", hooksLiveFireBranch)
	stdout, stderr, runErr, timedOut := runLiveFireClaude(claudePath, settingsPath, sandbox, prompt)
	if timedOut {
		return LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: fmt.Sprintf("blocked-shape probe timed out after %s", hooksLiveFireTimeout)}
	}

	branchCreated := sandboxBranchExists(sandbox, hooksLiveFireBranch)
	blockConfirmed := strings.Contains(stdout, hooksLiveFireBlockMarker) || strings.Contains(stderr, hooksLiveFireBlockMarker)

	return evaluateBlockedShape(settingsPath, branchCreated, blockConfirmed, runErr, stdout, stderr)
}

// runLiveFireAllowedShape asks a fresh sandboxed claude session to run a
// command the guard must NOT block ("git status"), chained with a marker
// file write. The marker file's presence afterward is the ground truth that
// the command actually executed — reading real filesystem state instead of
// trusting claude's own narration, the same pattern the blocked shape uses.
func runLiveFireAllowedShape(claudePath, settingsPath string) LiveFireShapeResult {
	sandbox, err := os.MkdirTemp("", "gt-hooks-live-fire-allowed-*")
	if err != nil {
		return LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: fmt.Sprintf("could not create sandbox dir: %v", err)}
	}
	defer os.RemoveAll(sandbox)

	if out, err := exec.Command("git", "-C", sandbox, "init", "-q").CombinedOutput(); err != nil {
		return LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: fmt.Sprintf("could not init sandbox git repo: %v (%s)", err, strings.TrimSpace(string(out)))}
	}

	prompt := fmt.Sprintf("Run exactly this one shell command via the Bash tool and nothing else — do not explain, do not ask for confirmation: git status && touch %s", hooksLiveFireAllowedMarker)
	stdout, stderr, runErr, timedOut := runLiveFireClaude(claudePath, settingsPath, sandbox, prompt)
	if timedOut {
		return LiveFireShapeResult{Verdict: LiveFireInconclusive, Detail: fmt.Sprintf("allowed-shape probe timed out after %s", hooksLiveFireTimeout)}
	}

	markerCreated := sandboxFileExists(sandbox, hooksLiveFireAllowedMarker)
	blockConfirmed := strings.Contains(stdout, hooksLiveFireBlockMarker) || strings.Contains(stderr, hooksLiveFireBlockMarker)

	return evaluateAllowedShape(settingsPath, markerCreated, blockConfirmed, runErr, stdout, stderr)
}

// runLiveFireClaude spawns claude -p with the given settings file and prompt
// inside dir, returning captured output. timedOut is true only when the
// subprocess was killed by the deadline.
func runLiveFireClaude(claudePath, settingsPath, dir, prompt string) (stdout, stderr string, runErr error, timedOut bool) {
	cctx, cancel := context.WithTimeout(context.Background(), hooksLiveFireTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, claudePath, "--dangerously-skip-permissions", "--settings", settingsPath, "-p", prompt)
	cmd.Dir = dir
	// GT_POLECAT puts the spawned claude subprocess in Gas Town agent
	// context so the pr-workflow guard's isGasTownAgentContext() check
	// actually evaluates true here — without it, a sandbox with no GT_*
	// env and a /tmp path (not under /polecats/ etc.) and no origin remote
	// makes the guard allow the command regardless of matcher wiring,
	// so the check would pass even against broken wiring (finding 1,
	// gt-wisp-db27).
	cmd.Env = append(withoutNestedSessionEnv(os.Environ()), "GT_POLECAT=live-fire")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr = cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return outBuf.String(), errBuf.String(), runErr, true
	}
	return outBuf.String(), errBuf.String(), runErr, false
}

// evaluateBlockedShape turns the raw evidence from a blocked-shape probe run
// into a verdict. Factored out so the three-way verdict logic (blocked / not
// blocked / inconclusive) can be unit-tested directly, without spawning a
// real claude subprocess.
func evaluateBlockedShape(settingsPath string, branchCreated, blockConfirmed bool, runErr error, stdout, stderr string) LiveFireShapeResult {
	if branchCreated {
		return LiveFireShapeResult{
			Verdict: LiveFireFail,
			Detail: fmt.Sprintf(
				"pr-workflow guard did NOT block 'git checkout -b' end-to-end against %s — hook matcher wiring is broken (gt-5ihs class regression); claude exit: %v; stdout: %s; stderr: %s",
				settingsPath, runErr, truncate(stdout, 400), truncate(stderr, 400),
			),
		}
	}

	// The branch not existing is necessary but not sufficient evidence of a
	// real block: claude exiting non-zero without ever attempting the
	// command (auth/network failure, a rejected --settings file, the model
	// declining the prompt) looks identical from branch state alone. Only
	// report pass when the guard's own block banner actually appeared in
	// the subprocess output; otherwise this is inconclusive, not a pass
	// (finding 2, gt-wisp-db27).
	if !blockConfirmed {
		return LiveFireShapeResult{
			Verdict: LiveFireInconclusive,
			Detail: fmt.Sprintf(
				"branch %q was not created against %s, but the pr-workflow block banner never appeared in output either — claude may not have attempted the command at all (claude exit: %v)",
				hooksLiveFireBranch, settingsPath, runErr,
			),
		}
	}

	return LiveFireShapeResult{
		Verdict: LiveFirePass,
		Detail:  fmt.Sprintf("pr-workflow guard blocked 'git checkout -b' end-to-end against %s", settingsPath),
	}
}

// evaluateAllowedShape turns the raw evidence from an allowed-shape probe
// run into a verdict. Mirrors evaluateBlockedShape's three-way logic: a
// created marker is a confirmed pass, a block banner with no marker is a
// confirmed regression (the guard over-fired on a command it must allow —
// exactly the 2026-09-10 dropped-'if' incident class), and anything else is
// inconclusive rather than either a false pass or a false failure.
func evaluateAllowedShape(settingsPath string, markerCreated, blockConfirmed bool, runErr error, stdout, stderr string) LiveFireShapeResult {
	if markerCreated {
		return LiveFireShapeResult{
			Verdict: LiveFirePass,
			Detail:  fmt.Sprintf("pr-workflow guard allowed 'git status' end-to-end against %s", settingsPath),
		}
	}

	if blockConfirmed {
		return LiveFireShapeResult{
			Verdict: LiveFireFail,
			Detail: fmt.Sprintf(
				"pr-workflow guard blocked 'git status' — a command it must allow — against %s (over-broad matcher, e.g. a dropped 'if' scoping field); claude exit: %v; stdout: %s; stderr: %s",
				settingsPath, runErr, truncate(stdout, 400), truncate(stderr, 400),
			),
		}
	}

	return LiveFireShapeResult{
		Verdict: LiveFireInconclusive,
		Detail: fmt.Sprintf(
			"marker file was not created against %s, and no block banner appeared either — claude may not have attempted the command at all (claude exit: %v)",
			settingsPath, runErr,
		),
	}
}

// evaluatePairResult turns a combined pair result into a doctor CheckResult.
// Both shapes must pass for StatusOK; either confirmed failure is
// StatusError naming the shape; anything short of that (including a mix of
// pass and inconclusive) stays StatusSkipped — a probe that could not fully
// prove both shapes has not proven the guard correct.
func (c *HooksLiveFireCheck) evaluatePairResult(pair *LiveFirePairResult) *CheckResult {
	if pair.Failed() {
		var details []string
		if pair.Blocked.Verdict == LiveFireFail {
			details = append(details, "blocked shape: "+pair.Blocked.Detail)
		}
		if pair.Allowed.Verdict == LiveFireFail {
			details = append(details, "allowed shape: "+pair.Allowed.Detail)
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("hooks live-fire pair failed against %s", pair.Label),
			Details: details,
			FixHint: "Check internal/hooks DefaultBase/DefaultOverrides: PreToolUse matchers must be the bare tool name (e.g. \"Bash\"), and guards must self-filter rather than rely on a permission-rule 'if' pattern",
		}
	}

	if pair.Passed() {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("hooks live-fire pair (blocked + allowed) confirmed end-to-end against %s", pair.Label),
		}
	}

	return c.inconclusive(fmt.Sprintf(
		"blocked=%s, allowed=%s against %s — %s / %s",
		pair.Blocked.Verdict, pair.Allowed.Verdict, pair.Label, pair.Blocked.Detail, pair.Allowed.Detail,
	))
}

// Fix is a no-op — this check has no mechanical fix, only investigation.
func (c *HooksLiveFireCheck) Fix(ctx *CheckContext) error {
	return fmt.Errorf("hooks-live-fire is not auto-fixable — investigate internal/hooks DefaultBase/DefaultOverrides matcher wiring")
}

// CanFix returns false — see Fix.
func (c *HooksLiveFireCheck) CanFix() bool {
	return false
}

// inconclusive reports that the live-fire probe could not determine whether
// the guard blocks or not — infra failure, timeout, or ambiguous evidence.
// This is a could-not-ask result, not a warning about something observed,
// so it must never aggregate as StatusOK and must be consistent with the
// rest of the doctor framework's could-not-ask convention (StatusSkipped).
func (c *HooksLiveFireCheck) inconclusive(message string) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusSkipped,
		Message: "unknown: " + message,
	}
}

// findPolecatSettings returns the path to any live polecat settings.json in
// the town and a human-readable label for it.
func findPolecatSettings(townRoot string) (path, label string, err error) {
	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		return "", "", err
	}
	for _, t := range targets {
		if t.Role != "polecat" {
			continue
		}
		if _, statErr := os.Stat(t.Path); statErr != nil {
			continue
		}
		return t.Path, t.DisplayKey(), nil
	}
	return "", "", fmt.Errorf("no polecat settings.json exists yet")
}

// sandboxBranchExists reports whether branch exists in the git repo at dir.
func sandboxBranchExists(dir, branch string) bool {
	out, err := exec.Command("git", "-C", dir, "branch", "--list", branch).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// sandboxFileExists reports whether name exists as a file directly under dir.
func sandboxFileExists(dir, name string) bool {
	_, err := os.Stat(dir + string(os.PathSeparator) + name)
	return err == nil
}

// withoutNestedSessionEnv strips Claude Code's nested-session-detection
// variables so a live-fire subprocess spawned from inside a Claude Code
// session isn't blocked by the nesting guard — this check intentionally
// spawns an independent, disposable Claude Code instance.
func withoutNestedSessionEnv(environ []string) []string {
	var filtered []string
	for _, e := range environ {
		if strings.HasPrefix(e, "CLAUDECODE=") || strings.HasPrefix(e, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// truncate shortens s to at most maxLen runes, appending an ellipsis marker
// when truncated.
func truncate(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "…"
}
