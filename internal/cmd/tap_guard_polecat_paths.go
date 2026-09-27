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

// tap_guard_polecat_paths.go implements `gt tap guard polecat-paths` (gt-hmaf).
//
// The incident: on 2026-09-16 a polecat (obsidian, gt-8yx2) edited
// internal/cmd/rig_config.go inside CORAL's worktree instead of its own. A
// cross-worktree edit corrupts another polecat's branch in a way its owner
// cannot see, and cheaper models do it more often — the same lack of path
// discipline showed up across ~/gt in gt-6e2l's root scans. The watcher
// (tools/polecat-trial.py) catches it after the fact and rolls the whole rig
// back to the fallback model; this guard prevents it.
//
// Design rules, in the order they matter:
//
//  1. Scope first. The guard is a no-op unless it can positively identify the
//     polecat whose paths it protects (GT_POLECAT plus a worktree path). It
//     never blocks anything in a non-polecat session.
//  2. Canonicalise before comparing. Every target is expanded (~, $HOME, and
//     any other variable this process can see), resolved against the session
//     cwd, and symlink-resolved through its longest existing prefix — so a
//     file that does not exist yet still resolves through its parent. Compare
//     by whole path components with a trailing separator, so /worktree-evil
//     never matches /worktree.
//  3. Fail CLOSED. A target the guard is asked to check but cannot resolve is
//     DENIED, never allowed: an unknown variable or a missing ancestor means
//     "we do not know what this writes to", and guessing is what the guard
//     exists to prevent. (The guard's own derived directories are not
//     attacker-controlled, so those fall back to a cleaned path instead.)
//  4. Check writes, not reads. Read-only commands (grep/cat/ls) stay allowed
//     anywhere. For Bash the deny set is deliberately narrow — paths inside
//     the town that are not the polecat's own — so that a command's ordinary
//     paths (/dev/null, /tmp, the worktree, URLs) never trip it.
//     Edit/Write/NotebookEdit are stricter: only the worktree, temp
//     directories and the session scratchpad are writable at all.
//  5. Judge the parse, not the spelling. Bash arguments are checked from a
//     tokenised command with heredoc bodies stripped, redirection targets read
//     off the raw line, command substitutions recursed into, and both
//     `--flag=/path` values and the path-shaped runs inside an interpreter's
//     -c/-e payload treated as targets. Heredoc bodies are data on stdin, so a
//     commit message or mail body that merely *mentions* a sibling worktree
//     must not trip the guard.
//
// What this guard deliberately does not attempt: reading file contents (a
// script written inside the worktree and then executed can still reach
// anywhere), and a complete shell parse. It is a seat belt, not a cage — the
// tool-trial watcher remains the backstop for hazards it misses.
const (
	// maxSubstitutionDepth bounds recursion into $( ) / ` ` payloads.
	maxSubstitutionDepth = 3

	// sessionScratchDir is the transcripts/scratch root Gas Town gives agent
	// sessions inside the town (the Claude config dir they run under).
	sessionScratchDir = ".claude-town"
)

var tapGuardPolecatPathsCmd = &cobra.Command{
	Use:   "polecat-paths",
	Short: "Block Edit/Write/Bash targets outside the polecat's own worktree",
	Long: `Block cross-worktree writes by Gas Town polecats (Claude Code PreToolUse hook).

A polecat works in exactly one worktree ("Directory Discipline" in the polecat
context doc). A polecat that edits a sibling's worktree corrupts a branch its
owner cannot see, which is how gt-hmaf's incident happened.

  - Edit / Write / MultiEdit / NotebookEdit are denied when the resolved
    file_path (or notebook_path) is outside the polecat's own worktree. Temp
    directories and the session scratchpad stay writable.
  - Bash (and Monitor, which carries the same tool_input.command shape as a
    background/streaming watch) is denied when a write-capable command
    (cp/mv/rm/mkdir/tee/chmod/..., an interpreter, curl -o/wget), a shell write
    redirection, a cd, or a git -C target names a path inside the town that is
    not the polecat's own worktree, its own polecat directory, or its rig's
    .repo.git. Read-only commands (grep/cat/ls) are allowed anywhere.

Paths are expanded (~, $HOME, and any variable this process can see), resolved
against the session cwd, symlink-resolved through their longest existing
prefix, and compared by whole path components. A target that cannot be
resolved is DENIED — the guard fails closed rather than guessing.

Exit codes:
  0 - Operation allowed (also: not a polecat session, nothing to guard)
  2 - Operation BLOCKED (one-line reason on stderr, so the model self-corrects)`,
	// The block reason is the whole message: a usage dump after it (the other
	// hook guards that return NewSilentExit(2) do the same) buries the line the
	// model needs to see.
	SilenceUsage: true,
	RunE:         runTapGuardPolecatPaths,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardPolecatPathsCmd)
}

