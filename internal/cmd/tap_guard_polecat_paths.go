package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardPolecatPathsCmd = &cobra.Command{
	Use:   "polecat-paths",
	Short: "Block Edit/Write/Bash targets outside the polecat's own worktree (PreToolUse hook)",
	Long: `Prevent a polecat from touching another polecat's files (PreToolUse hook).

Blocks cross-worktree edits that corrupt another polecat's branch:
  * Edit / Write / NotebookEdit whose file_path resolves outside the
    polecat's own worktree (allowed anyway: /tmp, /private/tmp,
    ~/.claude/projects/ transcripts).
  * Bash commands whose absolute path arguments resolve under another
    polecat's directory (/Users/sloan/gt/<rig>/polecats/<other>/) or
    under the town's mayor/, deacon/, settings/ trees.

A polecat is detected by GT_POLECAT env var, or by /polecats/ in the cwd path.
The town root is GT_TOWN_ROOT, GT_ROOT, or ~/gt.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED (one-line stderr reason so the model self-corrects)

This command expects the Claude Code hook payload JSON on stdin.`,
	SilenceUsage: true,
	RunE:         runTapGuardPolecatPaths,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardPolecatPathsCmd)
}

// polecatContext identifies a polecat session and its own directory.
// Returns (rig, name, ownDir) or ("", "", "") if not a polecat context.
func polecatContext() (rig, name, ownDir string) {
	home := os.ExpandEnv("$HOME")
	town := os.Getenv("GT_TOWN_ROOT")
	if town == "" {
		town = os.Getenv("GT_ROOT")
	}
	if town == "" {
		town = home + "/gt"
	}

	rig = os.Getenv("GT_RIG")
	name = os.Getenv("GT_POLECAT")

	if rig != "" && name != "" {
		return rig, name, town + "/" + rig + "/polecats/" + name
	}

	// Fallback: detect from cwd.
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", ""
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", "", ""
	}

	// Match /rig/polecats/name pattern.
	// town is already /Users/sloan/gt (or overridden).
	prefix := town + "/"
	if !strings.HasPrefix(realCwd, prefix) {
		return "", "", ""
	}
	rest := realCwd[len(prefix):]
	// Split: rig/polecats/name/...
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 4 || parts[1] != "polecats" {
		return "", "", ""
	}
	rig = parts[0]
	name = parts[2]
	return rig, name, filepath.Join(town, rig, "polecats", name)
}

// allowedPrefixes returns path prefixes that the polecat may always touch.
func allowedPrefixes(own string) []string {
	home := os.ExpandEnv("$HOME")
	return []string{
		own + "/",        // own polecat dir
		"/tmp/",
		"/private/tmp/",
		"/private/var/folders/", // session scratchpad
		home + "/.claude/projects/",
	}
}

// isPathAllowed checks if path resolves under any allowed prefix.
func isPathAllowed(path string, own string) bool {
	p := path
	if !filepath.IsAbs(p) {
		cwd, err := os.Getwd()
		if err == nil {
			p = filepath.Join(cwd, p)
		}
	}
	realPath, err := filepath.EvalSymlinks(p)
	if err != nil {
		// Can't resolve — allow (fail open for ephemeral paths).
		return true
	}
	realPath = filepath.Clean(realPath)

	prefixes := allowedPrefixes(own)
	for _, prefix := range prefixes {
		if realPath == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(realPath, prefix) {
			return true
		}
	}
	return false
}

