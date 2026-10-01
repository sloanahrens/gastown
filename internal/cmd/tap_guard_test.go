package cmd

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestIsPRCreateCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"gh pr create", "gh pr create --title foo", true},
		{"gh pr create mixed case", "GH PR CREATE --title foo", true},
		{"chained after unrelated segment", "echo hi && gh pr create --title foo", true},
		{"git checkout -b", "git checkout -b temp origin/branch", false},
		{"git switch -c", "git switch -c temp origin/branch", false},
		{"unrelated", "echo hello", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPRCreateCommand(tt.command); got != tt.want {
				t.Errorf("isPRCreateCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// gt-cyz8: both pr-workflow exemptions must be keyed on the leading-command
// branch-creation shape, not the old line-wide isFeatureBranchCommand (now
// removed). The two matched the same leading-command shapes but differed on
// the chained case — an exemption's escape was a "git checkout -b" glued
// after a "gh pr create" on one line, which line-wide containment matches but
// a leading-command match does not.
func TestIsLeadingBranchCreation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"refinery rehearsal checkout", "git checkout -b temp origin/polecat/topaz+abc123", true},
		{"polecat session branch", "git checkout -b polecat/pearl/gt-da2x+mu6jwe92", true},
		{"switch -c", "git switch -c temp origin/branch", true},
		{"checkout -b with extra flag", "git checkout -q -b temp origin/branch", true},
		{"gh pr create", "gh pr create --title foo", false},
		{"plain checkout (no -b)", "git checkout main", false},
		{"plain switch (no -c)", "git switch main", false},
		{"no git", "checkout -b temp", false},
		{"empty", "", false},
		{"chained after unrelated segment", "echo hi && git checkout -b temp origin/branch", false},
		{"chained after pr create", "gh pr create --title foo && git checkout -b temp", false},
		{"chained before pr create", "git checkout -b temp && gh pr create --title foo", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLeadingBranchCreation(tt.command); got != tt.want {
				t.Errorf("isLeadingBranchCreation(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// fakeGuardProcess is a guardProcess whose environment is env and whose
// working directory is wd ("" makes getwd fail, as a deleted cwd does). HOME
// and TMPDIR come from env, as os.UserHomeDir and os.TempDir read them on
// Unix; there are no host temp dirs, no origin remote, and the host is idle.
func fakeGuardProcess(env map[string]string, wd string) guardProcess {
	return guardProcess{
		getenv: func(key string) string { return env[key] },
		lookupEnv: func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		},
		getwd: func() (string, error) {
			if wd == "" {
				return "", errors.New("getwd: no working directory")
			}
			return wd, nil
		},
		homeDir: func() (string, error) {
			if home := env["HOME"]; home != "" {
				return home, nil
			}
			return "", errors.New("$HOME is not defined")
		},
		tempDir: func() string {
			if dir := env["TMPDIR"]; dir != "" {
				return dir
			}
			return "/tmp"
		},
		originURL: func() (string, error) { return "", errors.New("no origin remote") },
		load1:     func() (float64, bool) { return 0, true },
	}
}

// bareGuardProcess has an empty environment and no working directory.
var bareGuardProcess = fakeGuardProcess(nil, "")

// noTownSession judges a command as bareGuardProcess, outside any town.
var noTownSession = guardSession{proc: bareGuardProcess}

// unreadableReader fails every read, the way a write-only stdin does.
type unreadableReader struct{}

func (unreadableReader) Read([]byte) (int, error) { return 0, errors.New("read: bad file descriptor") }

func TestRunTapGuardPRWorkflow_RefineryStillBlocksPRCreate(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_REFINERY": "1",
		"GT_ROLE":     "gastown/refinery",
	}, "")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"gh pr create --title foo"}}`
	err := tapGuardPRWorkflow(strings.NewReader(hookInput), io.Discard, proc)
	if err == nil {
		t.Error("expected gh pr create to remain blocked for the refinery role, got nil error")
	}
}

// The same shape must stay BLOCKED for a non-refinery agent — the branch
// name alone is not an exemption; the role is what carries it (gt-r2xm's
// scoping, and TestRunTapGuardPRWorkflow_NonRefineryStillBlocksCheckout's
// sandbox-cwd pin applies).
func TestRunTapGuardPRWorkflow_NonRefineryStillBlocksRehearsalBranchName(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_ROLE":    "gastown/polecats/topaz",
		"GT_POLECAT": "topaz",
	}, "")

	hookInput := polecatBranchPayload(t.TempDir(), "git fetch --prune origin\ngit checkout -b temp origin/main\ngit merge --no-ff --no-edit origin/main")
	err := tapGuardPRWorkflow(strings.NewReader(hookInput), io.Discard, proc)
	if err == nil {
		t.Error("expected a non-refinery agent to remain blocked on a rehearsal-named branch, got nil error")
	}
}

// A later-segment checkout naming an arbitrary (non-rehearsal) branch is
// the feature-branch shape this guard blocks, refinery role included:
// the name-scoped exemption (gt-mo53) must not widen into "refinery may
// create any branch on any segment."
func TestRunTapGuardPRWorkflow_RefineryArbitraryBranchLaterSegmentStillBlocks(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_REFINERY": "1",
		"GT_ROLE":     "gastown/refinery",
	}, "")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"git fetch --prune origin\ngit checkout -b feature/foo origin/main"}}`
	err := tapGuardPRWorkflow(strings.NewReader(hookInput), io.Discard, proc)
	if err == nil {
		t.Error("expected a non-rehearsal branch name on a later segment to remain blocked for the refinery role, got nil error")
	}
}

