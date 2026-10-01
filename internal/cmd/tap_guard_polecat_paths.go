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
//     cwd, and symlink-resolved component by component exactly as the kernel
//     does (see resolveLikeKernel) — so "sym/.." and a dangling link are judged
//     where a write really lands, and a file that does not exist yet still
//     resolves through its parent. Compare
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
//  6. Expand a variable only against an environment the command line has not
//     already replaced. The guard sees the session's environment; the shell
//     that runs the command sees that environment plus whatever the line
//     assigns. Under rule 3 a name the guard cannot look up is denied, but a
//     name it *can* look up may hold a different value there — gt-tt8sg:
//     `path=$(command -v bd) ... rm "$path"` deleted the production bd, because
//     zsh exports a PATH-shaped `path` the guard read as an ordinary path
//     outside the town. A write target reached through a name this line assigns
//     (or sets from command -v/which/type) is denied as the indirection it is —
//     in the words that can name a write, which is to say a write-capable
//     command's arguments, an interpreter's -c/-e payload, and a redirection.
//     A command is judged wherever a construct puts it: the grouping and case
//     punctuation in front of a command word, which a flat token scan reports
//     as the word itself, is dropped before the word is read.
//
// What this guard deliberately does not attempt: reading file contents (a
// script written inside the worktree and then executed can still reach
// anywhere), and a complete shell parse. It is a seat belt, not a cage — the
// tool-trial watcher remains the backstop for hazards it misses.
//
// gt-tnts5: on 2026-09-26 a local-coder polecat (obsidian) wrote a shell stub
// over the production bd binary at $HOME/.local/bin/bd via a Bash echo, then
// repeatedly cp'd a backup over it trying to fix it — a town-wide bd outage
// (every agent's bd reads/writes broke for ~3 minutes). $HOME/.local/bin is
// the Makefile's INSTALL_DIR for both gt and bd, shared by every agent on the
// host via PATH, but it sits OUTSIDE the town tree — so the town-membership
// check above (isTownPath) never protected it. protectedBinDirs adds it (and
// only it — the concrete shared install path, not a speculative denylist of
// every bin-shaped directory) as a target no polecat may ever write to,
// regardless of town membership.
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
  - $HOME/.local/bin (the shared gt/bd install directory, off the town tree)
    is denied to Edit/Write/Bash alike, unconditionally — a polecat has no
    legitimate reason to write there, and a partial write breaks the binary
    for every agent on the host (gt-tnts5).

Paths are expanded (~, $HOME, and any variable this process can see), resolved
against the session cwd, symlink-resolved component by component the way the
kernel resolves them (a dangling link is judged by the target a write through
it creates), and compared by whole path components. A target that cannot be
resolved is DENIED — the guard fails closed rather than guessing.

A write target reached indirectly is DENIED as well (gt-tt8sg): the value of a
variable the command line assigns itself, of one set from command -v / which /
type, or of a command substitution is not a value this process can know —
path=$(command -v bd) ... rm "$path" deleted the production bd binary through
exactly that gap. Write the literal path.

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
	return tapGuardPolecatPaths(os.Stdin, os.Stderr, realGuardProcess())
}

// tapGuardPolecatPaths is the polecat-paths guard: it reads the hook payload
// from stdin and the session from proc, and prints a block to stderr.
func tapGuardPolecatPaths(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
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
	scope, ok := resolvePolecatPathScope(hook.Cwd, proc)
	if !ok {
		return nil // not a polecat session — outside this guard's remit
	}
	reason := scope.evaluate(hook)
	if reason == "" {
		return nil
	}
	fmt.Fprintf(stderr, "polecat-paths: %s\n", reason)
	return NewSilentExit(2)
}