// isReadOnlyCommand reports whether cmd is a read-only utility (grep, cat, ls, etc.)
// that the polecat is allowed to run anywhere.
func isReadOnlyCommand(cmd string) bool {
	// Split into tokens; check the first non-flag token.
	tokens := shellTokenize(cmd)
	if len(tokens) == 0 {
		return false
	}
	// Strip leading flags (-f, --quiet, etc.)
	first := tokens[0]
	if strings.HasPrefix(first, "-") {
		for i := 1; i < len(tokens); i++ {
			if !strings.HasPrefix(tokens[i], "-") {
				first = tokens[i]
				break
			}
		}
	}
	// Also strip "sudo" prefix.
	if first == "sudo" && len(tokens) > 1 {
		first = tokens[1]
	}
	readOnly := map[string]bool{
		"grep": true, "egrep": true, "fgrep": true,
		"cat": true, "tac": true, "head": true, "tail": true,
		"ls": true, "pwd": true, "echo": true, "printf": true,
		"wc": true, "sort": true, "uniq": true, "cut": true,
		"date": true, "true": true, "false": true,
		"test": true, "[": true, "which": true, "command": true,
		"find": true, "du": true, "df": true, "stat": true,
		"file": true, "nproc": true, "uname": true,
		"jq": true, "python3": true, "python": true,
		"ruby": true, "perl": true, "node": true,
		"curl": true, "wget": true,
		"tree": true, "md5": true, "shasum": true,
		"gh": true, "bd": true, "gt": true, "git": true,
	}
	return readOnly[first]
}

// isPolecatCommand reports whether cmd is a gt/bd lifecycle command
// that the polecat should be allowed to run from anywhere.
func isPolecatCommand(cmd string) bool {
	tokens := shellTokenize(cmd)
	if len(tokens) == 0 {
		return false
	}
	first := tokens[0]
	if first == "sudo" && len(tokens) > 1 {
		first = tokens[1]
	}
	return first == "gt" || first == "bd" || first == "kill"
}

// extractFilePath extracts file_path or notebook_path from a tool input JSON.
func extractFilePath(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var hookInput struct {
		ToolName string `json:"tool_name"`
		Tool     string `json:"tool"`
		ToolInput struct {
			FilePath    string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return ""
	}
	return hookInput.ToolInput.FilePath + hookInput.ToolInput.NotebookPath
}

// extractBashCommand extracts the command from a Bash tool input JSON.
func extractBashCommand(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var hookInput struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return ""
	}
	return hookInput.ToolInput.Command
}