// polecatPathsInput is the subset of the Claude Code PreToolUse payload this
// guard reads. cwd is the session's (shell's) working directory, which is what
// relative targets resolve against.
type polecatPathsInput struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

func runTapGuardPolecatPaths(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil || len(input) == 0 {
		// No payload to judge. Some harness wrappers drain stdin before
		// invoking a guard (gt-wisp-52y4's Copilot template does), and denying
		// every call on an empty payload would wedge the session — the guard
		// reports nothing it can substantiate rather than blocking blind.
		return nil
	}
	var hook polecatPathsInput
	if err := json.Unmarshal(input, &hook); err != nil {
		return nil
	}
	scope, ok := resolvePolecatPathScope(hook.Cwd)
	if !ok {
		return nil // not a polecat session — outside this guard's remit
	}
	reason := scope.evaluate(hook)
	if reason == "" {
		return nil
	}
	fmt.Fprintf(os.Stderr, "polecat-paths: %s\n", reason)
	return NewSilentExit(2)
}

// polecatPathScope is the set of paths the guarded polecat may write to,
// resolved once per hook invocation.
type polecatPathScope struct {
	worktree string // canonical own git worktree (the session's cwd)
	ownDir   string // canonical <town>/<rig>/polecats/<name>
	repoGit  string // canonical <town>/<rig>/.repo.git
	townRoot string // canonical town root
	cwd      string // canonical directory relative targets resolve against
	scratch  []string
}

// evaluate reports why the hook payload must be blocked, or "" to allow it.
func (s polecatPathScope) evaluate(hook polecatPathsInput) string {
	switch hook.ToolName {
	case "Edit", "Write", "MultiEdit":
		return s.checkFileTarget(hook.ToolInput.FilePath, hook.ToolName)
	case "NotebookEdit":
		target := hook.ToolInput.NotebookPath
		if target == "" {
			target = hook.ToolInput.FilePath
		}
		return s.checkFileTarget(target, hook.ToolName)
	case "Bash", "Monitor":
		// Monitor runs the same command shape as Bash (a shell command in
		// tool_input.command) as a background/streaming watch, and was
		// invisible to this guard until this case existed — the matcher
		// alone (shellExecutingToolMatcher) only routes the call here, it
		// doesn't teach the guard the tool's shape (gt-vx2mm).
		return s.checkBashCommand(hook.ToolInput.Command, 0)
	default:
		// Anything else that can write (an MCP filesystem tool, a future
		// Claude Code tool) is not understood here; the guard stays silent
		// rather than denying calls it cannot reason about.
		return ""
	}
}

// checkFileTarget decides an Edit/Write/MultiEdit/NotebookEdit call: the
// target must live inside the polecat's own worktree, a temp directory, or the
// session scratchpad.
func (s polecatPathScope) checkFileTarget(raw, tool string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	target, ok := canonicalizeToolPath(raw, s.cwd)
	if !ok {
		return fmt.Sprintf("%s target %q cannot be resolved (unknown variable or missing parent) — refusing to guess. Write inside your own worktree: %s", tool, raw, s.worktree)
	}
	if s.isWorktreePath(target) || s.isScratchPath(target) {
		return ""
	}
	return fmt.Sprintf("%s target is outside your worktree: %s (yours: %s). Polecats edit only their own worktree; use /tmp for scratch files.", tool, target, s.worktree)
}

