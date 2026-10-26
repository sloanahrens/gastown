package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// scriptFileAllowlist names repo paths that legitimately end in a script
// extension — files that only look like scripts. It starts empty and is never
// added to (gt-b43ra): a new script is the debt this guard exists to stop, and
// the files a repo must hold under these names (a Makefile, a Dockerfile, CI
// YAML) do not carry one of these extensions at all.
var scriptFileAllowlist = []string{}

// scriptFileCreatingVerbs are the shell commands whose operands name a file
// they create. A redirection creates one the same way and is read off the raw
// line (redirectTargets), because the tokenizer keeps "echo x>y.sh" or the
// target of "cat > y.sh" from reaching an operand-shaped check.
var scriptFileCreatingVerbs = map[string]bool{
	"touch": true, "tee": true, "install": true,
}

// scriptVerbValueFlags are the options of scriptFileCreatingVerbs that own a
// value, so an option's value ("-m 755", "-t bin", "--date=<stamp>") is not
// read as a path the command creates.
var scriptVerbValueFlags = map[string]map[string]bool{
	"touch": {
		"-d": true, "--date": true, "-r": true, "--reference": true,
		"-t": true, "--time": true,
	},
	"tee": {},
	"install": {
		"-m": true, "--mode": true, "-o": true, "--owner": true,
		"-g": true, "--group": true, "-t": true, "--target-directory": true,
		"-S": true, "--suffix": true,
	},
}

