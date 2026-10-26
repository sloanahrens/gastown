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

// tap_guard_script_files.go implements `gt tap guard script-files` (gt-b43ra).
//
// A one-off shell, Perl or Ruby script is the easy way to finish a job and it
// is debt: untestable, unowned, and invisible to every gate. The town already
// grades a new script that holds logic as a MAJOR finding (.om.json,
// scripts-are-debt); this guard makes that request at the moment the file
// would be created, while the agent can still choose the Go package.
//
// What it does not do: block editing or deleting an existing script (that is
// debt being fixed, not added), block a new Python file (allowed when Python
// genuinely is the best tool; the guard prints an advisory asking for the
// reason in the bead instead), or judge any path outside the session's own
// worktree (scratch directories are out of scope).

// scriptFileExtensions are the suffixes of the files this guard refuses to
// let a session create. They are compared lowercased, so "setup.SH" is the
// same request as "setup.sh".
var scriptFileExtensions = []string{".sh", ".bash", ".zsh", ".pl", ".rb"}

// scriptFilePythonExtension is the one script-shaped suffix that is allowed,
// with the advisory judgeNewScript prints.
const scriptFilePythonExtension = ".py"

// scriptFilePolicy is the whole redirect this guard delivers, in one constant
// so the denial message, the unreadable-payload denial and the tests cannot
// drift apart (gt-b43ra).
const scriptFilePolicy = `Scripts are debt: write a Go package with unit tests and seams for the
clock, filesystem and process runner instead of a one-off script. A procedure
that is only operator steps is a runbook, not a script.
Bash is not judged by this guard. The planned commit gate gt-lpm8b, not yet
landed, is to catch a Bash-created script when it is committed.`

var tapGuardScriptFilesCmd = &cobra.Command{
	Use:   "script-files",
	Short: "Deny a new script file in a worktree and redirect to testable Go",
	Long: `Deny creating a new shell, Perl or Ruby script in a Gas Town worker's worktree.

Edit/Write/MultiEdit/NotebookEdit are denied when the target is inside the
session's own worktree, does not exist yet, and ends in .sh, .bash, .zsh, .pl
or .rb. Editing a script that already exists is allowed, and removing one is
always allowed. Bash is not judged here; the denial redirect says where Bash-created scripts
are to be caught.

The guard fails closed on what it can name: a hook payload it cannot read or
parse, or a session whose worktree it cannot place, is denied when the payload
or the target names a script suffix. A path that cannot be resolved is denied
when its name ends in a denied script suffix.

A new .py file is allowed with an advisory on stderr asking for the reason to
be recorded in the bead: Python is allowed when it genuinely is the best tool,
so it gets a reminder rather than a denial.

The denial redirects: ` + scriptFilePolicy + `

Scope: file-writing tools in a polecat or crew session's own Gas Town worktree
(<rig>/polecats/<name>/<repo> or <rig>/crew/<name>). Paths outside every
worktree are not judged. Every other session is judged as a worker.

Exit codes:
  0 - Allowed (not a new script, or the target exists). A file write allowed
      because the worktree could not be placed prints a warning on stderr.
  2 - DENIED (one-line reason on stderr, so the model self-corrects)`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return tapGuardScriptFiles(os.Stdin, os.Stderr, realGuardProcess())
	},
}

func init() {
	tapGuardCmd.AddCommand(tapGuardScriptFilesCmd)
}

// scriptFileInput is the subset of the Claude Code PreToolUse payload this
// guard reads. cwd is where relative targets resolve from.
type scriptFileInput struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// tapGuardScriptFiles is the guard: it reads the hook payload from stdin and
// the session from proc, and prints a denial to stderr.
func tapGuardScriptFiles(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
	if err != nil || len(input) == 0 {
		return unjudgedPayload(input, stderr, "the hook payload could not be read")
	}
	var hook scriptFileInput
	if err := json.Unmarshal(input, &hook); err != nil {
		return unjudgedPayload(input, stderr, "the hook payload could not be parsed")
	}
	scope, ok := resolveScriptFileScope(hook.Cwd, proc)
	if !ok {
		return unjudgedWorktree(hook, stderr)
	}
	switch hook.ToolName {
	case "Edit", "Write", "MultiEdit":
		return scope.checkFileTarget(hook.ToolInput.FilePath, hook.ToolName, stderr)
	case "NotebookEdit":
		target := hook.ToolInput.NotebookPath
		if target == "" {
			target = hook.ToolInput.FilePath
		}
		return scope.checkFileTarget(target, hook.ToolName, stderr)
	}
	return nil
}