// checkBashCommand reports why a Bash command must be blocked, or "". depth
// bounds recursion into command substitutions.
func (s polecatPathScope) checkBashCommand(command string, depth int) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	if depth > maxSubstitutionDepth {
		return "nested command substitutions are too deep to check — run the command in a form this guard can read"
	}

	// Heredoc bodies are data on stdin, not paths: the shared stripper (see
	// tap_guard_dangerous.go) removes them before scanning, so a commit
	// message, mail body or README quote that happens to mention a sibling
	// worktree cannot trip the guard. A heredoc still counts as uncheckable
	// *input* when its reader is an interpreter (see below).
	stripped := stripHeredocBodies(command)
	// A heredoc that matters here has a body, which means the command spans
	// more than one line: the shared pattern also matches a bit-shift
	// expression inside an unquoted -c payload ("x << n"), and those are code,
	// not stdin.
	heredoc := strings.Contains(command, "\n") && heredocStartPattern.MatchString(command)

	segments := splitShellSegments(shellTokenize(stripped))
	for _, segment := range segments {
		word, args := segmentCommandWord(segment)
		if word == "" {
			continue
		}
		base := filepath.Base(word)
		switch {
		case isInterpreterCommand(base):
			if heredoc {
				return "a script fed to " + base + " on stdin (heredoc) cannot be inspected — write the script inside your worktree and run that file instead"
			}
			if reason := s.checkBashArgs(args, base); reason != "" {
				return reason
			}
		case isWriteCapableCommand(base, args):
			if reason := s.checkBashArgs(args, base); reason != "" {
				return reason
			}
		case base == "cd" || base == "pushd":
			// cd is not a write, but it decides where every later relative path
			// lands: a polecat that walks into a sibling worktree (or the
			// mayor/deacon/settings trees) is one careless relative write away
			// from corrupting them.
			if reason := s.checkBashArgs(args, base); reason != "" {
				return reason
			}
		case base == "git":
			// "git -C <dir>" runs git in another tree — the one way to reach a
			// sibling worktree without naming a path anywhere else in the line.
			for _, dir := range gitDashCDirs(args) {
				if reason := s.checkBashTarget(dir, "git -C"); reason != "" {
					return reason
				}
			}
		}
	}

	for _, target := range redirectTargets(command) {
		if reason := s.checkBashTarget(target, "redirection"); reason != "" {
			return reason
		}
	}

	for _, nested := range commandSubstitutions(stripped) {
		if reason := s.checkBashCommand(nested, depth+1); reason != "" {
			return reason
		}
	}
	return ""
}

// checkBashArgs checks every argument of a write-capable/cd command. The word
// list is checked rather than a per-command model of which argument is the
// destination: cp/mv/ln/rsync/tee each take different shapes, and a path-shaped
// argument is a write destination in all of them (or a read the guard has no
// reason to block once it resolves outside the town).
func (s polecatPathScope) checkBashArgs(args []string, command string) string {
	for _, arg := range args {
		if reason := s.checkBashTarget(arg, command); reason != "" {
			return reason
		}
	}
	return ""
}

// checkBashTarget decides one Bash word (or redirection target). Only
// town-internal paths are denied: a path outside the town (/dev/null, /tmp,
// /etc/..., a URL, another project entirely) is not this guard's business, and
// allowing them is what keeps ordinary commands from being blocked.
func (s polecatPathScope) checkBashTarget(raw, context string) string {
	for _, candidate := range bashPathCandidates(raw) {
		target, ok := canonicalizeToolPath(candidate, s.cwd)
		if !ok {
			return fmt.Sprintf("%s path %q cannot be resolved (unknown variable or missing parent) — refusing to guess; name an explicit path inside your worktree: %s", context, candidate, s.worktree)
		}
		if !s.isTownPath(target) {
			continue
		}
		if s.isWorktreePath(target) || s.isOwnDirPath(target) || s.isRepoGitPath(target) || s.isScratchPath(target) {
			continue
		}
		return fmt.Sprintf("%s target is inside the town but not yours: %s (your worktree: %s, your polecat dir: %s, %s is allowed). Polecats work only in their own worktree.", context, target, s.worktree, s.ownDir, bareRepoDir)
	}
	return ""
}

// isTownPath reports whether target lies inside the town — the tree this guard
// protects. An unidentified town root disables only this rule, never the
// worktree rules.
func (s polecatPathScope) isTownPath(target string) bool {
	return isWithinPath(target, s.townRoot)
}

func (s polecatPathScope) isWorktreePath(target string) bool {
	return isWithinPath(target, s.worktree)
}

func (s polecatPathScope) isOwnDirPath(target string) bool {
	return isWithinPath(target, s.ownDir)
}

