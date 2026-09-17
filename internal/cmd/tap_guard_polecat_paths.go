package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
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
	// inside the town but outside the polecat's own worktree. Paths outside
	// the town are allowed.
	if isFileTarget {
		if !isPathInPolecatWorktree(targetPath, polecatWorktreeRoot) {
			// Outside the worktree — only block if inside the town.
			if townRoot != "" && isPathWithinTown(targetPath, townRoot) {
				printPolecatPathsBlock(targetPath, "file path outside the polecat's own worktree")
				return NewSilentExit(2)
			}
			return nil
		}
		return nil
	}

	// For Bash: only check paths when the command is write-capable.
	if toolName != "" && !writeCapableTools[toolName] {
		return nil
	}

	// Resolve the worktree root for path comparison (case-insensitive on macOS).
	// Resolve through symlinks so the worktree root matches resolved target paths.
	worktreeResolved := polecatWorktreeRoot
	if resolved, err := filepath.EvalSymlinks(polecatWorktreeRoot); err == nil {
		worktreeResolved = resolved
	}
	worktreeLower := strings.ToLower(filepath.Clean(worktreeResolved))

	// Collect and resolve all path arguments from the Bash command.
	if toolName != "" {
		targets := extractBashTargets(input)
		for _, raw := range targets {
			resolved := expandAndResolvePath(raw)
			if resolved == "" {
				continue
			}
			resolvedClean := filepath.Clean(resolved)

			// Inside the polecat's own worktree — allowed.
			if isPathWithin(resolvedClean, worktreeLower) {
				continue
			}

			// Outside the worktree — check if it's a blocked town path.
			if isBlockedTownPath(resolvedClean, townRoot, worktreeResolved) {
				printPolecatPathsBlock(raw, "target is a sibling worktree or restricted town directory")
				return NewSilentExit(2)
			}
		}
	}

	return nil
}