// scriptFileScope is the worktree one session's creations are judged against,
// resolved once per hook invocation.
type scriptFileScope struct {
	worktree string // canonical own worktree; the only tree this guard polices
	cwd      string // canonical directory relative targets resolve from
	proc     guardProcess
}

// resolveScriptFileScope identifies the session's worktree. ok reports whether
// it could be named. When it cannot, the hook does not judge the call against
// a worktree: unjudgedWorktree denies only a file-writing target whose name ends
// in a denied script suffix and allows the rest (gt-b43ra).
func resolveScriptFileScope(payloadCwd string, proc guardProcess) (scope scriptFileScope, ok bool) {
	role := scriptFileWorkerRole(proc)
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
	if cwd == "" {
		return scriptFileScope{}, false
	}

	worktree := ""
	if role == RolePolecat {
		// The polecat-paths helper already answers which tree is this polecat's
		// (identity from GT_POLECAT, layout from GT_POLECAT_PATH); two guards
		// disagreeing about that would be a bug of its own.
		if polecat, ok := resolvePolecatPathScope(cwd, proc); ok {
			worktree = polecat.worktree
		}
	}
	if worktree == "" {
		// Crew have no worktree variable: their checkout is the directory the
		// session runs in, found by walking up to its .git. A session with no
		// role says nothing about which tree is its own, so any checkout it
		// stands in is judged.
		if root, ok := gitWorktreeRoot(cwd, proc.checkoutBound); ok && (role == "" || isWorkerWorktreeForRole(root, role)) {
			worktree = root
		}
	}
	if worktree == "" {
		return scriptFileScope{}, false
	}
	return scriptFileScope{worktree: worktree, cwd: cwd, proc: proc}, true
}

// scriptFileWorkerRole reports the role whose worktree this guard looks for.
// GT_ROLE decides whenever it is set — a coordinator can carry a stale
// GT_POLECAT from having spawned polecats — as it does for the rest of the
// guard family. Every session is judged: only a recognized polecat or crew role
// names a worktree by role, and any other session (an unrecognized role, or no
// identifying variable at all) is judged as a worker, fail closed (gt-b43ra).
func scriptFileWorkerRole(proc guardProcess) Role {
	if role := strings.TrimSpace(proc.getenv("GT_ROLE")); role != "" {
		parsed, _, _ := parseRoleString(role)
		if parsed == RolePolecat || parsed == RoleCrew {
			return parsed
		}
		return ""
	}
	switch {
	case proc.getenv("GT_POLECAT") != "":
		return RolePolecat
	case proc.getenv("GT_CREW") != "":
		return RoleCrew
	}
	return ""
}