// extractToolName returns the tool name from the hook input.
func extractToolName(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var hookInput struct {
		ToolName string `json:"tool_name"`
		Tool     string `json:"tool"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return ""
	}
	if hookInput.ToolName != "" {
		return hookInput.ToolName
	}
	return hookInput.Tool
}

// resolveBashPaths extracts absolute or relative paths from a bash command string.
// It looks for common patterns that pass file arguments:
//   - --file=PATH, -f PATH (explicit flags)
//   - code PATH, open PATH, fzf PATH (editor/launcher commands)
//   - cat PATH, vim PATH, etc.
func resolveBashPaths(cmd string) []string {
	var paths []string
	tokens := shellTokenize(cmd)

	for i, tok := range tokens {
		// Skip flags without values.
		if strings.HasPrefix(tok, "-") && !strings.Contains(tok, "=") {
			continue
		}
		// Handle --flag=value patterns.
		if idx := strings.Index(tok, "="); idx > 0 && strings.HasPrefix(tok, "--") {
			val := tok[idx+1:]
			if filepath.IsAbs(val) || strings.HasPrefix(val, "./") || strings.HasPrefix(val, "../") {
				paths = append(paths, val)
			}
			continue
		}
		// Handle command followed by path argument.
		if i > 0 {
			prev := tokens[i-1]
			cmds := map[string]bool{
				"code": true, "open": true, "fzf": true,
				"vim": true, "vi": true, "nano": true, "emacs": true,
				"less": true, "more": true, "head": true, "tail": true,
				"cat": true, "tac": true, "diff": true, "patch": true,
				"cp": true, "mv": true, "rm": true, "ln": true,
				"mkdir": true, "install": true, "scp": true, "rsync": true,
				"tee": true, "xargs": true, "perl": true, "python": true,
				"python3": true, "node": true, "ruby": true,
				"brew": true, "npm": true, "yarn": true, "pip": true,
				"pip3": true, "go": true, "rustc": true, "cargo": true,
				"make": true, "cmake": true, "gcc": true, "g++": true,
				"clang": true, "clang++": true, "swift": true,
			}
			if cmds[prev] {
				if filepath.IsAbs(tok) || strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") {
					paths = append(paths, tok)
				}
			}
		}
	}
	return paths
}

func runTapGuardPolecatPaths(cmd *cobra.Command, args []string) error {
	// Read hook input from stdin.
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open
	}

	rig, name, own := polecatContext()
	if rig == "" || name == "" {
		return nil // not a polecat context
	}

	toolName := extractToolName(input)
	if toolName == "" {
		return nil
	}

	// Block Edit/Write/NotebookEdit outside own worktree.
	editTools := map[string]bool{
		"Edit": true, "Write": true, "NotebookEdit": true, "MultiEdit": true,
	}
	if editTools[toolName] {
		// Extract the file path from the tool input.
		if len(input) == 0 {
			return nil
		}
		var hookInput struct {
			ToolInput map[string]interface{} `json:"tool_input"`
		}
		if err := json.Unmarshal(input, &hookInput); err != nil {
			return nil
		}
		fp := ""
		if v, ok := hookInput.ToolInput["file_path"]; ok {
			if s, ok := v.(string); ok && s != "" {
				fp = s
			}
		}
		if v, ok := hookInput.ToolInput["notebook_path"]; ok {
			if s, ok := v.(string); ok && s != "" {
				fp = s
			}
		}
		if fp == "" {
			return nil
		}
		if !isPathAllowed(fp, own) {
			fmt.Fprintf(os.Stderr, "polecat-path-guard: %s target is outside your worktree. Edit only files under your own polecat directory (%s). Blocked: %s\n",
				toolName, filepath.Base(own), fp)
			return NewSilentExit(2)
		}
		return nil
	}

	// For Bash, check the command.
	if toolName == "Bash" {
		bashCmd := extractBashCommand(input)
		if bashCmd == "" {
			return nil
		}

		// Always allow read-only commands and polecat lifecycle commands.
		if isReadOnlyCommand(bashCmd) || isPolecatCommand(bashCmd) {
			return nil
		}

		// Check for absolute path arguments that resolve outside allowed dirs.
		cmdPaths := resolveBashPaths(bashCmd)
		for _, p := range cmdPaths {
			if !isPathAllowed(p, own) {
				fmt.Fprintf(os.Stderr, "polecat-path-guard: command references another polecat's directory. Work only inside your own worktree (%s). Blocked: %s\n",
					filepath.Base(own), p)
				return NewSilentExit(2)
			}
		}

		// Also check if the command itself (not just paths) references another polecat's dir.
		// This catches cases where the path is constructed inline or is relative.
		town := os.Getenv("GT_TOWN_ROOT")
		if town == "" {
			town = os.Getenv("GT_ROOT")
		}
		if town == "" {
			town = os.ExpandEnv("$HOME") + "/gt"
		}
		// Check for references to other polecats or town-level dirs.
		otherPolecatRe := strings.ReplaceAll(town, "/", `\/`) + `/([^/\s'\"]+)/polecats/([^/\s'\"]+)/`
		if matched, _ := regexp.MatchString(otherPolecatRe, bashCmd); matched {
			// Double-check it's a different polecat.
			submatch := regexp.MustCompile(otherPolecatRe).FindStringSubmatch(bashCmd)
			if len(submatch) >= 3 {
				if submatch[1] != rig || submatch[2] != name {
					fmt.Fprintf(os.Stderr, "polecat-path-guard: command references another polecat's directory (%s/polecats/%s). Work only inside your own worktree. Blocked.\n",
						submatch[1], submatch[2])
					return NewSilentExit(2)
				}
			}
		}

		// Check for references to town-level directories.
		townDirsRe := strings.ReplaceAll(town, "/", `\/`) + `/(mayor|deacon|settings)/`
		if matched, _ := regexp.MatchString(townDirsRe, bashCmd); matched {
			fmt.Fprintln(os.Stderr, "polecat-path-guard: command references the town's mayor/, deacon/ or settings/ tree. Polecats do not touch town state. Blocked.")
			return NewSilentExit(2)
		}

		return nil
	}

	return nil // unknown tool — allow
}
