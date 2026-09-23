package refinery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// This file is the refinery's pre-merge revert gate (gt-0wy03 REDESIGN): the
// single, authoritative, fail-closed check for a branch that undoes content
// already merged to its target. It is the one choke point that covers every
// path a branch reaches main by — gt done's own push, gt mq submit, a
// single-MR merge (doMerge) and a multi-MR batch (BuildRebaseStack) — since
// client-side checks (gt done's reportRevertedMerges) are warnings only now
// and gt mq submit never ran a check of its own at all.
//
// There is deliberately no relocation detection here. Every earlier attempt
// at this bead tried to distinguish a real revert of a repeated or
// boilerplate line from a harmless relocation of it, and each version either
// let a real revert through or was defeated by the next review round. A
// genuine relocation that trips this gate is rare, and the fix is a mayor
// escalation, not more detection code: the mayor writes an override file
// naming the exact branch and the reverted commits it has judged safe, and
// this gate is the only thing that reads it.

// allowRevertsOverrideDir is the mayor-only directory this gate reads
// overrides from, relative to the town root. Nothing else grants an override:
// no CLI flag, env var or bead field. Polecats cannot write here — it lies
// outside every polecat's worktree, own directory and scratch roots, so
// `gt tap guard polecat-paths` already refuses any polecat write to it.
const allowRevertsOverrideDir = "mayor/overrides/allow-reverts"

// checkRevertGate refuses mr when its submitted head (headCommit) undoes
// content already merged to origin/target. Unlike gt done's own client-side
// warning, a failure here actually blocks the merge, and an error running
// the check itself refuses too (fail closed) rather than let an unjudged
// branch through.
func (e *Engineer) checkRevertGate(mr *MRInfo, target, headCommit string) ProcessResult {
	if e.git == nil {
		return ProcessResult{Success: false, Error: "git client is missing"}
	}
	base := "origin/" + target
	found, err := git.DetectRevertedMerges(e.git, base, headCommit, headCommit)
	if err != nil {
		return ProcessResult{Success: false, Error: fmt.Sprintf(
			"cannot verify %s against %s for reverted merged work: %v — refusing rather than risk landing a revert",
			shortSHA(headCommit), base, err)}
	}
	if len(found) == 0 {
		return ProcessResult{Success: true}
	}

	override, overrideErr := loadAllowRevertsOverride(filepath.Dir(e.rig.Path), mr.Branch)
	if overrideErr != nil {
		return ProcessResult{Success: false, Error: fmt.Sprintf(
			"branch undoes work merged to %s and its allow-reverts override is unreadable: %v", target, overrideErr)}
	}
	if override != nil && override.coversAll(found) {
		note := fmt.Sprintf("allow-reverts override applied for %s: %s (reverted commit(s): %s)",
			mr.Branch, override.Reason, joinRevertedCommits(found))
		if mr.ID != "" && !e.isSyntheticMergeMechanicsMR(mr) {
			if commentErr := e.beads.AddComment(mr.ID, note); commentErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record allow-reverts override on %s: %v\n", mr.ID, commentErr)
			}
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] %s\n", note)
		return ProcessResult{Success: true}
	}

	return ProcessResult{Success: false, Error: revertGateRefusal(target, found)}
}

// revertGateRefusal builds the refusal message for a branch this gate blocks.
// It names the mayor escalation path rather than any flag or env var, because
// none exists: the only override is the mayor-authored file this gate reads.
func revertGateRefusal(target string, found []git.RevertedMerge) string {
	var b strings.Builder
	fmt.Fprintf(&b, "this branch undoes work already merged to %s\n\n", target)
	b.WriteString("Reverted commit(s):\n")
	for _, f := range found {
		fmt.Fprintf(&b, "  %s (paths: %s)\n", shortSHA(f.Commit), strings.Join(f.Paths, ", "))
	}
	b.WriteString("\nRebase onto the current target and confirm your diff carries only your own files. " +
		"If this is a genuine relocation rather than a real revert, escalate to the mayor: only a " +
		"mayor-authored override file (" + allowRevertsOverrideDir + "/<branch>) can let it through.")
	return b.String()
}

// joinRevertedCommits renders the reverted-merge evidence for an audit note.
func joinRevertedCommits(found []git.RevertedMerge) string {
	shas := make([]string, 0, len(found))
	for _, f := range found {
		shas = append(shas, shortSHA(f.Commit))
	}
	return strings.Join(shas, ", ")
}

// allowRevertsOverride is one mayor-authored override: safe to land the named
// branch despite it undoing the named commits, for the stated reason.
type allowRevertsOverride struct {
	Reason string
	SHAs   []string // as written in the file; matched by prefix against full commit SHAs.
}

// coversAll reports whether every commit DetectRevertedMerges found is named
// in the override. Partial coverage refuses: an override that does not name
// every reverted commit has not judged all of them safe.
func (o *allowRevertsOverride) coversAll(found []git.RevertedMerge) bool {
	for _, f := range found {
		if !o.covers(f.Commit) {
			return false
		}
	}
	return true
}

func (o *allowRevertsOverride) covers(commit string) bool {
	commit = strings.ToLower(commit)
	for _, sha := range o.SHAs {
		if sha != "" && strings.HasPrefix(commit, sha) {
			return true
		}
	}
	return false
}

// loadAllowRevertsOverride reads the mayor-authored override for branch, or
// returns (nil, nil) when none exists. A file that exists but cannot be
// parsed is an error, not "no override" — silently ignoring a malformed
// override file is how gt-2bp8-shaped bugs happen, and this gate fails
// closed.
func loadAllowRevertsOverride(townRoot, branch string) (*allowRevertsOverride, error) {
	if townRoot == "" || strings.TrimSpace(branch) == "" {
		return nil, nil
	}
	path := filepath.Join(townRoot, allowRevertsOverrideDir, sanitizeOverrideBranchName(branch))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseAllowRevertsOverride(data)
}

// parseAllowRevertsOverride reads the override file format: the first
// non-empty, non-comment line is the reason; every line after it is one
// commit SHA (full or shortSHA). Blank lines and lines starting with "#" are
// ignored throughout.
func parseAllowRevertsOverride(data []byte) (*allowRevertsOverride, error) {
	override := &allowRevertsOverride{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if override.Reason == "" {
			override.Reason = line
			continue
		}
		override.SHAs = append(override.SHAs, strings.ToLower(line))
	}
	if override.Reason == "" {
		return nil, fmt.Errorf("override file has no reason line")
	}
	if len(override.SHAs) == 0 {
		return nil, fmt.Errorf("override file for %q names no reverted commits", override.Reason)
	}
	return override, nil
}

// sanitizeOverrideBranchName maps a branch name to a safe filename: every
// character other than [A-Za-z0-9._+-] becomes "_", so "/" (universal in
// polecat branch names) cannot create or traverse a subdirectory.
func sanitizeOverrideBranchName(branch string) string {
	var b strings.Builder
	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '+':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	sanitized := b.String()
	if sanitized == "" || sanitized == "." || sanitized == ".." {
		return "_invalid_branch_name_"
	}
	return sanitized
}