// gitWorktreeRoot returns the directory containing cwd's ".git" entry, which
// is the top of the checkout a crew member works in.
func gitWorktreeRoot(cwd, bound string) (string, bool) {
	for dir := cwd; ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		if bound != "" && dir == bound {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// isWorkerWorktreeForRole reports whether root sits where Gas Town keeps that
// role's checkout: <rig>/polecats/<name>[/<repo>] for a polecat,
// <rig>/crew/<name> for a crew member. The component check keeps the guard
// off an ordinary repository that happens to live under a directory named
// "crew" or "polecats".
func isWorkerWorktreeForRole(root string, role Role) bool {
	want := "polecats"
	if role == RoleCrew {
		want = "crew"
	}
	parts := kernelPathComponents(root)
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == want {
			return true
		}
	}
	return false
}

// checkFileTarget decides one Edit/Write/MultiEdit/NotebookEdit call.
func (s scriptFileScope) checkFileTarget(raw, tool string, stderr io.Writer) error {
	// The name the agent wrote is judged even when it is a dangling symlink,
	// because resolving the link would judge its target's name instead.
	named := raw
	if !filepath.IsAbs(named) {
		named = filepath.Join(s.cwd, named)
	}
	if scriptFileNamed(raw) && isDanglingSymlink(named) {
		printScriptFileRefusal(stderr, tool, raw)
		return NewSilentExit(2)
	}
	// The write lands at the end of any symlink chain. This walk follows the
	// chain itself, up to maxWriteHops, so a link's target is judged even when
	// the kernel-like resolver gives up on the chain (gt-b43ra).
	target, ok := s.resolveTarget(raw, s.cwd)
	// Walk from the canonical directory with the name as written. Resolving the
	// whole path first would replace the chain with its final target, and the
	// names on the way (the reason for the walk) would be lost where the kernel
	// resolves a long chain; the directory alone is canonicalized, so the walk
	// compares with the worktree's canonical root.
	start := named
	if dir, dirOK := s.resolveTarget(filepath.Dir(raw), s.cwd); dirOK {
		start = filepath.Join(dir, filepath.Base(raw))
	}
	chain, final, walked := followWriteChain(start)
	if walked {
		// A link's text may spell the worktree through a symlinked directory, so
		// the final path is canonicalized before it is compared with the worktree.
		if canonical, cok := s.resolveTarget(final, s.cwd); cok {
			final = canonical
		}
		return s.judgeNewScript(final, tool, stderr)
	}
	// The walk hit its cap, so the chain's end is unknown to it. A script name on
	// the way denies, whichever resolver then answers; otherwise the kernel-like
	// answer decides, and an unresolved path follows the unresolvable rule.
	for _, name := range chain {
		if scriptFileNamed(name) {
			printScriptFileRefusal(stderr, tool, name)
			return NewSilentExit(2)
		}
	}
	if ok {
		return s.judgeNewScript(target, tool, stderr)
	}
	return s.refuseUnresolvedTarget(raw, tool, stderr)
}

// maxWriteHops bounds how many symlinks followWriteChain follows itself. Past
// it the write's target is unknown, and the names on the chain decide.
const maxWriteHops = 8

// followWriteChain follows path's symlinks to the file a write to it creates.
// It returns the names it passed, the final path, and whether the chain ended
// within maxWriteHops. A dangling link ends the chain at its missing target,
// which is where the write lands.
func followWriteChain(path string) (chain []string, final string, walked bool) {
	p := filepath.Clean(path)
	chain = []string{p}
	for hop := 0; ; hop++ {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return chain, p, true
		}
		if hop == maxWriteHops {
			return chain, "", false
		}
		text, err := os.Readlink(p)
		if err != nil || text == "" {
			return chain, "", false
		}
		if !filepath.IsAbs(text) {
			text = filepath.Join(filepath.Dir(p), text)
		}
		p = filepath.Clean(text)
		chain = append(chain, p)
	}
}

// resolveTarget canonicalises raw against dir, reporting ok=false when the
// path cannot be resolved (an unknown variable, a command substitution).
func (s scriptFileScope) resolveTarget(raw, dir string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", false
	}
	target, ok := canonicalizeToolPath(s.proc, raw, dir)
	if !ok || target == "" {
		return "", false
	}
	return target, true
}

// judgeNewScript denies target when it is a new script file inside the
// worktree, and prints an advisory (never a denial) for a new Python file.
func (s scriptFileScope) judgeNewScript(target, via string, stderr io.Writer) error {
	if !isWithinPath(target, s.worktree) || !isNewFilePath(target) {
		// Outside the worktree, or an existing file being edited or re-created —
		// all allowed.
		return nil
	}
	if scriptFileExtension(target) {
		printScriptFileRefusal(stderr, via, target)
		return NewSilentExit(2)
	}
	if pythonFileExtension(target) {
		fmt.Fprintf(stderr, "script-files: %s creates a new Python file %s — record in the bead why Python is the best tool for this.\n", via, target)
	}
	return nil
}

// refuseUnresolvedTarget is the fail-closed reading of a path the guard cannot
// place: a name ending in a denied script suffix is still a request to create
// a script, so it is refused; any other name is allowed, because the guard
// denies only what it can name (gt-b43ra).
func (s scriptFileScope) refuseUnresolvedTarget(raw, via string, stderr io.Writer) error {
	if !scriptFileNamed(raw) {
		return nil
	}
	printScriptFileRefusal(stderr, via, raw)
	return NewSilentExit(2)
}

// scriptFileNamed reports whether raw's own text ends in a denied script
// suffix. It is read even when the path cannot be resolved, because the request
// to create a script is visible in the name alone.
func scriptFileNamed(raw string) bool {
	return scriptFileExtension(strings.Trim(strings.TrimSpace(raw), `"'`))
}

// scriptFileExtension reports whether path ends in one of the denied script
// suffixes.
func scriptFileExtension(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, denied := range scriptFileExtensions {
		if ext == denied {
			return true
		}
	}
	return false
}

// pythonFileExtension reports whether path ends in the allowed .py suffix.
func pythonFileExtension(path string) bool {
	return strings.EqualFold(filepath.Ext(path), scriptFilePythonExtension)
}