func (s polecatPathScope) isRepoGitPath(target string) bool {
	return isWithinPath(target, s.repoGit)
}

func (s polecatPathScope) isScratchPath(target string) bool {
	for _, root := range s.scratch {
		if isWithinPath(target, root) {
			return true
		}
	}
	return false
}

// resolvePolecatPathScope identifies the polecat whose paths this guard
// protects, or reports ok=false when the session is not a polecat's at all.
//
// Sources, in order of authority:
//
//  1. GT_POLECAT_PATH — the worktree the session manager launched this polecat
//     in (internal/polecat/session_manager.go sets it at spawn).
//  2. GT_POLECAT + the session cwd — used only when the env path is absent AND
//     cwd sits inside this polecat's own slot directory, so a cwd that wandered
//     into a sibling's worktree can never redefine which tree is "mine".
//
// The path supplies the layout (town root, rig root, sibling names); GT_RIG and
// GT_POLECAT supply identity, and identity wins if the two disagree.
func resolvePolecatPathScope(payloadCwd string) (polecatPathScope, bool) {
	name := os.Getenv("GT_POLECAT")
	if name == "" {
		return polecatPathScope{}, false
	}
	cwd := ""
	if payloadCwd != "" && filepath.IsAbs(payloadCwd) {
		if canonical, ok := canonicalizeToolPath(payloadCwd, ""); ok {
			cwd = canonical
		}
	}
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			if canonical, ok := canonicalizeToolPath(wd, ""); ok {
				cwd = canonical
			}
		}
	}

	own := os.Getenv("GT_POLECAT_PATH")
	if own == "" && cwd != "" {
		if layout, ok := splitPolecatLayout(cwd); ok && layout.name == name {
			own = layout.worktree
		}
	}
	if own == "" {
		return polecatPathScope{}, false
	}
	layout, ok := splitPolecatLayout(own)
	if !ok {
		return polecatPathScope{}, false
	}
	if rig := os.Getenv("GT_RIG"); rig != "" && rig != layout.rig {
		layout.polecatDir = filepath.Join(layout.townRoot, rig, "polecats", name)
		layout.worktree = filepath.Join(layout.polecatDir, filepath.Base(layout.worktree))
	}

	scope := polecatPathScope{
		worktree: resolveGuardDir(layout.worktree),
		ownDir:   resolveGuardDir(layout.polecatDir),
		repoGit:  resolveGuardDir(filepath.Join(layout.rigRoot, bareRepoDir)),
		townRoot: resolveGuardDir(layout.townRoot),
		cwd:      cwd,
		scratch:  scratchRoots(layout.townRoot),
	}
	if scope.worktree == "" {
		scope.worktree = scope.ownDir
	}
	if scope.worktree == "" {
		// Without a worktree there is no boundary to enforce; the guard stays
		// silent rather than denying every write in the session.
		return polecatPathScope{}, false
	}
	if scope.cwd == "" {
		scope.cwd = scope.worktree
	}
	return scope, true
}

// polecatLayout is the structural decomposition of a polecat path:
//
//	<townRoot>/<rig>/polecats/<name>[/<worktree>[/<deeper>...]]
type polecatLayout struct {
	townRoot   string
	rigRoot    string
	polecatDir string // <townRoot>/<rig>/polecats/<name>
	worktree   string // polecatDir, or the single directory level below it
	name       string
	rig        string
}