var tapGuardScriptFilesCmd = &cobra.Command{
	Use:   "script-files",
	Short: "Deny a new script file in a worktree and redirect to testable Go",
	Long: `Deny creating a new shell, Perl or Ruby script in a Gas Town worker's worktree.

Edit/Write/MultiEdit/NotebookEdit are denied when the target is inside the
session's own worktree, does not exist yet, and ends in .sh, .bash, .zsh, .pl
or .rb. Bash and Monitor are denied when a redirection, touch, tee or install
would create such a file. Editing a script that already exists is allowed, and
removing one is always allowed.

A new .py file is allowed with an advisory on stderr asking for the reason to
be recorded in the bead: Python is allowed when it genuinely is the best tool,
so it gets a reminder rather than a denial.

The denial redirects: write a Go package with unit tests and a seam for the
clock, filesystem and process runner — ~/.claude/docs/standards/
test-engineering.md, "Scripts are debt". A procedure that is only operator
steps is a runbook, not a script.

Scope: a polecat or crew session working inside its own Gas Town worktree
(<rig>/polecats/<name>/<repo> or <rig>/crew/<name>). Paths outside every
worktree are not judged, and a session that is neither a polecat nor crew is
never judged.

Exit codes:
  0 - Allowed (not a worker session, not a new script, or the target exists)
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
		// No payload to judge: a harness wrapper that drains stdin (gt-wisp-52y4)
		// must not turn every call into a denial.
		return nil
	}
	var hook scriptFileInput
	if err := json.Unmarshal(input, &hook); err != nil {
		return nil
	}
	scope, ok := resolveScriptFileScope(hook.Cwd, proc)
	if !ok {
		return nil // not a worker session in a worktree — outside this guard's remit
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
	case "Bash", "Monitor":
		// Monitor runs the same command shape as Bash, and was invisible to the
		// guards that matched a bare "Bash" (gt-vx2mm).
		return scope.checkBashCommand(hook.ToolInput.Command, 0, stderr)
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

// resolveScriptFileScope identifies the worker session and its worktree, or
// reports ok=false when the session is neither a polecat's nor a crew
// member's, or its worktree cannot be named.
func resolveScriptFileScope(payloadCwd string, proc guardProcess) (scriptFileScope, bool) {
	role, ok := scriptFileWorkerRole(proc)
	if !ok {
		return scriptFileScope{}, false
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
		// session runs in, found by walking up to its .git.
		if root, ok := gitWorktreeRoot(cwd); ok && isWorkerWorktreeForRole(root, role) {
			worktree = root
		}
	}
	if worktree == "" {
		return scriptFileScope{}, false
	}
	return scriptFileScope{worktree: worktree, cwd: cwd, proc: proc}, true
}

// scriptFileWorkerRole reports the role this guard polices for this process:
// a polecat or a crew member. GT_ROLE decides whenever it is set — a
// coordinator can carry a stale GT_POLECAT from having spawned polecats — as
// it does for the rest of the guard family.
func scriptFileWorkerRole(proc guardProcess) (Role, bool) {
	if role := strings.TrimSpace(proc.getenv("GT_ROLE")); role != "" {
		parsed, _, _ := parseRoleString(role)
		return parsed, parsed == RolePolecat || parsed == RoleCrew
	}
	switch {
	case proc.getenv("GT_POLECAT") != "":
		return RolePolecat, true
	case proc.getenv("GT_CREW") != "":
		return RoleCrew, true
	}
	return "", false
}

// gitWorktreeRoot returns the directory containing cwd's ".git" entry, which
// is the top of the checkout a crew member works in.
func gitWorktreeRoot(cwd string) (string, bool) {
	for dir := cwd; ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
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
	target, ok := s.resolveTarget(raw, s.cwd)
	if !ok {
		// This guard denies what it can positively name: an unresolvable path is
		// the polecat-paths guard's to refuse, not evidence of a new script here.
		return nil
	}
	return s.judgeNewScript(target, tool, stderr)
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
	if !isWithinPath(target, s.worktree) || scriptFileAllowlisted(target) || !isNewFilePath(target) {
		// Outside the worktree, allowlisted, or an existing file being edited or
		// re-created — all allowed.
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

// checkBashCommand judges a shell command line as starting in the session cwd.
func (s scriptFileScope) checkBashCommand(command string, depth int, stderr io.Writer) error {
	return s.checkBashFrom(command, s.cwd, true, depth, stderr)
}

// checkBashFrom judges command as a shell starting in base; cwdKnown is false
// when base is only a stand-in for a directory the guard could not place, so a
// relative target in it is not judged.
//
// Segment directories come from segmentWalkRoot, so "cd sub && touch x.sh" is
// judged in sub and "cd /tmp && touch x.sh" is scratch, not a worktree file.
func (s scriptFileScope) checkBashFrom(command, base string, cwdKnown bool, depth int, stderr io.Writer) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	if depth > maxSubstitutionDepth {
		return nil
	}
	stripped := stripHeredocBodies(command)
	tokens := shellTokenize(stripped)
	vars := shellVarAssignments(tokens)

	var dirs []string
	seen := make(map[string]bool)
	var segment []string
	start := 0
	judge := func(words []string, from int) error {
		dir, known := segmentWalkRoot(s.proc, tokens, from, base, vars)
		known = known && cwdKnown
		if !known {
			dir = base
		}
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
		return s.checkBashSegment(words, dir, known, depth, stderr)
	}
	for i, token := range tokens {
		if shellCommandSeparators[token] {
			if err := judge(segment, start); err != nil {
				return err
			}
			segment = nil
			start = i + 1
			continue
		}
		segment = append(segment, token)
	}
	if err := judge(segment, start); err != nil {
		return err
	}

	// A redirect target is read off the raw line (the tokenizer cannot see one
	// glued to its word), so it carries no segment of its own. With one
	// directory it is judged there exactly; after a cd the target's directory
	// is ambiguous, so only a path from a fixed root — where the reading cannot
	// depend on the cd — is judged rather than guessing.
	oneDir := len(dirs) == 1
	for _, target := range redirectTargets(stripped) {
		if !oneDir && !fixedRootPath(target) {
			continue
		}
		for _, dir := range dirs {
			if err := s.judgeBashTarget(target, dir, true, "redirection", stderr); err != nil {
				return err
			}
		}
	}

	// A heredoc fed to a shell is a body of commands the shell runs, not data:
	// "bash <<'EOF' touch x.sh EOF" creates the file the plain line would. The
	// body runs in the directory the shell reached by the time it read the
	// heredoc (heredocBodyCwd), not necessarily where the line started.
	for _, span := range shellFedHeredocSpans(command) {
		bodyCwd, placed := heredocBodyCwd(s.proc, command, span, base)
		known := placed && cwdKnown
		if !known {
			bodyCwd = base
		}
		if err := s.checkBashFrom(span.body, bodyCwd, known, depth+1, stderr); err != nil {
			return err
		}
	}
	return nil
}

// checkBashSegment judges one shell segment's command word and arguments.
func (s scriptFileScope) checkBashSegment(segment []string, dir string, known bool, depth int, stderr io.Writer) error {
	word, args := segmentCommandWordPastRedirections(trimShellKeywords(segment))
	if word == "" {
		return nil
	}
	base := strings.ToLower(filepath.Base(word))
	if scriptFileCreatingVerbs[base] {
		for _, target := range createdScriptTargets(base, args) {
			if err := s.judgeBashTarget(target, dir, known, base, stderr); err != nil {
				return err
			}
		}
	}
	switch {
	case isInterpreterCommand(base):
		// A shell's -c payload is a command line of its own: "bash -c 'touch
		// x.sh'" creates the file the plain line would.
		if payload, ok := shellPayload(base, args); ok {
			return s.checkBashFrom(payload, dir, known, depth+1, stderr)
		}
	case base == "eval" && len(args) > 0:
		// eval runs its joined arguments as a command line.
		return s.checkBashFrom(strings.Join(args, " "), dir, known, depth+1, stderr)
	}
	return nil
}

// judgeBashTarget decides one path a command line creates.
func (s scriptFileScope) judgeBashTarget(raw, dir string, known bool, via string, stderr io.Writer) error {
	if !known && !fixedRootPath(raw) {
		return nil
	}
	target, ok := s.resolveTarget(raw, dir)
	if !ok {
		return nil
	}
	return s.judgeNewScript(target, via, stderr)
}

// fixedRootPath reports whether raw names a path from a fixed root, so the
// directory the shell stands in does not change what it resolves to.
func fixedRootPath(raw string) bool {
	return strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "~")
}

// createdScriptTargets returns the paths one creating verb names. touch and
// tee create every operand; install creates its last operand, or each operand
// under -t DIR.
func createdScriptTargets(verb string, args []string) []string {
	if verb == "install" && (hasShortFlag(args, 'd') || slices.Contains(args, "--directory")) {
		return nil // install -d creates directories, not files
	}
	operands := scriptVerbOperands(verb, args)
	if verb != "install" {
		return operands
	}
	if targetDir := installTargetDir(args); targetDir != "" {
		targets := make([]string, 0, len(operands))
		for _, operand := range operands {
			targets = append(targets, filepath.Join(targetDir, filepath.Base(operand)))
		}
		return targets
	}
	if len(operands) == 0 {
		return nil
	}
	return operands[len(operands)-1:]
}

// scriptVerbOperands returns the non-flag words of args, stepping over each
// option's value.
func scriptVerbOperands(verb string, args []string) []string {
	values := scriptVerbValueFlags[verb]
	var operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return append(operands, args[i+1:]...)
		case strings.HasPrefix(arg, "--"):
			if name, _, attached := strings.Cut(arg, "="); values[name] && !attached && i+1 < len(args) {
				i++
			}
		case len(arg) > 1 && arg[0] == '-':
			switch {
			case values[arg]:
				i++ // the value is the next word ("-m", "755")
			case len(arg) > 2 && values[arg[:2]]:
				// the value is attached ("-m755", "-tbin")
			}
		default:
			operands = append(operands, arg)
		}
	}
	return operands
}

// installTargetDir returns the directory install's -t/--target-directory
// names, or "".
func installTargetDir(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-t" || arg == "--target-directory":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(arg, "--target-directory="):
			return strings.TrimPrefix(arg, "--target-directory=")
		case strings.HasPrefix(arg, "-t") && len(arg) > 2:
			return arg[2:]
		}
	}
	return ""
}

// hasShortFlag reports whether args carry the short option letter, alone or in
// a cluster ("-d", "-ad").
func hasShortFlag(args []string, letter byte) bool {
	for _, arg := range args {
		if len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.IndexByte(arg[1:], letter) >= 0 {
			return true
		}
	}
	return false
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
	_, err := os.Lstat(path)
	return err != nil
}

// scriptFileAllowlisted reports whether target is a path the allowlist exempts.
func scriptFileAllowlisted(target string) bool {
	for _, allowed := range scriptFileAllowlist {
		if isWithinPath(target, allowed) {
			return true
		}
	}
	return false
}

// printScriptFileRefusal is the denial: it names the redirect the model is to
// take, not just the ban.
func printScriptFileRefusal(w io.Writer, via, target string) {
	fmt.Fprintf(w, "\n❌ NO NEW SCRIPTS: %s (%s) would create a new script file.\n", target, via)
	fmt.Fprintln(w, "   Write a Go package with unit tests and seams for the clock, filesystem and")
	fmt.Fprintln(w, "   process runner: ~/.claude/docs/standards/test-engineering.md, \"Scripts are debt\".")
	fmt.Fprintln(w, "   A procedure that is only operator steps is a runbook, not a script.")
	fmt.Fprintln(w, "   Editing an existing script and removing one are allowed.")
}