// The guard's whole job is blocking PR creation — the pr-workflow prefix
// match is the self-filter that routes these commands here, so any
// segment containing "gh pr create" stays blocked for the refinery role
// too (gt-cyz8's asymmetric-matcher class). The leading-branch-creation
// clause of the exemption (retained for the chains its tests pin) is not
// subject to the isPRCreateCommand clause, so this stays blocked because
// the command does NOT match the leading shape — it does not prove the
// clause itself.
func TestRunTapGuardPRWorkflow_RefineryPRCreateThenRehearsalNameStillBlocks(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_REFINERY": "1",
		"GT_ROLE":     "gastown/refinery",
	}, "")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"git fetch origin\ngit checkout -b temp origin/main && gh pr create --title foo"}}`
	err := tapGuardPRWorkflow(strings.NewReader(hookInput), io.Discard, proc)
	if err == nil {
		t.Error("expected a rehearsal-named checkout glued after gh pr create to remain blocked, got nil error")
	}
}

// TestRunTapGuardPRWorkflow_NonRefineryStillBlocksCheckout pins that the
// refinery's merge-rehearsal exemption (gt-r2xm) is scoped to the refinery
// role.
//
// Hermetic (gt-f1mun): the verdict must not move with the environment this
// suite runs in. The gt-6hg7 exemption reads the session's role and its own
// worktree from the process env, so an inherited polecat context — a
// GT_POLECAT_PATH naming a worktree, or a cwd inside one, which
// isInOwnPolecatWorktree falls back to when the payload omits it — would allow
// this checkout instead of blocking it. Both are pinned: GT_POLECAT_PATH empty,
// and a payload cwd in a fresh sandbox outside any polecat layout. The GT_*
// variables isGasTownAgentContext reads can only add a block, which is the
// verdict asserted here. The positive case — a polecat's session branch in its
// own worktree — is TestRunTapGuardPRWorkflow_PolecatSessionBranchAllowed.
func TestRunTapGuardPRWorkflow_NonRefineryStillBlocksCheckout(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_ROLE":    "gastown/polecats/topaz",
		"GT_POLECAT": "topaz",
	}, "")

	hookInput := polecatBranchPayload(t.TempDir(), "git checkout -b temp origin/main")
	err := tapGuardPRWorkflow(strings.NewReader(hookInput), io.Discard, proc)
	if err == nil {
		t.Error("expected non-refinery feature-branch checkout to remain blocked, got nil error")
	}
}

// TestRunTapGuardPRWorkflow_BlocksNewlineSeparatedCommand pins the gt-3j8u
// bypass at the guard boundary rather than at the matcher: the reported
// fail-open was a multi-line Bash call whose blocked shape sat on a later
// line ("cd /tmp" + newline + "gh pr create ..."), read from the hook
// payload the harness actually sends. JSON's \n escape decodes to the real
// newline the tokenizer has to treat as a command separator.
func TestRunTapGuardPRWorkflow_BlocksNewlineSeparatedCommand(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_ROLE":    "gastown/polecats/topaz",
		"GT_POLECAT": "topaz",
	}, "")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"cd /tmp\ngit checkout -b feature/x"}}`
	err := tapGuardPRWorkflow(strings.NewReader(hookInput), io.Discard, proc)
	if err == nil {
		t.Error("expected a blocked shape on a later line of a multi-line command to be blocked, got nil error")
	}
}