// splitPolecatLayout decomposes a path containing a "<rig>/polecats/<name>"
// component. Structural rather than registry-based on purpose: this runs on
// every Bash call in a session, and mayor/rigs.json is missing or stale often
// enough to matter (the live-fire probe for gt-6e2l ran against a town with no
// rigs.json at all).
//
// The worktree is the first directory level below the polecat's slot (rigs keep
// their checkout at polecats/<name>/<repo>); deeper paths resolve to that same
// level, so a shell cwd three directories into the checkout still yields the
// checkout root.
//
// The FIRST "<rig>/polecats/<name>" component wins, not the last: a checkout
// may itself contain a directory called polecats (a repo's own
// internal/polecats/...), and that path's owner is still the outer slot.
func splitPolecatLayout(path string) (polecatLayout, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return polecatLayout{}, false
	}
	clean := filepath.Clean(path)
	marker := string(filepath.Separator) + "polecats" + string(filepath.Separator)
	idx := strings.Index(clean, marker)
	if idx <= 0 {
		return polecatLayout{}, false
	}
	rigRoot := clean[:idx]
	rest := clean[idx+len(marker):]
	name, below, _ := strings.Cut(rest, string(filepath.Separator))
	if name == "" {
		return polecatLayout{}, false
	}
	polecatDir := filepath.Join(rigRoot, "polecats", name)
	worktree := polecatDir
	if below != "" {
		first, _, _ := strings.Cut(below, string(filepath.Separator))
		worktree = filepath.Join(polecatDir, first)
	}
	return polecatLayout{
		townRoot:   filepath.Dir(rigRoot),
		rigRoot:    rigRoot,
		polecatDir: polecatDir,
		worktree:   worktree,
		name:       name,
		rig:        filepath.Base(rigRoot),
	}, true
}

// canonicalizeToolPath resolves a path supplied by a tool call to the absolute,
// symlink-resolved, cleaned path it names:
//
//   - ~, $HOME, ${HOME} and any other variable this process can see are
//     expanded ($TMPDIR, $GT_POLECAT_PATH, ... — the guard runs with the
//     session's environment);
//   - a relative path resolves against cwd (the session's working directory);
//   - symlinks are resolved in the longest existing prefix, and the remaining
//     components are appended — so a file that does not exist yet still
//     resolves through its parent, which is how a Write to a new file is
//     judged without failing open on the missing leaf.
//
// ok is false when the path cannot be resolved at all — an unknown variable, an
// unresolvable home directory, a command substitution, no usable cwd, or no
// existing ancestor. Callers must treat !ok as DENY (fail closed, rule 3).
func canonicalizeToolPath(raw, cwd string) (string, bool) {
	if raw == "" {
		return "", false
	}
	// Command substitution is not knowable from here; treating its text as a
	// path would be a guess in both directions.
	if strings.Contains(raw, "$(") || strings.Contains(raw, "`") {
		return "", false
	}
	expanded := expandEnvVars(raw)
	expanded, ok := expandHomePath(expanded)
	if !ok || expanded == "" {
		return "", false
	}
	if strings.Contains(expanded, "$") {
		return "", false // a variable this process cannot see
	}
	if !filepath.IsAbs(expanded) {
		if cwd == "" {
			return "", false
		}
		expanded = filepath.Join(cwd, expanded)
	}
	expanded = filepath.Clean(expanded)

	// Resolve symlinks in the longest existing prefix; keep walking up until a
	// component resolves (or the root is reached, which always resolves).
	dir, rest := expanded, ""
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(resolved, rest), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// expandEnvVars expands $VAR and ${VAR} for every variable this process can see.
// A variable it cannot see is left in place ("$NAME"), which the caller detects
// and treats as unresolvable — guessing a value there is how a guard starts
// blocking innocent commands, or letting a real one through.
func expandEnvVars(raw string) string {
	return os.Expand(raw, func(key string) string {
		if value, found := os.LookupEnv(key); found {
			return value
		}
		return "$" + key
	})
}

// resolveGuardDir canonicalises one of the guard's *own* derived directories.
// Unlike a tool-supplied target (rule 3), a failure here falls back to the
// cleaned path instead of denying everything: these come from the session
// environment, not from the model, and a bogus boundary would wedge the session.
func resolveGuardDir(path string) string {
	if path == "" {
		return ""
	}
	if resolved, ok := canonicalizeToolPath(path, ""); ok {
		return resolved
	}
	return filepath.Clean(path)
}

// configScratchSubdirs are the $CLAUDE_CONFIG_DIR subdirectories a session owns
// and may write to: plan mode's plan files, the transcript and session-memory
// store, and todos.
var configScratchSubdirs = []string{"plans", "projects", "todos"}

// claudeConfigScratchRoots returns the writable subdirectories of the session's
// $CLAUDE_CONFIG_DIR.
//
// The config dir ITSELF is deliberately not a root. Gas Town points
// CLAUDE_CONFIG_DIR at the town's config tree (~/gt/.claude-town), which holds
// .claude.json (permissions, trust, MCP servers), settings.json (hooks) and
// session-env/ beside those state dirs — so allowlisting the whole tree let a
// guarded session rewrite the very harness config that governs it (gt-ovo1).
// Only the enumerated subdirectories are handed over.
//
// Fail closed on a config dir that cannot be a session's own: unset, relative,
// or at $HOME / above it. Each of those would allowlist a tree far wider than
// session state — $HOME would expose ~/plans next to ~/.ssh, and "/" would
// expose the filesystem root's own plans/, projects/ and todos/ — so they yield
// NO roots rather than a wrong one. The guard never depends on these entries,
// so an empty result only narrows it: the worktree, the town scratchpad and the
// temp directories stay writable.
func claudeConfigScratchRoots(configDir, home string) []string {
	if configDir == "" || !filepath.IsAbs(configDir) {
		return nil
	}
	cleaned := filepath.Clean(configDir)
	// A filesystem root is rejected before the home comparison: isWithinPath
	// matches on a "<root>/" prefix, which a lone "/" cannot form.
	if cleaned == string(filepath.Separator) {
		return nil
	}
	if home != "" && isWithinPath(filepath.Clean(home), cleaned) {
		return nil // the home dir itself, or an ancestor of it
	}
	roots := make([]string, 0, len(configScratchSubdirs))
	for _, sub := range configScratchSubdirs {
		roots = append(roots, filepath.Join(cleaned, sub))
	}
	return roots
}

// scratchRoots lists the directories a session may always write to: the host's
// temp directories, the state subdirectories of the session's own
// CLAUDE_CONFIG_DIR (see claudeConfigScratchRoots), and the transcripts/scratch
// area the agent runtime keeps outside the worktree (~/.claude/projects, and
// the town's own .claude-town).
//
// The temp entries are the session's own $TMPDIR (os.TempDir — which is where
// mktemp hands out paths) plus the well-known /tmp and /var/tmp. A blanket
// /var/folders entry is deliberately NOT included: it would make the whole
// macOS per-user temp hierarchy writable, which is far more than a polecat
// needs, and it is what made an earlier version of this guard's tests
// meaningless — a temp-dir test town looked like scratch space.
func scratchRoots(townRoot string) []string {
	candidates := []string{os.TempDir(), "/tmp", "/var/tmp"}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	candidates = append(candidates, claudeConfigScratchRoots(os.Getenv("CLAUDE_CONFIG_DIR"), home)...)
	if townRoot != "" {
		candidates = append(candidates, filepath.Join(townRoot, sessionScratchDir, "projects"))
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".claude", "projects"))
	}
	roots := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if root := resolveGuardDir(candidate); root != "" {
			roots = append(roots, root)
		}
	}
	return roots
}