// extractToolInfo parses the hook input JSON and returns:
//
//	toolName: the tool name (Bash, Edit, Write, NotebookEdit, MultiEdit)
//	targetPath: the file/notebook path for file-targeting tools, "" for Bash
//	isFileTarget: true if the tool targets a file path
func extractToolInfo(input []byte) (toolName, targetPath string, isFileTarget bool) {
	if len(input) == 0 {
		return "", "", false
	}
	var hookInput struct {
		ToolName string `json:"tool_name"`
		ToolInput struct {
			Command      string `json:"command"`
			FilePath     string `json:"file_path"`
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
		// Extract file paths from Python open() calls: open('path') or open("path").
		if path := extractOpenPath(tok); path != "" {
			targets = append(targets, path)
			continue
		}
		targets = append(targets, tok)
	}
	return targets
}

// extractOpenPath checks if a token is a Python open() call argument and
// returns the file path inside the quotes, or "" if not an open() call.
func extractOpenPath(tok string) string {
	const prefix = "open("
	if !strings.HasPrefix(tok, prefix) {
		return ""
	}
	inner := strings.TrimPrefix(tok, prefix)
	// Strip trailing paren/paren-comma if present.
	inner = strings.TrimSuffix(inner, ")")
	inner = strings.TrimSuffix(inner, "),")
	// Extract the first quoted string.
	if len(inner) < 2 {
		return ""
	}
	quote := inner[0]
	if quote != '\'' && quote != '"' {
		return ""
	}
	end := strings.IndexByte(inner[1:], quote)
	if end < 0 {
		return ""
	}
	return inner[1 : 1+end]
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
// own worktree root. It resolves symlinks on both target and worktree so
// that temp-dir symlinks (/var/folders/... vs /private/var/folders/...)
// compare correctly. Fails CLOSED on error.
func isPathInPolecatWorktree(targetPath, worktreeRoot string) bool {
	if targetPath == "" || worktreeRoot == "" {
		return false
	}

	targetClean := filepath.Clean(targetPath)

	// Resolve the worktree root through symlinks.
	worktreeResolved := worktreeRoot
	if resolved, err := filepath.EvalSymlinks(worktreeRoot); err == nil {
		worktreeResolved = resolved
	}

	// Try resolving symlinks; fail closed on error.
	resolved, err := filepath.EvalSymlinks(targetPath)
	if err == nil {
		return isPathWithinCaseInsensitive(resolved, worktreeResolved)
	}

	// Path doesn't exist — check its parent directory.
	parent := filepath.Dir(targetClean)
	if parent != "" && parent != targetClean {
		if _, statErr := os.Stat(parent); statErr == nil {
			resolvedParent, err := filepath.EvalSymlinks(parent)
			if err == nil {
				return isPathWithinCaseInsensitive(resolvedParent, worktreeResolved)
			}
		}
	}

	// Parent doesn't exist either — use path-prefix check on the given path.
	return isPathWithinCaseInsensitive(targetClean, worktreeResolved)
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

// isPathWithinCaseInsensitive reports whether target is strictly inside ref,
// comparing both paths case-insensitively. This avoids the pitfall of
// lowercasing only ref: on macOS, /T/ in target vs /t/ in lowercased ref
// makes filepath.Rel produce "../..." escapes that falsely report "outside".
// Both paths are lowercased so filepath.Rel sees identical case and produces
// a clean relative path.
func isPathWithinCaseInsensitive(target, ref string) bool {
	if ref == "" {
		return false
	}
	rel, err := filepath.Rel(strings.ToLower(ref), strings.ToLower(target))
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

	townLower := strings.ToLower(filepath.Clean(townRoot))
	targetClean := strings.ToLower(filepath.Clean(target))

	// The polecat's own worktree is always allowed.
	ownWorktree := strings.ToLower(filepath.Clean(polecatWorktreeRoot))
	if ownWorktree != "" && (targetClean == ownWorktree ||
		strings.HasPrefix(targetClean, ownWorktree+string(filepath.Separator))) {
		return false
	}

	// Outside the town entirely — allowed.
	rel, err := filepath.Rel(townLower, targetClean)
	if err != nil || !isWithinRel(rel) {
		return false
	}

	// Rig/town root itself is blocked.
	if targetClean == townLower {
		return true
	}

	// Paths directly inside a rig root (one level below town) are blocked.
	if relParts := strings.Split(rel, string(filepath.Separator)); len(relParts) == 1 {
		return true
	}

	// Paths inside a rig root directory (any depth) are blocked — the rig
	// root is the first component below town, and anything under it is a rig.
	if relParts := strings.Split(rel, string(filepath.Separator)); len(relParts) >= 1 {
		rigRoot := relParts[0]
		if strings.HasPrefix(rel, rigRoot+string(filepath.Separator)) {
			return true
		}
	}

	// Paths inside a rig directory are blocked (rig roots are the town's
	// biggest single trees, so anything inside one is hazardous).
	if firstRel := strings.Split(rel, string(filepath.Separator)); len(firstRel) >= 1 {
		if isRestrictedTownLevel(firstRel[0]) {
			return true
		}
	}

	// Restricted town-level directories.
	base := filepath.Base(targetClean)
	if isRestrictedTownLevel(base) {
		return true
	}

	// Sibling polecat worktrees: any /polecats/<name> at any depth.
	if strings.Contains(rel, string(filepath.Separator)+"polecats"+string(filepath.Separator)) {
		return true
	}

	return false
}

// isPathWithinTown reports whether targetPath is strictly inside the town
// rooted at townRoot. Returns false when the path is outside the town.
func isPathWithinTown(targetPath, townRoot string) bool {
	if townRoot == "" {
		return false
	}
	targetClean := filepath.Clean(targetPath)
	townClean := filepath.Clean(townRoot)
	rel, err := filepath.Rel(townClean, targetClean)
	if err != nil {
		return false
	}
	return isWithinRel(rel)
}

// isRestrictedTownLevel reports whether a path component names a restricted
// town-level directory (rig root, mayor, deacon, settings, logs, .dolt-data).
func isRestrictedTownLevel(name string) bool {
	return name == "mayor" || name == "deacon" || name == "settings" ||
		name == "logs" || name == ".dolt-data"
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