// polecatPathScope is the set of paths the guarded polecat may write to,
// resolved once per hook invocation.
type polecatPathScope struct {
	worktree     string // canonical own git worktree (the session's cwd)
	ownDir       string // canonical <town>/<rig>/polecats/<name>
	repoGit      string // canonical <town>/<rig>/.repo.git
	townRoot     string // canonical town root
	cwd          string // canonical directory relative targets resolve against
	scratch      []string
	protectedBin []string // shared host binary dirs no polecat may write to (gt-tnts5)
	vars         commandVars
	proc         guardProcess
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
	target, ok := canonicalizeToolPath(s.proc, raw, s.cwd)
	if !ok {
		return fmt.Sprintf("%s target %q cannot be resolved (unknown variable or missing parent) — refusing to guess. Write inside your own worktree: %s", tool, raw, s.worktree)
	}
	if s.isProtectedBinPath(target) {
		return fmt.Sprintf("%s target is a shared host binary directory: %s. Polecats must never write there — a stub or partial write breaks gt/bd for every agent on the host (gt-tnts5).", tool, target)
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
	// What this command line assigns shadows the guard's environment for the
	// whole line, substitutions included (rule 6), so it is collected once here
	// and inherited by the recursive calls below.
	if s.vars.assigned == nil {
		s.vars = scanCommandVars(stripped)
	}
	// A heredoc that matters here has a body, which means the command spans
	// more than one line: the shared pattern also matches a bit-shift
	// expression inside an unquoted -c payload ("x << n"), and those are code,
	// not stdin.
	heredoc := strings.Contains(command, "\n") && heredocStartPattern.MatchString(command)

	segments := splitShellSegments(shellTokenize(stripped))
	for _, segment := range segments {
		word, args := segmentCommandWord(trimShellKeywords(segment))
		// A launcher can carry a construct rather than a command — `time ( rm
		// -rf X )`, `! { rm -rf X; }` — so what it carried is re-read until the
		// word is a command again.
		for opensConstruct(word) {
			word, args = segmentCommandWord(trimShellKeywords(append([]string{word}, args...)))
		}
		if word == "" {
			continue
		}
		base := filepath.Base(word)
		switch {
		case isInterpreterCommand(base):
			if heredoc {
				return "a script fed to " + base + " on stdin (heredoc) cannot be inspected — write the script inside your worktree and run that file instead"
			}
			// A payload is code, and code writes, so its words are judged
			// destructively like a write command's (gt-tt8sg).
			if reason := s.checkBashArgs(args, base, true); reason != "" {
				return reason
			}
		case isWriteCapableCommand(base, args):
			if reason := s.checkBashArgs(args, base, true); reason != "" {
				return reason
			}
			if base == "ln" {
				if reason := s.checkSymlinkText(args); reason != "" {
					return reason
				}
			}
		case base == "cd" || base == "pushd":
			// cd is not a write, but it decides where every later relative path
			// lands: a polecat that walks into a sibling worktree (or the
			// mayor/deacon/settings trees) is one careless relative write away
			// from corrupting them.
			if reason := s.checkBashArgs(args, base, false); reason != "" {
				return reason
			}
		case base == "git":
			// "git -C <dir>" runs git in another tree — the one way to reach a
			// sibling worktree without naming a path anywhere else in the line.
			for _, dir := range gitDashCDirs(args) {
				if reason := s.checkBashTarget(dir, "git -C", false); reason != "" {
					return reason
				}
			}
		}
	}

	for _, target := range redirectTargets(command) {
		if reason := s.checkBashTarget(target, "redirection", true); reason != "" {
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
// reason to block once it resolves outside the town). destructive marks the
// commands whose words can reach a write path — the write-capable ones, an
// interpreter's payload, and redirections — whose targets rule 6 also judges.
func (s polecatPathScope) checkBashArgs(args []string, command string, destructive bool) string {
	for _, arg := range args {
		if reason := s.checkBashTarget(arg, command, destructive); reason != "" {
			return reason
		}
	}
	return ""
}

// checkBashTarget decides one Bash word (or redirection target). Only
// town-internal paths are denied: a path outside the town (/dev/null, /tmp,
// /etc/..., a URL, another project entirely) is not this guard's business, and
// allowing them is what keeps ordinary commands from being blocked.
func (s polecatPathScope) checkBashTarget(raw, context string, destructive bool) string {
	candidates := bashPathCandidates(raw)
	if destructive {
		for _, candidate := range candidates {
			if reason := s.checkBashIndirection(candidate, context); reason != "" {
				return reason
			}
		}
	}
	for _, candidate := range candidates {
		if reason := s.checkBashPathFrom(candidate, s.cwd, context); reason != "" {
			return reason
		}
	}
	return ""
}

// checkBashIndirection denies a write target that names its destination through
// a value this process cannot know (rule 6, gt-tt8sg). It runs for the words
// that can name a write target — a write-capable command's arguments, an
// interpreter's payload, a redirection — and not for the rest: a read through
// an unknowable path is not a hazard, so it stays allowed (see
// TestPolecatPathGuardBashUnresolvableReadIsAllowed).
//
// Like the per-argument check above, this cannot tell a destination from a
// source or a payload — `cp "$src" "$dst"` is two candidates — so it over-blocks
// rather than under-blocks: `body=hi; curl -d "$body" https://x` is denied along
// with the incident's `rm "$path"`. The reason names the rewrite, which is the
// model's to make; guessing which operand the shell would really write to is
// the one thing this guard must not do.
func (s polecatPathScope) checkBashIndirection(candidate, context string) string {
	if strings.Contains(candidate, "$(") || strings.Contains(candidate, "`") {
		return destructiveTargetReason(context, candidate, "a command substitution has no value until it runs")
	}
	for _, name := range shellVarNames(candidate) {
		if s.vars.lookup[name] {
			return destructiveTargetReason(context, candidate, "$"+name+" is set from a command lookup (command -v, which, type)")
		}
		if s.vars.assigned[name] {
			return destructiveTargetReason(context, candidate, "$"+name+" is assigned by this same command line")
		}
	}
	return ""
}

// destructiveTargetReason is one line the model can act on: it names the rewrite.
func destructiveTargetReason(context, candidate, detail string) string {
	return fmt.Sprintf("%s target %q: unresolvable destructive target; write the literal path (%s, so this guard cannot tell what would be deleted or overwritten).", context, candidate, detail)
}

// checkBashPathFrom decides one path, resolved against base, by the Bash rule:
// only town-internal paths that are not the polecat's own (and the shared bin
// directory) are denied.
func (s polecatPathScope) checkBashPathFrom(candidate, base, context string) string {
	target, ok := canonicalizeToolPath(s.proc, candidate, base)
	if !ok {
		return fmt.Sprintf("%s path %q cannot be resolved (unknown variable or missing parent) — refusing to guess; name an explicit path inside your worktree: %s", context, candidate, s.worktree)
	}
	if s.isProtectedBinPath(target) {
		return fmt.Sprintf("%s target is a shared host binary directory: %s. Polecats must never write there — a stub or partial write breaks gt/bd for every agent on the host (gt-tnts5).", context, target)
	}
	if !s.isTownPath(target) {
		return ""
	}
	if s.isWorktreePath(target) || s.isOwnDirPath(target) || s.isRepoGitPath(target) || s.isScratchPath(target) {
		return ""
	}
	return fmt.Sprintf("%s target is inside the town but not yours: %s (your worktree: %s, your polecat dir: %s, %s is allowed). Polecats work only in their own worktree.", context, target, s.worktree, s.ownDir, bareRepoDir)
}

// checkSymlinkText judges the link text of "ln -s TEXT... LINK" where the kernel
// will read it: relative to the directory the link is created in, not the
// session cwd (gt-22hdp.41). checkBashArgs already judged every operand against
// the cwd; a link planted in scratch whose relative text climbs into the town
// passes that check, and then any later write through the link (by a script
// the guard never sees) lands in the town. Each text is therefore also judged
// from each directory the link may be created in. "ln -r" computes the text
// from cwd-relative operands, which checkBashArgs covers, so it is skipped.
func (s polecatPathScope) checkSymlinkText(args []string) string {
	ln := parseLnArgs(args)
	if !ln.symbolic || ln.relative || len(ln.texts) == 0 {
		return ""
	}
	for _, text := range ln.texts {
		for _, dir := range s.symlinkDirs(ln) {
			if reason := s.checkBashPathFrom(text, dir, "ln -s link text"); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// symlinkDirs returns the canonical directories an ln invocation may create its
// links in: -t DIR; else the destination's parent directory, plus the
// destination itself when it is an existing directory (ln then creates the link
// inside it); else, with a single operand, the cwd.
func (s polecatPathScope) symlinkDirs(ln lnArgs) []string {
	var raws []string
	switch {
	case ln.targetDir != "":
		raws = append(raws, ln.targetDir)
	case ln.dest != "":
		raws = append(raws, pathDirPart(ln.dest))
		if dest, ok := canonicalizeToolPath(s.proc, ln.dest, s.cwd); ok {
			if info, err := os.Stat(dest); err == nil && info.IsDir() {
				raws = append(raws, dest)
			}
		}
	default:
		raws = append(raws, ".")
	}
	var dirs []string
	for _, raw := range raws {
		dir, ok := canonicalizeToolPath(s.proc, raw, s.cwd)
		if !ok {
			// Only an unresolvable spelling gets here (an unknown variable), and
			// checkBashArgs has already denied that same operand fail-closed.
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

// pathDirPart returns the directory part of a path as spelled — everything
// before its final component — without cleaning it (a lexical clean before
// symlink resolution is the "sym/.." mistake). A bare name's directory is ".".
func pathDirPart(path string) string {
	trimmed := strings.TrimRight(path, string(filepath.Separator))
	if trimmed == "" {
		return string(filepath.Separator)
	}
	idx := strings.LastIndex(trimmed, string(filepath.Separator))
	switch {
	case idx < 0:
		return "."
	case idx == 0:
		return string(filepath.Separator)
	}
	return trimmed[:idx]
}

// lnArgs is the part of an ln command line that decides where its links point.
type lnArgs struct {
	symbolic  bool
	relative  bool     // -r / --relative: text computed from cwd-relative operands
	targetDir string   // -t DIR / --target-directory=DIR
	texts     []string // link texts (the TARGET operands)
	dest      string   // the LINK_NAME or DIRECTORY operand, when not -t
}

// lnValueKind is how a GNU ln long option takes its value.
type lnValueKind int

const (
	lnNoValue       lnValueKind = iota
	lnRequiredValue             // attached with "=" or the next word
	lnOptionalValue             // attached with "=" only
)

// lnLongOption is one of GNU ln's long options.
type lnLongOption struct {
	name  string
	value lnValueKind
}

// lnLongOptions is GNU ln's long-option table. getopt_long accepts any
// unambiguous prefix of these names, so "--sym" is --symbolic and
// "--target-dir=X" is --target-directory=X; matching only the full spellings
// would let an abbreviated form skip the link-text check.
var lnLongOptions = []lnLongOption{
	{"backup", lnOptionalValue},
	{"directory", lnNoValue},
	{"force", lnNoValue},
	{"help", lnNoValue},
	{"interactive", lnNoValue},
	{"logical", lnNoValue},
	{"no-dereference", lnNoValue},
	{"no-target-directory", lnNoValue},
	{"physical", lnNoValue},
	{"relative", lnNoValue},
	{"suffix", lnRequiredValue},
	{"symbolic", lnNoValue},
	{"target-directory", lnRequiredValue},
	{"verbose", lnNoValue},
	{"version", lnNoValue},
}

// resolveLnLongOption resolves a long option's spelling (without "--" or any
// "=value") the way getopt_long does: an exact name wins, otherwise a prefix
// that names exactly one option. An ambiguous or unknown spelling makes ln
// exit with an error before creating anything, so it resolves to ok=false.
func resolveLnLongOption(name string) (lnLongOption, bool) {
	var match lnLongOption
	matches := 0
	for _, opt := range lnLongOptions {
		if opt.name == name {
			return opt, true
		}
		if name != "" && strings.HasPrefix(opt.name, name) {
			match = opt
			matches++
		}
	}
	return match, matches == 1
}

// parseLnArgs reads GNU and BSD ln's option syntax: clustered short flags
// (-sfn), -t/-S taking a value (attached or as the next word), the long forms
// including their unambiguous abbreviations, and "--" ending options.
func parseLnArgs(args []string) lnArgs {
	var (
		ln       lnArgs
		operands []string
	)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			operands = append(operands, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(arg, "--"):
			name, value, attached := strings.Cut(arg[2:], "=")
			opt, ok := resolveLnLongOption(name)
			if !ok {
				continue
			}
			if opt.value == lnRequiredValue && !attached && i+1 < len(args) {
				i++
				value = args[i]
			}
			switch opt.name {
			case "symbolic":
				ln.symbolic = true
			case "relative":
				ln.relative = true
			case "target-directory":
				ln.targetDir = value
			}
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
		cluster:
			for j := 1; j < len(arg); j++ {
				switch arg[j] {
				case 's':
					ln.symbolic = true
				case 'r':
					ln.relative = true
				case 't', 'S':
					value := arg[j+1:]
					if value == "" && i+1 < len(args) {
						i++
						value = args[i]
					}
					if arg[j] == 't' {
						ln.targetDir = value
					}
					break cluster
				}
			}
		default:
			operands = append(operands, arg)
		}
	}
	switch {
	case ln.targetDir != "":
		ln.texts = operands
	case len(operands) == 1:
		ln.texts = operands
	case len(operands) > 1:
		ln.texts = operands[:len(operands)-1]
		ln.dest = operands[len(operands)-1]
	}
	return ln
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

// isScratchPath reports whether target is in scratch space. A scratch root
// that contains the town (a town under /tmp, or a session whose $TMPDIR is the
// town's parent) is scratch only outside the town: honoring it inside would
// exempt every town path from the rules this guard exists for. On Linux
// t.TempDir() is under /tmp, which is how every polecat-paths test passed on
// macOS and failed on the CI runner (gt-22hdp.39). A root inside the town
// (the town's .claude-town/projects) is unaffected.
func (s polecatPathScope) isScratchPath(target string) bool {
	for _, root := range s.scratch {
		if !isWithinPath(target, root) {
			continue
		}
		if s.townRoot != "" && isWithinPath(s.townRoot, root) && s.isTownPath(target) {
			continue
		}
		return true
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
func resolvePolecatPathScope(payloadCwd string, proc guardProcess) (polecatPathScope, bool) {
	name := proc.getenv("GT_POLECAT")
	if name == "" {
		return polecatPathScope{}, false
	}
	cwd := ""
	if payloadCwd != "" && filepath.IsAbs(payloadCwd) {
		if canonical, ok := canonicalizeToolPath(proc, payloadCwd, ""); ok {
			cwd = canonical
		}
	}
	if cwd == "" {
		if wd, err := proc.getwd(); err == nil {
			if canonical, ok := canonicalizeToolPath(proc, wd, ""); ok {
				cwd = canonical
			}
		}
	}

	own := proc.getenv("GT_POLECAT_PATH")
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
	if rig := proc.getenv("GT_RIG"); rig != "" && rig != layout.rig {
		layout.polecatDir = filepath.Join(layout.townRoot, rig, "polecats", name)
		layout.worktree = filepath.Join(layout.polecatDir, filepath.Base(layout.worktree))
	}

	scope := polecatPathScope{
		worktree:     resolveGuardDir(proc, layout.worktree),
		ownDir:       resolveGuardDir(proc, layout.polecatDir),
		repoGit:      resolveGuardDir(proc, filepath.Join(layout.rigRoot, bareRepoDir)),
		townRoot:     resolveGuardDir(proc, layout.townRoot),
		cwd:          cwd,
		scratch:      scratchRoots(proc, layout.townRoot),
		protectedBin: protectedBinDirs(proc),
		proc:         proc,
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
// symlink-resolved, cleaned path the kernel would write to:
//
//   - ~, $HOME, ${HOME} and any other variable this process can see are
//     expanded ($TMPDIR, $GT_POLECAT_PATH, ... — the guard runs with the
//     session's environment);
//   - a relative path resolves against cwd (the session's working directory);
//   - the result is resolved the way the kernel resolves it (see
//     resolveLikeKernel): component by component, following each symlink as
//     it is met — so "sym/.." is the parent of sym's TARGET, relative link
//     text is relative to the link's own directory, and a dangling link is
//     judged by the target a write through it would create. Components that
//     do not exist yet are kept as spelled, which is how a Write to a new file
//     is judged without failing open on the missing leaf.
//
// ok is false when the path cannot be resolved at all — an unknown variable, an
// unresolvable home directory, a command substitution, no usable cwd, or a
// symlink loop. Callers must treat !ok as DENY (fail closed, rule 3).
func canonicalizeToolPath(proc guardProcess, raw, cwd string) (string, bool) {
	if raw == "" {
		return "", false
	}
	// Command substitution is not knowable from here; treating its text as a
	// path would be a guess in both directions.
	if strings.Contains(raw, "$(") || strings.Contains(raw, "`") {
		return "", false
	}
	expanded := expandEnvVars(proc, raw)
	expanded, ok := expandHomePath(proc, expanded)
	if !ok || expanded == "" {
		return "", false
	}
	if strings.Contains(expanded, "$") {
		return "", false // a variable this process cannot see
	}
	if !filepath.IsAbs(expanded) {
		if cwd == "" || !filepath.IsAbs(cwd) {
			return "", false
		}
		// Concatenate, never filepath.Join: Join cleans, and a lexical clean
		// before symlink resolution is exactly the "sym/.." mistake.
		expanded = cwd + string(filepath.Separator) + expanded
	}
	return resolveLikeKernel(expanded)
}

// maxSymlinkFollows bounds how many symlinks resolveLikeKernel follows for one
// path, the way the kernel's own limit (MAXSYMLINKS: 32 on macOS, 40 on Linux)
// turns a link cycle into ELOOP instead of a hang.
const maxSymlinkFollows = 40

// resolveLikeKernel resolves an absolute path the way namei does (gt-22hdp.41).
// The previous implementation cleaned the path lexically and then resolved its
// longest existing prefix with filepath.EvalSymlinks, which disagreed with the
// kernel three ways, each a way to write into the town from a path that looked
// like scratch:
//
//  1. "a/sym/../f" was cleaned to "a/f"; the kernel follows sym first and takes
//     ".." from its target.
//  2. A dangling link could not be EvalSymlinks'd, so the walk fell back to the
//     link's parent directory — while a write through a dangling final link
//     creates the file at the link's target.
//  3. Relative link text in such a chain was never read at all, instead of
//     being resolved against the link's own directory.
//
// The walk: start at the root; for each component, ".." moves to the parent of
// the path resolved so far (which is symlink-free, so its lexical parent is its
// real parent) and "." is skipped. Anything else is appended and Lstat'ed; a
// symlink is replaced by its Readlink text — absolute text restarts from the
// root, relative text continues from the link's directory — and that text's
// components are resolved before the rest. A component that does not exist is
// kept as spelled (the Lstat of everything below it fails the same way), so a
// not-yet-created file or directory run still resolves. More than
// maxSymlinkFollows links is a loop: fail closed.
func resolveLikeKernel(path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	root := filepath.VolumeName(path) + string(filepath.Separator)
	resolved := root
	pending := kernelPathComponents(path[len(filepath.VolumeName(path)):])
	follows := 0
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved) // Dir of the root is the root, as in the kernel
			continue
		}
		next := filepath.Join(resolved, name)
		info, err := os.Lstat(next)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			// Missing (a file or directory the write will create), a real file or
			// directory, or unreadable (the polecat runs as this same user, so
			// its own write cannot traverse it either): keep it as spelled.
			resolved = next
			continue
		}
		follows++
		if follows > maxSymlinkFollows {
			return "", false
		}
		text, err := os.Readlink(next)
		if err != nil || text == "" {
			return "", false
		}
		if filepath.IsAbs(text) {
			resolved = filepath.VolumeName(text) + string(filepath.Separator)
			text = text[len(filepath.VolumeName(text)):]
		}
		// Relative text: resolved is still the link's directory.
		pending = append(kernelPathComponents(text), pending...)
	}
	return resolved, true
}

// kernelPathComponents splits a path into its non-empty components, keeping "."
// and ".." (they are resolved in order, never cleaned away). It splits on the
// OS separator only: a backslash is an ordinary filename byte on Unix, which is
// why a separator-agnostic split is not used here.
func kernelPathComponents(path string) []string {
	parts := strings.Split(path, string(filepath.Separator))
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// expandEnvVars expands $VAR and ${VAR} for every variable proc can see.
// A variable it cannot see is left in place ("$NAME"), which the caller detects
// and treats as unresolvable — guessing a value there is how a guard starts
// blocking innocent commands, or letting a real one through.
func expandEnvVars(proc guardProcess, raw string) string {
	return os.Expand(raw, func(key string) string {
		if value, found := proc.lookupEnv(key); found {
			return value
		}
		return "$" + key
	})
}

// resolveGuardDir canonicalises one of the guard's *own* derived directories.
// Unlike a tool-supplied target (rule 3), a failure here falls back to the
// cleaned path instead of denying everything: these come from the session
// environment, not from the model, and a bogus boundary would wedge the session.
func resolveGuardDir(proc guardProcess, path string) string {
	if path == "" {
		return ""
	}
	if resolved, ok := canonicalizeToolPath(proc, path, ""); ok {
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
// hostTempScratchDirs are the well-known host temp dirs scratchRoots adds
// beside $TMPDIR (guardProcess.hostTemp in production). A field there so the
// guard's test town can be judged apart from wherever the host's /tmp is: on
// Linux t.TempDir() is itself under /tmp, so the test town's $HOME and its
// surroundings were scratch space there and not on macOS.
var hostTempScratchDirs = []string{"/tmp", "/var/tmp"}

func scratchRoots(proc guardProcess, townRoot string) []string {
	candidates := append([]string{proc.tempDir()}, proc.hostTemp...)
	home, err := proc.homeDir()
	if err != nil {
		home = ""
	}
	candidates = append(candidates, claudeConfigScratchRoots(proc.getenv("CLAUDE_CONFIG_DIR"), home)...)
	if townRoot != "" {
		candidates = append(candidates, filepath.Join(townRoot, sessionScratchDir, "projects"))
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".claude", "projects"))
	}
	roots := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if root := resolveGuardDir(proc, candidate); root != "" {
			roots = append(roots, root)
		}
	}
	return roots
}

// protectedBinDirs lists shared host binary directories a polecat must never
// write to, independent of town membership (gt-tnts5). $HOME/.local/bin is the
// Makefile's INSTALL_DIR for both gt and bd — every agent on the host resolves
// them off PATH there — and it sits outside the town tree, so isTownPath alone
// never covers it. An unresolvable $HOME yields no roots rather than guessing:
// callers still have the town-membership rules to fall back on.
func protectedBinDirs(proc guardProcess) []string {
	home, err := proc.homeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{resolveGuardDir(proc, filepath.Join(home, ".local", "bin"))}
}

// isProtectedBinPath reports whether target lies inside a shared host binary
// directory (see protectedBinDirs) — checked ahead of, and regardless of,
// town membership: the whole point is that these paths are NOT inside the
// town tree.
func (s polecatPathScope) isProtectedBinPath(target string) bool {
	for _, dir := range s.protectedBin {
		if isWithinPath(target, dir) {
			return true
		}
	}
	return false
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

// commandVars is what one command line does to shell variables. The guard
// expands a target's variable from its OWN environment, which is stale the
// moment the line assigns that name itself (rule 6, gt-tt8sg), so the names a
// line assigns are collected before any target is judged.
type commandVars struct {
	assigned map[string]bool // `NAME=...`, `for NAME in ...`, `read NAME` in this line
	lookup   map[string]bool // subset: the value is a command -v/which/type result
}

// setterCommands are the words that name a variable in their arguments rather
// than by assignment: `read NAME`, `mapfile NAME`, `printf -v NAME`. The set is
// the setters a command line realistically uses, read at command position only
// (see commandStart): a word missed here is a name whose stale environment
// value the guard would go on to trust.
var setterCommands = map[string]bool{
	"read": true, "mapfile": true, "readarray": true, "printf": true,
}

// lookupAssignmentPattern matches an assignment whose value is a command
// substitution around a lookup: NAME=$(command -v ...), NAME=$(which ...) or
// NAME=$(type ...), in either substitution spelling and with or without
// quotes. It is matched against the raw line rather than the tokens because an
// unquoted substitution is split across tokens (`path=$(command`, `-v`,
// `$bin)`).
var lookupAssignmentPattern = regexp.MustCompile("([A-Za-z_][A-Za-z0-9_]*)=[\"']?(?:\\$\\(|`)\\s*(?:command\\s+-v|which|type)\\b")

// scanCommandVars collects the variable names a command line assigns. It reads
// the tokenised line so a "NAME=" inside a commit message or a quoted argument
// is not mistaken for an assignment, and strips heredoc bodies first for the
// same reason (they are data on stdin).
func scanCommandVars(command string) commandVars {
	vars := commandVars{assigned: map[string]bool{}, lookup: map[string]bool{}}
	tokens := shellTokenize(stripHeredocBodies(command))
	for i, token := range tokens {
		if isEnvAssignment(token) {
			name, _, _ := strings.Cut(token, "=")
			vars.assigned[name] = true
			continue
		}
		// A NAME=word is an assignment wherever it stands, but `for`/`read`/
		// `printf` only set a variable as a command word: `echo for x` and
		// `grep -n read p` name no variable, and reading them as setters would
		// deny a target the line really does spell out.
		if !setterCommands[token] && token != "for" && token != "select" {
			continue
		}
		if !commandStart(tokens, i) {
			continue
		}
		switch token {
		case "for", "select":
			if i+1 < len(tokens) && isShellVarName(tokens[i+1]) {
				vars.assigned[tokens[i+1]] = true
			}
		case "read", "mapfile", "readarray":
			// Every name-shaped word after the command is a name it sets. A
			// flag's own value (-p prompt, -d delim) can look like one, and
			// that over-collection is deliberate: the fail-closed direction is
			// to judge a target through a name that may have been replaced.
			for j := i + 1; j < len(tokens) && !shellCommandSeparators[tokens[j]]; j++ {
				if isShellVarName(tokens[j]) {
					vars.assigned[tokens[j]] = true
				}
			}
		case "printf":
			// Only -v sets a name; the format and its arguments do not.
			for j := i + 1; j+1 < len(tokens); j++ {
				if tokens[j] == "-v" && isShellVarName(tokens[j+1]) {
					vars.assigned[tokens[j+1]] = true
				}
			}
		}
	}
	for _, match := range lookupAssignmentPattern.FindAllStringSubmatch(command, -1) {
		vars.lookup[match[1]] = true
		vars.assigned[match[1]] = true
	}
	return vars
}

// commandStart reports whether tokens[i] stands where a command word does: at
// the front of the line, or after a separator or a keyword that opens one.
func commandStart(tokens []string, i int) bool {
	if i == 0 {
		return true
	}
	prev := tokens[i-1]
	return shellCommandSeparators[prev] || shellCommandKeywords[prev] || setterLaunchers[prev]
}

// setterLaunchers carry the real command behind them (see segmentCommandWord),
// so a setter word right after one is still a command word.
var setterLaunchers = map[string]bool{
	"!": true, "time": true, "sudo": true, "command": true,
	"nohup": true, "env": true, "nice": true, "stdbuf": true,
}

// shellVarNames returns the variable names a word references, in $NAME and
// ${NAME} form. `$?`, `$$` and positional `$1` are not names and are skipped —
// they are never destinations a caller meant to name indirectly.
func shellVarNames(word string) []string {
	var names []string
	for i := 0; i < len(word); i++ {
		if word[i] != '$' || i+1 >= len(word) {
			continue
		}
		if word[i+1] == '{' {
			end := strings.IndexByte(word[i+2:], '}')
			if end < 0 {
				break
			}
			if name := word[i+2 : i+2+end]; isShellVarName(name) {
				names = append(names, name)
			}
			i += 2 + end
			continue
		}
		j := i + 1
		for j < len(word) && isShellVarNameChar(word[j]) {
			j++
		}
		if name := word[i+1 : j]; isShellVarName(name) {
			names = append(names, name)
			i = j - 1
		}
	}
	return names
}

// isShellVarName reports whether name is a variable name a shell could assign:
// a letter or underscore, then letters, digits and underscores. `$1` is
// therefore not a name.
func isShellVarName(name string) bool {
	if name == "" {
		return false
	}
	if c := name[0]; c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isShellVarNameChar(name[i]) {
			return false
		}
	}
	return true
}

// isShellVarNameChar reports whether b may appear anywhere in a variable name.
func isShellVarNameChar(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
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
	"chown": true, "chgrp": true, "chflags": true, "curl": true, "wget": true,
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
// start a new command (; / & / && / || / |), so each sub-command is judged on
// its own command word — "cd x && cp a /elsewhere/b" must be checked at cp, not
// at cd. A background & belongs here with the others: it runs the following
// command just as ; does (gt-wwwht).
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

// shellCommandKeywords introduce a command, or open a construct that contains
// one, instead of being one themselves. A segment that opens with one reports
// the keyword as its command word, which hides what follows: `do rm -rf X`,
// `( rm -rf X )`, `{ rm -rf X; }` and a `case` body's `a) rm -rf X ;;` all
// named no write-capable command (gt-tt8sg).
var shellCommandKeywords = map[string]bool{
	"do": true, "then": true, "else": true, "elif": true,
	"if": true, "while": true, "until": true,
	// A subshell, a brace group, and the `case` shell that opens one.
	"(": true, "{": true, "case": true, "in": true, "esac": true,
}

// opensConstruct reports whether a command word is really the structure of a
// construct, with the command it contains still behind it.
func opensConstruct(word string) bool {
	return shellCommandKeywords[word] || strings.HasPrefix(word, "(") || strings.HasPrefix(word, "{")
}

// trimShellKeywords drops the structure in front of a segment, so the command
// word behind it is the one judged:
//
//   - the control keywords and grouping tokens above;
//   - a `case` pattern, a word ending in ')' ("a)", "*.go)") that names no
//     command itself;
//   - a grouping token glued to its command by the tokenizer, which splits on
//     whitespace only ("(rm" and "{rm" arrive as one token).
func trimShellKeywords(segment []string) []string {
	for len(segment) > 0 {
		head := segment[0]
		if head == "case" {
			// `case WORD in` names no command: the word (and any expansion in
			// it) is the subject, and `in` opens the first pattern. Drop through
			// the `in`, then let the pattern handling below take over.
			segment = segment[1:]
			for len(segment) > 0 && segment[0] != "in" {
				segment = segment[1:]
			}
			if len(segment) > 0 {
				segment = segment[1:]
			}
			continue
		}
		if shellCommandKeywords[head] || strings.HasSuffix(head, ")") {
			segment = segment[1:]
			continue
		}
		trimmed := strings.TrimLeft(head, "({")
		if trimmed == head {
			break
		}
		if trimmed == "" {
			segment = segment[1:]
			continue
		}
		segment = append([]string{trimmed}, segment[1:]...)
	}
	return segment
}

// segmentCommandWord returns a segment's command word and its arguments,
// skipping leading VAR=value assignments and the small set of launchers that
// carry the real command behind them (env VAR=x cmd, time cmd, sudo cmd, exec
// cmd — gt-tt8sg: `exec rm -rf X` named exec and the rm went unseen).
func segmentCommandWord(segment []string) (string, []string) {
	for i, token := range segment {
		if isEnvAssignment(token) {
			continue
		}
		switch token {
		case "!", "time", "sudo", "command", "nohup", "env", "nice", "stdbuf", "exec", "builtin":
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