// isWithinPath reports whether target is root itself or lies below it,
// comparing whole path components (so /worktree-evil never matches /worktree).
//
// Comparison is case-insensitive on purpose: the operator's host is macOS,
// whose default filesystem folds case, so /Users/sloan/GT and /Users/sloan/gt
// name the same tree. On a case-sensitive filesystem the only effect is a
// redundant block for a path differing from a real one in case alone, which is
// the safe direction — and matches how the scan guards already fold case.
func isWithinPath(target, root string) bool {
	if target == "" || root == "" {
		return false
	}
	target = strings.ToLower(filepath.Clean(target))
	root = strings.ToLower(filepath.Clean(root))
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}

// interpreterCommands execute a program supplied on their command line or on
// stdin, so neither their arguments nor their payload can be reasoned about as
// plain paths (gt-hmaf: the previous attempt whitelisted python/ruby/perl/node
// as read-only, which allowed python3 -c "open(...,'w').write(...)" anywhere).
var interpreterCommands = map[string]bool{
	"python": true, "python3": true, "ruby": true, "perl": true,
	"node": true, "nodejs": true, "php": true,
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
}

// writeCommands write, create or delete the paths named on their command line.
// Deliberately limited to commands whose *purpose* is writing: tar/unzip/zip/
// awk/patch are left out because they are at least as often readers — one of
// them reading a sibling worktree is normal polecat work (reading is allowed),
// and a false block costs more than the hole it would close.
var writeCommands = map[string]bool{
	"cp": true, "mv": true, "rm": true, "rmdir": true, "mkdir": true,
	"touch": true, "ln": true, "tee": true, "dd": true, "truncate": true,
	"shred": true, "install": true, "rsync": true, "chmod": true,
	"chown": true, "chgrp": true, "curl": true, "wget": true,
}

