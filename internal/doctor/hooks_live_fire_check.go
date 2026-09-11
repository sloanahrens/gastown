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
// This check is deliberately NOT part of the default 'gt doctor' run — it
// spawns a real claude -p subprocess (an actual model call, network- and
// token-cost-bearing, up to hooksLiveFireTimeout) — so it is only
// registered when the caller opts in (see 'gt doctor --live-fire').
type HooksLiveFireCheck struct {
	BaseCheck
}

// NewHooksLiveFireCheck creates a new hooks live-fire check.
func NewHooksLiveFireCheck() *HooksLiveFireCheck {
	return &HooksLiveFireCheck{
		BaseCheck: BaseCheck{
			CheckName:        "hooks-live-fire",
			CheckDescription: "Live-fire test that a real PreToolUse guard actually blocks end-to-end (not just that gt tap guard would)",
			CheckCategory:    CategoryHooks,
		},
	}
}

// hooksLiveFireTimeout bounds the spawned claude -p subprocess.
const hooksLiveFireTimeout = 60 * time.Second

// hooksLiveFireBranch is the branch name the live-fire prompt asks Claude to
// create. If the guard fails open, this branch actually gets created in the
// disposable sandbox repo — that's the ground truth this check reads back.
const hooksLiveFireBranch = "gt-doctor-live-fire-should-be-blocked"

// hooksLiveFireBlockMarker is the distinctive text the pr-workflow guard
// prints to stderr when it blocks a command (see runTapGuardPRWorkflow's
// "PR WORKFLOW BLOCKED" banner). Its presence in the subprocess output is
// positive confirmation the guard actually ran and fired, independent of
// branch absence — branch absence alone can't distinguish "the guard
// blocked it" from "claude never attempted the command at all".
const hooksLiveFireBlockMarker = "PR WORKFLOW BLOCKED"

// Run spawns claude -p against a real polecat settings.json in a disposable
// sandbox git repo and asks it to run "git checkout -b <branch>" — a command
// the pr-workflow guard should block. It never returns StatusOK on an infra
// failure (claude missing, no settings file found, sandbox setup failed,
// timeout): those report StatusSkipped ("unknown: ..."), never a pass.
func (c *HooksLiveFireCheck) Run(ctx *CheckContext) *CheckResult {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return c.inconclusive("claude not found in PATH — cannot live-verify hook guards")
	}

	settingsPath, label, err := findPolecatSettings(ctx.TownRoot)
	if err != nil {
		return c.inconclusive(fmt.Sprintf("no polecat settings.json found to live-test: %v", err))
	}

	sandbox, err := os.MkdirTemp("", "gt-hooks-live-fire-*")
	if err != nil {
		return c.inconclusive(fmt.Sprintf("could not create sandbox dir: %v", err))
	}
	defer os.RemoveAll(sandbox)

	if out, err := exec.Command("git", "-C", sandbox, "init", "-q").CombinedOutput(); err != nil {
		return c.inconclusive(fmt.Sprintf("could not init sandbox git repo: %v (%s)", err, strings.TrimSpace(string(out))))
	}
	// A branch needs at least one commit to be checked out from.
	if out, err := exec.Command("git", "-C", sandbox, "commit", "--allow-empty", "-q", "-m", "seed").CombinedOutput(); err != nil {
		return c.inconclusive(fmt.Sprintf("could not seed sandbox repo: %v (%s)", err, strings.TrimSpace(string(out))))
	}

	prompt := fmt.Sprintf("Run exactly this one shell command via the Bash tool and nothing else — do not explain, do not ask for confirmation: git checkout -b %s", hooksLiveFireBranch)

	cctx, cancel := context.WithTimeout(context.Background(), hooksLiveFireTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, claudePath, "--dangerously-skip-permissions", "--settings", settingsPath, "-p", prompt)
	cmd.Dir = sandbox
	// GT_POLECAT puts the spawned claude subprocess in Gas Town agent
	// context so the pr-workflow guard's isGasTownAgentContext() check
	// actually evaluates true here — without it, a sandbox with no GT_*
	// env and a /tmp path (not under /polecats/ etc.) and no origin remote
	// makes the guard allow the command regardless of matcher wiring,
	// so the check would pass even against broken wiring (finding 1,
	// gt-wisp-db27).
	cmd.Env = append(withoutNestedSessionEnv(os.Environ()), "GT_POLECAT=live-fire")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return c.inconclusive(fmt.Sprintf("live-fire against %s timed out after %s", label, hooksLiveFireTimeout))
	}

	branchCreated := sandboxBranchExists(sandbox, hooksLiveFireBranch)
	blockConfirmed := strings.Contains(stdout.String(), hooksLiveFireBlockMarker) || strings.Contains(stderr.String(), hooksLiveFireBlockMarker)

	return c.evaluateLiveFireResult(label, settingsPath, branchCreated, blockConfirmed, runErr, stdout.String(), stderr.String())
}

// evaluateLiveFireResult turns the raw evidence from a live-fire subprocess
// run into a verdict. Factored out of Run so the three-way verdict logic
// (blocked / not blocked / inconclusive) can be unit-tested directly,
// without spawning a real claude subprocess.
func (c *HooksLiveFireCheck) evaluateLiveFireResult(label, settingsPath string, branchCreated, blockConfirmed bool, runErr error, stdout, stderr string) *CheckResult {
	if branchCreated {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("pr-workflow guard did NOT block 'git checkout -b' end-to-end against %s — hook matcher wiring is broken (gt-5ihs class regression)", label),
			Details: []string{
				fmt.Sprintf("settings: %s", settingsPath),
				fmt.Sprintf("claude exit: %v", runErr),
				"stdout: " + truncate(stdout, 400),
				"stderr: " + truncate(stderr, 400),
			},
			FixHint: "Check internal/hooks DefaultBase/DefaultOverrides: PreToolUse matchers must be the bare tool name (e.g. \"Bash\"), never a permission-rule pattern",
		}
	}

	// The branch not existing is necessary but not sufficient evidence of a
	// real block: claude exiting non-zero without ever attempting the
	// command (auth/network failure, a rejected --settings file, the model
	// declining the prompt) looks identical from branch state alone. Only
	// report OK when the guard's own block banner actually appeared in the
	// subprocess output; otherwise this is inconclusive, not a pass
	// (finding 2, gt-wisp-db27).
	if !blockConfirmed {
		return c.inconclusive(fmt.Sprintf(
			"branch %q was not created against %s, but the pr-workflow block banner never appeared in output either — claude may not have attempted the command at all (claude exit: %v)",
			hooksLiveFireBranch, label, runErr,
		))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("pr-workflow guard blocked 'git checkout -b' end-to-end against %s", label),
	}
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