// The following two tests pin the composition of the refinery exemption
// (gt-r2xm) with the command self-filter (gt-pjeh): the exemption only
// fires for the exact feature-branch shape, and the self-filter's fail-shut
// fallback on unreadable stdin (gt-wisp-52y4) still applies under the
// refinery role — the exemption must not widen into "refinery role always
// passes."
func TestRunTapGuardPRWorkflow_RefineryUnrelatedCommandAllowed(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_REFINERY": "1",
		"GT_ROLE":     "gastown/refinery",
	}, "")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"ls -la"}}`
	err := tapGuardPRWorkflow(strings.NewReader(hookInput), io.Discard, proc)
	if err != nil {
		t.Errorf("expected unrelated command to be allowed for refinery role via self-filter, got error: %v", err)
	}
}

func TestRunTapGuardPRWorkflow_RefineryEmptyStdinStillBlocks(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_REFINERY": "1",
		"GT_ROLE":     "gastown/refinery",
	}, "")

	err := tapGuardPRWorkflow(strings.NewReader(""), io.Discard, proc)
	if err == nil {
		t.Error("expected refinery role with empty/unparsable stdin to still be blocked (fail closed), got nil error")
	}
}

// gt-hift: an io.ReadAll error returned nil straight out of the guard,
// failing it open — the one path that let a real PR command through in
// agent context, since a read error was indistinguishable from "allow" at
// the call site. An unreadable stdin is the same "we don't know what
// command this is" condition gt-wisp-52y4 gives the fail-closed fallback,
// so it must reach the unconditional context/origin check.
func TestRunTapGuardPRWorkflow_UnreadableStdinStillBlocks(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_POLECAT": "topaz",
	}, "")

	err := tapGuardPRWorkflow(unreadableReader{}, io.Discard, proc)
	if err == nil {
		t.Error("expected unreadable stdin in agent context to fail closed (block), got nil error")
	}
}

// TestEvaluatePRWorkflowGuard_UnknownInputFailsClosed pins the decision
// itself for every shape of "no command", nil included — the value
// runTapGuardPRWorkflow substitutes on a read error. Unknown input must
// fall through to the context check, and must not widen the refinery
// exemption into "refinery always passes" (gt-r2xm composed with
// gt-wisp-52y4).
func TestEvaluatePRWorkflowGuard_UnknownInputFailsClosed(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_POLECAT": "topaz",
		"GT_ROLE":    "gastown/polecats/topaz",
	}, "")

	unknown := []struct {
		name  string
		input []byte
	}{
		{"nil input (unreadable stdin)", nil},
		{"empty payload", []byte("")},
		{"payload with no command", []byte("not json")},
	}
	for _, tt := range unknown {
		t.Run(tt.name, func(t *testing.T) {
			if got := evaluatePRWorkflowGuard(tt.input, proc); got != prWorkflowBlockAgentContext {
				t.Errorf("evaluatePRWorkflowGuard(%q) = %v, want prWorkflowBlockAgentContext (unknown input fails closed)", tt.input, got)
			}
		})
	}

	t.Run("refinery role does not exempt unknown input", func(t *testing.T) {
		refinery := fakeGuardProcess(map[string]string{
			"GT_POLECAT":  "topaz",
			"GT_REFINERY": "1",
			"GT_ROLE":     "gastown/polecats/topaz",
		}, "")
		if got := evaluatePRWorkflowGuard(nil, refinery); got != prWorkflowBlockAgentContext {
			t.Errorf("evaluatePRWorkflowGuard(nil) under GT_REFINERY = %v, want prWorkflowBlockAgentContext", got)
		}
	})

	t.Run("known unrelated command is still allowed", func(t *testing.T) {
		hookInput := []byte(`{"tool_name":"Bash","tool_input":{"command":"ls -la"}}`)
		if got := evaluatePRWorkflowGuard(hookInput, proc); got != prWorkflowAllow {
			t.Errorf("evaluatePRWorkflowGuard(unrelated command) = %v, want prWorkflowAllow", got)
		}
	})
}

// gt-cyz8: the refinery exemption (gt-r2xm) was keyed on isFeatureBranchCommand,
// which matches "git checkout -b" ANYWHERE on the line, while the "gh pr create
// stays blocked" guard was anchored differently. Asymmetric matchers let a
// "gh pr create ... && git checkout -b temp" chain fire the exemption (feature
// branch found) while the PR-create guard never matched, so the exemption let
// the chained PR create through. The exemption is now keyed on
// isLeadingBranchCreation — anchored to the command's first word, exactly as
// the hook "if" glob that routes the command here is anchored — so the escape
// is closed.
func TestRunTapGuardPRWorkflow_RefineryChainedPRCreateStillBlocks(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_REFINERY": "1",
		"GT_ROLE":     "gastown/refinery",
	}, "")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"gh pr create --title foo && git checkout -b temp origin/main"}}`
	err := tapGuardPRWorkflow(strings.NewReader(hookInput), io.Discard, proc)
	if err == nil {
		t.Error("expected chained 'gh pr create && git checkout -b' to remain blocked for the refinery role, got nil error")
	}
}

// Outside an agent context the guard still blocks a PR in the maintainer's
// own repo, read through the origin remote — in either URL format — and lets
// any other origin through.
func TestEvaluatePRWorkflowGuard_MaintainerOrigin(t *testing.T) {
	t.Parallel()
	hookInput := []byte(`{"tool_name":"Bash","tool_input":{"command":"gh pr create --title foo"}}`)
	tests := []struct {
		origin string
		want   prWorkflowGuardDecision
	}{
		{"https://github.com/steveyegge/gastown.git", prWorkflowBlockMaintainerOrigin},
		{"git@github.com:steveyegge/gastown.git", prWorkflowBlockMaintainerOrigin},
		{"git@github.com:someone/gastown.git", prWorkflowAllow},
	}
	for _, tt := range tests {
		proc := fakeGuardProcess(nil, "/home/u/src/gastown")
		proc.originURL = func() (string, error) { return tt.origin, nil }
		if got := evaluatePRWorkflowGuard(hookInput, proc); got != tt.want {
			t.Errorf("evaluatePRWorkflowGuard with origin %s = %v, want %v", tt.origin, got, tt.want)
		}
	}
}