func isInterpreterCommand(base string) bool {
	return interpreterCommands[base]
}

// isWriteCapableCommand reports whether a command word can write to a path
// named on its command line. sed joins the list only with -i: without it sed
// reads and prints, and a polecat reading a sibling worktree is allowed.
func isWriteCapableCommand(base string, args []string) bool {
	if writeCommands[base] {
		return true
	}
	if base == "sed" {
		for _, arg := range args {
			// "-i", "-i.bak" (backup suffix) and "--in-place[=SUFFIX]".
			if strings.HasPrefix(arg, "-i") || strings.HasPrefix(arg, "--in-place") {
				return true
			}
		}
	}
	return false
}

// bashPathCandidates returns the path-shaped pieces a Bash word can name: the
// word itself when it looks like a path, the value of a --flag=/path or
// NAME=value word (curl's --output=, dd's of=), and every path-shaped substring
// inside a word carrying code punctuation (an interpreter's -c payload).
func bashPathCandidates(word string) []string {
	if word == "" {
		return nil
	}
	if strings.Contains(word, "://") {
		return nil // a URL, not a path
	}
	var out []string
	if strings.HasPrefix(word, "-") {
		// --output=/path: the value after "=" is a destination even though the
		// word reads as a flag.
		if _, value, found := strings.Cut(word, "="); found {
			return bashPathCandidates(value)
		}
		return nil
	}
	if looksLikePathWord(word) {
		out = append(out, word)
	}
	if _, value, found := strings.Cut(word, "="); found && value != "" {
		out = append(out, bashPathCandidates(value)...)
	}
	out = append(out, embeddedPaths(word)...)
	return dedupeStrings(out)
}

// dedupeStrings drops repeated candidates, keeping the first occurrence: a word
// can be produced by more than one rule above (the whole word and, say, the
// path-shaped run it consists of).
func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]bool, len(values))
	out := values[:0]
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// looksLikePathWord reports whether a word names a path rather than a flag,
// pattern or plain argument. A variable-led word counts: either it expands to a
// path the guard can judge, or it is denied as unresolvable.
//
// A word carrying code punctuation is NOT a path word: it is an interpreter
// payload (or another quoted script), and its paths are found by
// embeddedPaths instead — so that the payload as a whole is never resolved as
// if it were a filename.
func looksLikePathWord(word string) bool {
	switch {
	case word == "" || strings.HasPrefix(word, "-") || strings.Contains(word, "://"):
		return false
	case word == "." || word == "..":
		return true
	case strings.HasPrefix(word, "/"), strings.HasPrefix(word, "~"), strings.HasPrefix(word, "./"), strings.HasPrefix(word, "../"):
		return true
	case strings.HasPrefix(word, "$"):
		return true
	case strings.ContainsAny(word, "'\"`(),;"):
		return false
	}
	return strings.Contains(word, string(filepath.Separator))
}

// embeddedPathDelimiters are the characters code uses around a path literal in
// an interpreter payload: open('/x/y','w'), shutil.copy("a","b"), and the shell
// punctuation that can precede a path in a quoted script.
const embeddedPathDelimiters = " \t\n'\"`,;:()=[]{}<>|&!+*?"

// embeddedPathRunes terminate a path run inside a payload. "$" deliberately
// does not terminate: a run may be led by a variable ($HOME/..., ${X}/...).
const embeddedPathRunes = " \t\n'\"`,;:()=[]{}<>|&!*?"

// embeddedPaths extracts path-shaped runs from a word that carries code
// punctuation. A run must start at the beginning of the word or after a
// delimiter, so "/" inside an expression (1/2) or a URL (http://x/y) is not
// mistaken for a path, and must begin with an explicit path form (/ ~ ./ ../ or
// a variable) — a bare word inside code is an identifier, not a path.
func embeddedPaths(word string) []string {
	var out []string
	for i := 0; i < len(word); i++ {
		if i > 0 && !strings.ContainsRune(embeddedPathDelimiters, rune(word[i-1])) {
			continue
		}
		run := pathRun(word[i:])
		if run == "" {
			continue
		}
		if looksLikeExplicitPath(run) {
			out = append(out, run)
		}
		i += len(run) - 1
	}
	return out
}