// isNewFilePath reports whether path does not exist yet, which is what makes
// creating it a new script rather than an edit of an existing one.
func isNewFilePath(path string) bool {
	if isDanglingSymlink(path) {
		return true
	}
	_, err := os.Lstat(path)
	return err != nil
}

// isDanglingSymlink reports whether path is a symlink whose target does not
// exist. Writing it creates the target, so it is a new file, not an existing
// one, and the name the agent chose is the name to judge (gt-b43ra).
func isDanglingSymlink(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	_, statErr := os.Stat(path)
	return statErr != nil
}

// printScriptFilePolicy writes the redirect under a denial, indented as the
// continuation of the ❌ line.
func printScriptFilePolicy(w io.Writer) {
	for _, line := range strings.Split(scriptFilePolicy, "\n") {
		fmt.Fprintf(w, "   %s\n", line)
	}
}

// printScriptFileRefusal is the denial: it names the redirect the model is to
// take, not just the ban.
func printScriptFileRefusal(w io.Writer, via, target string) {
	fmt.Fprintf(w, "\n❌ NO NEW SCRIPTS: %s (%s) would create a new script file.\n", target, via)
	printScriptFilePolicy(w)
	fmt.Fprintln(w, "   Editing an existing script and removing one are allowed.")
}

// unjudgedPayload is the answer to a payload the guard cannot parse: it denies
// only when the raw payload names a denied script suffix, the one fact left to
// read from it (gt-b43ra).
func unjudgedPayload(input []byte, stderr io.Writer, why string) error {
	// A Bash or Monitor call is not judged by this guard at all, even when its
	// payload cannot be read: the tool name is the one fact the guard can see.
	if payloadNamesShellTool(string(input)) {
		return nil
	}
	if !payloadNamesScriptSuffix(string(input)) {
		fmt.Fprintf(stderr, "script-files: %s, and it names no script, so this call is allowed.\n", why)
		return nil
	}
	printScriptFileUnjudgeable(stderr, why)
	return NewSilentExit(2)
}

// unjudgedWorktree is the answer when the session's own worktree cannot be
// placed: a file-writing call denies only a target whose name ends in a denied
// script suffix, and every other call is allowed (gt-b43ra).
func unjudgedWorktree(hook scriptFileInput, stderr io.Writer) error {
	target := hook.ToolInput.FilePath
	if hook.ToolName == "NotebookEdit" && hook.ToolInput.NotebookPath != "" {
		target = hook.ToolInput.NotebookPath
	}
	switch hook.ToolName {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if scriptFileNamed(target) {
			printScriptFileRefusal(stderr, hook.ToolName, target)
			return NewSilentExit(2)
		}
		// The write is allowed, but not silently: the warning names the cause
		// so an unjudged write shows up in the session's output (gt-b43ra).
		fmt.Fprintf(stderr, "script-files: WARNING: the session's worktree could not be placed, so %s to %s was not judged for new scripts.\n", hook.ToolName, target)
	}
	return nil
}

// payloadScriptSuffix matches a denied script suffix, in any case, ending a
// path-like word: the suffix followed by a quote, space, comma, brace or the
// end of the text.
var payloadScriptSuffix = regexp.MustCompile(`(?i)\.(sh|bash|zsh|pl|rb)(["'\s,}]|$)`)

// payloadShellTool matches a payload's tool name when it is a shell tool.
var payloadShellTool = regexp.MustCompile(`"tool_name"\s*:\s*"(Bash|Monitor)"`)

// payloadNamesShellTool reports whether raw text names a shell tool.
func payloadNamesShellTool(raw string) bool {
	return payloadShellTool.MatchString(raw)
}

// payloadNamesScriptSuffix reports whether raw text carries a denied script
// suffix as a path-like word. A bare substring would also match prose.
func payloadNamesScriptSuffix(raw string) bool {
	return payloadScriptSuffix.MatchString(raw)
}

// printScriptFileUnjudgeable denies a call the guard cannot judge. It fails
// closed on purpose (gt-b43ra): passing an unchecked call through is the hole
// this guard exists to close, so "cannot tell" reads as "no".
func printScriptFileUnjudgeable(w io.Writer, why string) {
	fmt.Fprintf(w, "\n❌ NO NEW SCRIPTS: %s, so this call is denied rather than passed through unchecked.\n", why)
	printScriptFilePolicy(w)
	fmt.Fprintln(w, "   If every call is denied this way, escalate to your witness: the hook harness is not delivering the payload.")
}
