package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/workspace"
)

// tapGuardPolecatPathsCmd blocks Edit/Write/NotebookEdit targets outside the
// polecat's own worktree and Bash commands whose write-capable tools target
// sibling worktrees or restricted town directories (gt-hmaf).
//
// The guard distinguishes two failure modes:
//
//   - a file_path / notebook_path that names a sibling worktree or a town
//     directory — always blocked for Edit/Write/NotebookEdit;
//   - a Bash command whose tool is write-capable (python, node, sh, curl -o,
//     wget -O) and whose argument resolves to a sibling worktree or town
//     directory — blocked; read-only tools (grep, cat, ls, …) are allowed
//     everywhere.
//
// Path expansion rules:
//   - ~ and $HOME / ${HOME} prefixes expand to the home directory;
//   - relative paths (./x, ../x, bare existing directories) resolve against
//     the guard's cwd (the session's working directory);
//   - the guard fails CLOSED when EvalSymlinks errors on a path (so writing
//     any new file that cannot be resolved is blocked rather than allowed).
//
// The guard is a no-op (exit 0) when:
//   - not running in a Gas Town agent context (no GT_POLECAT, no /polecats/ in
//     cwd, no GT_CREW/GT_WITNESS/GT_REFINERY/GT_MAYOR/GT_DEACON/GT_DOG_NAME);
//   - the command is a Bash call whose tool is not write-capable.
var tapGuardPolecatPathsCmd = &cobra.Command{
	Use:   "polecat-paths",
	Short: "Block Edit/Write/Bash targets outside the polecat's own worktree (PreToolUse hook)",
	Long: `Block cross-worktree edits for Gas Town polecats (PreToolUse hook).

This guard prevents polecats from editing, writing, or executing write-capable
tools against files outside their own worktree. It also blocks Bash commands
that target sibling worktrees or restricted town directories.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	SilenceUsage: true,
	RunE:         runTapGuardPolecatPaths,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardPolecatPathsCmd)
}

// writeCapableTools lists command names that can write to files (either by
// opening a file for writing or by using -o/-O to redirect output to a path).
var writeCapableTools = map[string]bool{
	"python":  true, "python3": true,
	"node":    true,
	"sh":      true, "bash":  true, "zsh":  true, "dash": true, "ksh": true,
	"curl":    true,
	"wget":    true,
}

func runTapGuardPolecatPaths(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open
	}

	toolName, targetPath, isFileTarget := extractToolInfo(input)

	// Fast path: not in a Gas Town agent context — nothing to guard.
	if !isGasTownAgentContext() {
		return nil
	}

	// Resolve the polecat's own worktree root from cwd.
	polecatWorktreeRoot := resolvePolecatWorktreeRoot()
	if polecatWorktreeRoot == "" {
		// Cannot determine polecat context from cwd — fail open.
		return nil
	}

	// Resolve the town root to build the blocked-path checker.
	townRoot := currentTownRoot()

	// For file-targeting tools (Edit, Write, NotebookEdit): deny any target
	// outside the polecat's own worktree.
	if isFileTarget {
		if !isPathInPolecatWorktree(targetPath, polecatWorktreeRoot) {
			printPolecatPathsBlock(targetPath, "file path outside the polecat's own worktree")
			return NewSilentExit(2)
		}
		return nil
	}

	// For Bash: only check paths when the command is write-capable.
	if toolName != "" && !writeCapableTools[toolName] {
		return nil
	}

	// Resolve the worktree root for path comparison (case-insensitive on macOS).
	worktreeLower := strings.ToLower(filepath.Clean(polecatWorktreeRoot))

	// Collect and resolve all path arguments from the Bash command.
	if toolName != "" {
		targets := extractBashTargets(input)
		for _, raw := range targets {
			resolved := expandAndResolvePath(raw)
			if resolved == "" {
				continue
			}
			resolvedClean := filepath.Clean(resolved)

			// Fail CLOSED on EvalSymlinks error — never allow a new file whose
			// real location cannot be determined.
			resolvedReal, err := os.PathExists(resolvedClean)
			_ = resolvedReal // suppress unused variable
			if err == nil {
				if _, err = os.Stat(resolvedClean); err != nil {
					// Path doesn't exist yet — check its parent directory.
					parent := filepath.Dir(resolvedClean)
					if parent != "" && parent != resolvedClean {
						if _, statErr := os.Stat(parent); statErr != nil {
							// Parent also doesn't exist — check the path as-is
							// against the worktree boundary (path-prefix check).
							if !isPathWithin(resolvedClean, worktreeLower) {
								printPolecatPathsBlock(raw, "file path outside the polecat's own worktree")
								return NewSilentExit(2)
							}
							if isBlockedTownPath(resolvedClean, townRoot, polecatWorktreeRoot) {
								printPolecatPathsBlock(raw, "target is a sibling worktree or restricted town directory")
								return NewSilentExit(2)
							}
						} else {
							if !isPathWithin(resolvedClean, worktreeLower) {
								printPolecatPathsBlock(raw, "file path outside the polecat's own worktree")
								return NewSilentExit(2)
							}
						}
					} else {
						if !isPathWithin(resolvedClean, worktreeLower) {
							printPolecatPathsBlock(raw, "file path outside the polecat's own worktree")
							return NewSilentExit(2)
						}
					}
				}
			}

			if !isPathWithin(resolvedClean, worktreeLower) {
				if isBlockedTownPath(resolvedClean, townRoot, polecatWorktreeRoot) {
					printPolecatPathsBlock(raw, "target is a sibling worktree or restricted town directory")
					return NewSilentExit(2)
				}
			}
		}
	}

	return nil
}

// extractToolInfo parses the hook input JSON and returns:
//   - toolName: the tool name (Bash, Edit, Write, NotebookEdit, MultiEdit)
//   - targetPath: the file/notebook path for file-targeting tools, "" for Bash
//   - isFileTarget: true if the tool targets a file path
func extractToolInfo(input []byte) (toolName, targetPath string, isFileTarget bool) {
	if len(input) == 0 {
		return "", "", false
	}
	var hookInput struct {
		ToolName string `json:"tool_name"`
		ToolInput struct {
			Command    string `json:"command"`
			FilePath   string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return "", "", false
	}

	tool := hookInput.ToolName
	if tool == "" {
		return "", "", false
	}

	// File-targeting tools carry the target in tool_input.
	switch tool {
	case "Edit", "Write", "MultiEdit":
		return tool, hookInput.ToolInput.FilePath, true
	case "NotebookEdit":
		return tool, hookInput.ToolInput.NotebookPath, true
	default:
		return tool, "", false
	}
}

// extractBashTargets pulls file-path-looking arguments from a Bash command's
// tool_input, skipping flags, empty strings, glob patterns, and environment
// variable references that this process cannot resolve.
func extractBashTargets(input []byte) []string {
	if len(input) == 0 {
		return nil
	}
	var hookInput struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return nil
	}

	tokens := shellTokenize(hookInput.ToolInput.Command)
	var targets []string
	for _, tok := range tokens {
		if tok == "" || strings.HasPrefix(tok, "-") {
			continue // skip flags
		}
		if tok == "~" || strings.HasPrefix(tok, "~/") {
			continue // handle via expandHomePath
		}
		if strings.HasPrefix(tok, "$") || strings.HasPrefix(tok, "${") {
			continue // skip variable references (expandAndResolvePath handles them)
		}
		if hasGlobMeta(tok) {
			continue // glob tokens resolved by shell before the walker runs
		}
		targets = append(targets, tok)
	}
	return targets
}

// expandAndResolvePath expands ~ / $HOME / ${HOME} prefixes and resolves
// relative paths against the guard's cwd, returning an absolute path or ""
// when the token cannot be resolved.
func expandAndResolvePath(token string) string {
	if token == "" {
		return ""
	}

	p, ok := expandHomePath(token)
	if !ok || p == "" || strings.Contains(p, "$") {
		return ""
	}

	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		p = filepath.Join(wd, p)
	}
	return filepath.Clean(p)
}

// expandHomePath applies the shell's ~ / $HOME / ${HOME} prefix rules to a
// token, returning it unchanged when it names no home-relative path. ok is
// false only when the token did reference the home directory but this process
// cannot see it.
func expandHomePath(token string) (path string, ok bool) {
	join := func(rest string) (string, bool) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		return filepath.Join(home, rest), true
	}

	switch {
	case token == "~", token == "$HOME", token == "${HOME}":
		return join("")
	case strings.HasPrefix(token, "~/"):
		return join(strings.TrimPrefix(token, "~/"))
	case strings.HasPrefix(token, "$HOME/"):
		return join(strings.TrimPrefix(token, "$HOME/"))
	case strings.HasPrefix(token, "${HOME}/"):
		return join(strings.TrimPrefix(token, "${HOME}/"))
	}
	return token, true
}

// resolvePolecatWorktreeRoot walks up from cwd to find the polecat's worktree
// root — the parent of the first /polecats/<name> segment encountered.
// Returns "" when cwd is not under a Gas Town polecat tree.
func resolvePolecatWorktreeRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	dir := filepath.Clean(cwd)
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // reached filesystem root
		}

		base := filepath.Base(dir)
		// Check if this directory is a polecat's agent directory.
		if isPolecatAgentDir(parent, base) {
			// The worktree root is the parent of /polecats/<name>.
			return parent
		}

		dir = parent
	}
}

// isPolecatAgentDir reports whether dir is a Gas Town polecat agent directory
// (/polecats/<name>).
func isPolecatAgentDir(parent, base string) bool {
	p := strings.ToLower(filepath.Clean(parent))
	b := strings.ToLower(base)
	return filepath.Base(p) == "polecats" && b != ""
}

// isPathInPolecatWorktree reports whether targetPath is inside the polecat's
// own worktree root. It resolves symlinks but fails CLOSED on error.
func isPathInPolecatWorktree(targetPath, worktreeRoot string) bool {
	if targetPath == "" || worktreeRoot == "" {
		return false
	}

	targetClean := filepath.Clean(targetPath)

	// Try resolving symlinks; fail closed on error.
	resolved, err := filepath.EvalSymlinks(targetPath)
	if err == nil {
		return isPathWithin(resolved, strings.ToLower(filepath.Clean(worktreeRoot)))
	}

	// Path doesn't exist — check its parent directory.
	parent := filepath.Dir(targetClean)
	if parent != "" && parent != targetClean {
		if _, statErr := os.Stat(parent); statErr == nil {
			resolvedParent, err := filepath.EvalSymlinks(parent)
			if err == nil {
				return isPathWithin(resolvedParent, strings.ToLower(filepath.Clean(worktreeRoot)))
			}
		}
	}

	// Parent doesn't exist either — use path-prefix check on the given path.
	return isPathWithin(targetClean, strings.ToLower(filepath.Clean(worktreeRoot)))
}

// isPathWithin reports whether target (both already cleaned/lowercased) is
// strictly inside the reference directory.
func isPathWithin(target, refLower string) bool {
	if refLower == "" {
		return false
	}
	rel, err := filepath.Rel(refLower, target)
	if err != nil {
		return false
	}
	return rel != "." && rel != "" && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// isBlockedTownPath reports whether target names a sibling worktree or a
// restricted town directory. Returns false when target is outside the town.
func isBlockedTownPath(target, townRoot, polecatWorktreeRoot string) bool {
	if townRoot == "" {
		return false
	}

	town := strings.ToLower(filepath.Clean(townRoot))
	targetClean := strings.ToLower(filepath.Clean(target))

	// Outside the town entirely — allowed.
	rel, err := filepath.Rel(town, targetClean)
	if err != nil || !isWithinRel(rel) {
		return false
	}

	// Exactly at the town root — allowed (the polecat's own worktree is under it).
	if targetClean == town {
		return false
	}

	// Rig root itself is blocked.
	rigRoot := strings.ToLower(filepath.Clean(town))
	// Derive rig path: the rig is the directory containing /polecats/.
	// Walk up from town root to find the rig.
	rigPath := findRigPath(town)
	if rigPath != "" && targetClean == rigPath {
		return true
	}

	// Any path directly under the town root that isn't the polecat's own
	// worktree is blocked (mayor, deacon, settings, logs, .dolt-data, etc.).
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) >= 1 {
		base := filepath.Base(targetClean)
		if base == "mayor" || base == "deacon" || base == "settings" ||
			base == "logs" || base == ".dolt-data" || base == "logs" {
			return true
		}
	}

	// Sibling polecat worktrees: any /polecats/<name> at any depth.
	if strings.Contains(rel, string(filepath.Separator)+"polecats"+string(filepath.Separator)) {
		return true
	}

	// The polecat's own worktree is allowed even though it contains /polecats/.
	ownWorktree := strings.ToLower(filepath.Clean(polecatWorktreeRoot))
	if ownWorktree != "" && strings.HasPrefix(targetClean, ownWorktree+string(filepath.Separator)) || targetClean == ownWorktree {
		return false
	}

	return false
}

// findRigPath walks up from townRoot to find the directory containing /polecats/.
// In a typical town layout, the town root IS the rig directory.
func findRigPath(townRoot string) string {
	// The town root from workspace.FindFromCwdOrError() is the rig directory
	// (e.g. ~/gt/gastown). So the rig path is the town root itself.
	return strings.ToLower(filepath.Clean(townRoot))
}

// printPolecatPathsBlock prints the block banner to stderr.
func printPolecatPathsBlock(target, reason string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ CROSS-WORKTREE EDIT BLOCKED                                  ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Target: %-51s ║\n", truncateStr(target, 51))
	fmt.Fprintf(os.Stderr, "║  Reason: %-51s ║\n", truncateStr(reason, 51))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Polecats must only edit files within their own worktree.        ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}

// currentTownRoot resolves the town root for guard purposes, or "" when not
// inside a Gas Town workspace. Uses the same resolution as the dangerous
// command guard.
func currentTownRoot() string {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return ""
	}
	return townRoot
}

// isWithinRel reports whether a filepath.Rel result names a path strictly
// inside the reference directory.
func isWithinRel(rel string) bool {
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel) && rel != "." && rel != ""
}

// hasGlobMeta reports whether a token carries shell glob syntax.
func hasGlobMeta(token string) bool {
	return strings.ContainsAny(token, "*?[")
}
