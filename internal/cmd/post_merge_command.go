package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/util"
)

// postMergeCommandParams describes one run of a rig's post_merge_command.
type postMergeCommandParams struct {
	RigName   string
	TownRoot  string
	WorkDir   string
	MergedSHA string
	Command   string
	MRIDs     []string
	Timeout   time.Duration
	Output    io.Writer
}

// Test seams: swapped by tests, the real implementations in production.
var (
	postMergeCommandFn = runPostMergeCommand
	postMergeEscalate  = escalatePostMergeCommand
)

// runPostMergeCommand runs the rig's post-merge command best-effort. The merge
// has already landed, so nothing here may fail it: a nonzero exit or timeout
// is escalated and swallowed.
func runPostMergeCommand(p postMergeCommandParams) {
	if strings.TrimSpace(p.Command) == "" {
		return
	}
	out := p.Output
	if out == nil {
		out = os.Stdout
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = config.DefaultPostMergeTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", p.Command)
	cmd.Dir = p.WorkDir
	cmd.Env = append(os.Environ(),
		"GT_MERGED_SHA="+p.MergedSHA,
		"GT_RIG="+p.RigName,
		"GT_TOWN_ROOT="+p.TownRoot,
		"GT_MR_IDS="+strings.Join(p.MRIDs, ","),
	)
	cmd.Stdout = out
	cmd.Stderr = out
	// Own process group so a timeout reaches make/go children, not just bash.
	util.SetProcessGroup(cmd)
	// A killed grandchild can hold the output pipe open; don't wait on it forever.
	cmd.WaitDelay = 5 * time.Second

	fmt.Fprintf(out, "post-merge command (%s @ %s): %s\n", p.RigName, p.MergedSHA, p.Command)
	start := time.Now()
	err := cmd.Run()
	if err == nil {
		fmt.Fprintf(out, "%s post-merge command finished in %s\n", style.Bold.Render("✓"), time.Since(start).Round(time.Second))
		return
	}
	reason := err.Error()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = fmt.Sprintf("timed out after %s", timeout)
	}
	style.PrintWarning("post-merge command failed (%s); the merge stands", reason)
	postMergeEscalate(p.RigName, fmt.Sprintf("post-merge command failed on %s at %s: %s (command: %s)", p.RigName, p.MergedSHA, reason, p.Command))
}

// escalatePostMergeCommand notifies the operator that a rig's post-merge
// command failed. Best-effort, like escalateRubricChange: the merge landed.
func escalatePostMergeCommand(rigName, msg string) {
	cmd := exec.Command("gt", "escalate",
		"--severity", "medium",
		"--reason", "post-merge-command",
		"--source", "refinery:post-merge",
		"--fingerprint", "post-merge-command:"+rigName,
		msg)
	if err := cmd.Run(); err != nil {
		style.PrintWarning("post-merge command escalation failed: %v", err)
	}
}

// postMergeCommandFor builds the run for a rig's configured post-merge
// command. ok is false when the rig configures none. The command runs in the
// refinery's worktree so the script is the merged version; rigPath itself is
// the rig root, which has no scripts/.
func postMergeCommandFor(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, mergedSHA, target string, mrIDs []string, out io.Writer) (postMergeCommandParams, bool) {
	if mq == nil || strings.TrimSpace(mq.PostMergeCommand) == "" {
		return postMergeCommandParams{}, false
	}
	workDir := filepath.Join(rigPath, "refinery", "rig")
	return postMergeCommandParams{
		RigName:   rigName,
		TownRoot:  townRoot,
		WorkDir:   workDir,
		MergedSHA: resolvePostMergeSHA(workDir, mergedSHA, target),
		Command:   mq.PostMergeCommand,
		MRIDs:     mrIDs,
		Timeout:   mq.GetPostMergeTimeout(),
		Output:    out,
	}, true
}

// resolvePostMergeSHA prefers the recorded merge commit and falls back to the
// target's remote tip. Empty when neither resolves; the command decides.
func resolvePostMergeSHA(workDir, mergedSHA, target string) string {
	if s := strings.TrimSpace(mergedSHA); s != "" {
		return s
	}
	if target == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", workDir, "rev-parse", "--verify", "origin/"+target).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runMRPostMergeCommand runs the post-merge command for one landed MR.
func runMRPostMergeCommand(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, mr *refinery.MergeRequest, out io.Writer) {
	if mr == nil {
		return
	}
	if p, ok := postMergeCommandFor(townRoot, rigName, rigPath, mq, mr.MergeCommit, mr.TargetBranch, []string{mr.ID}, out); ok {
		postMergeCommandFn(p)
	}
}

// runBatchPostMergeCommand runs the post-merge command once for a landed
// batch, at the batch's final pushed SHA. A batch whose push landed runs it
// even when some members' cleanup failed; a batch that pushed nothing doesn't.
func runBatchPostMergeCommand(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, result *refinery.BatchResult, target string, out io.Writer) {
	if result == nil || strings.TrimSpace(result.MergeCommit) == "" {
		return
	}
	ids := make([]string, 0, len(result.Merged))
	for _, mr := range result.Merged {
		if mr != nil {
			ids = append(ids, mr.ID)
		}
	}
	if p, ok := postMergeCommandFor(townRoot, rigName, rigPath, mq, result.MergeCommit, target, ids, out); ok {
		postMergeCommandFn(p)
	}
}