// pathRun returns the leading run of path characters in s.
func pathRun(s string) string {
	end := 0
	for end < len(s) && !strings.ContainsRune(embeddedPathRunes, rune(s[end])) {
		end++
	}
	return s[:end]
}

// looksLikeExplicitPath reports whether a run begins with an explicit path form.
// Used for runs found inside code, where a bare identifier must not count.
func looksLikeExplicitPath(run string) bool {
	switch {
	case run == "":
		return false
	case strings.HasPrefix(run, "/"), strings.HasPrefix(run, "~"), strings.HasPrefix(run, "./"), strings.HasPrefix(run, "../"):
		return true
	case strings.HasPrefix(run, "$"):
		return true
	}
	return false
}

// gitDashCDirs returns the directories named by a git command's -C flags.
func gitDashCDirs(args []string) []string {
	var out []string
	for i, arg := range args {
		switch {
		case arg == "-C":
			if i+1 < len(args) {
				out = append(out, args[i+1])
			}
		case strings.HasPrefix(arg, "-C") && len(arg) > 2:
			out = append(out, arg[2:])
		}
	}
	return out
}

// splitShellSegments splits a tokenised command on the shell operators that
// start a new command (&&, ||, ;, |), so each sub-command is judged on its own
// command word — "cd x && cp a /elsewhere/b" must be checked at cp, not at cd.
func splitShellSegments(tokens []string) [][]string {
	var (
		segments [][]string
		current  []string
	)
	for _, token := range tokens {
		if shellCommandSeparators[token] {
			if len(current) > 0 {
				segments = append(segments, current)
			}
			current = nil
			continue
		}
		current = append(current, token)
	}
	if len(current) > 0 {
		segments = append(segments, current)
	}
	return segments
}

// segmentCommandWord returns a segment's command word and its arguments,
// skipping leading VAR=value assignments and the small set of launchers that
// carry the real command behind them (env VAR=x cmd, time cmd, sudo cmd).
func segmentCommandWord(segment []string) (string, []string) {
	for i, token := range segment {
		if isEnvAssignment(token) {
			continue
		}
		switch token {
		case "!", "time", "sudo", "command", "nohup", "env", "nice", "stdbuf":
			continue
		}
		return token, segment[i+1:]
	}
	return "", nil
}

// isEnvAssignment reports whether a token is a leading NAME=value environment
// assignment rather than the command word.
func isEnvAssignment(token string) bool {
	name, _, found := strings.Cut(token, "=")
	if !found || name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// redirectTargets returns the target words of a command's write redirections
// (>, >>, >|), read off the raw line so a redirect glued to its target
// ("echo x>/elsewhere/y") is still found. ">&1" and "2>&1" duplicate a file
// descriptor and name no path, so they are skipped.
func redirectTargets(command string) []string {
	var targets []string
	runes := []rune(command)
	var quote rune
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			if r == '\\' && quote == '"' {
				i++
				continue
			}
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '\\':
			i++
		case '>':
			for i+1 < len(runes) && (runes[i+1] == '>' || runes[i+1] == '|') {
				i++
			}
			j := i + 1
			for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t') {
				j++
			}
			if j >= len(runes) || runes[j] == '&' {
				continue // a file-descriptor duplication, not a path
			}
			word, next := readShellWord(runes, j)
			if word != "" {
				targets = append(targets, word)
			}
			i = next - 1
		}
	}
	return targets
}

// readShellWord reads one shell word starting at start, honoring quotes and
// escapes, and returns its unquoted content plus the index just past it.
func readShellWord(runes []rune, start int) (string, int) {
	var b strings.Builder
	var quote rune
	i := start
	for i < len(runes) {
		r := runes[i]
		if quote != 0 {
			if r == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				b.WriteRune(runes[i])
				i++
				continue
			}
			if r == quote {
				quote = 0
				i++
				continue
			}
			b.WriteRune(r)
			i++
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
		case r == '\\':
			if i+1 < len(runes) {
				i++
				b.WriteRune(runes[i])
			}
		case r == ' ' || r == '\t' || r == '\n' || strings.ContainsRune(";|&<>()", r):
			return b.String(), i
		default:
			b.WriteRune(r)
		}
		i++
	}
	return b.String(), i
}
