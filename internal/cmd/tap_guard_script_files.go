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

// scriptFilePolicy is the whole redirect this guard delivers, in one constant
// so the denial message, the unreadable-payload denial and the tests cannot
// drift apart (gt-b43ra).
const scriptFilePolicy = `Write a Go package with unit tests and seams for the clock, filesystem and
process runner: ~/.claude/docs/standards/test-engineering.md, "Scripts are debt".
A procedure that is only operator steps is a runbook, not a script.
Limit: a Bash path the shell builds at runtime is judged only when its command
names a script suffix; this guard is best effort for Bash and exact for files.`

// scriptFileCreatingVerbs are the shell commands whose operands name a file
// they create. A redirection creates one the same way, read from the same
// segment's tokens (redirectFileTargets).
var scriptFileCreatingVerbs = map[string]bool{
	"touch": true, "tee": true, "install": true, "cp": true, "mv": true, "ln": true,
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
	"cp": targetDirectoryValueFlags,
	"mv": targetDirectoryValueFlags,
	"ln": targetDirectoryValueFlags,
}

// targetDirectoryValueFlags are the options cp, mv, ln and install share that
// own a value: -t/--target-directory names the directory the copies land in,
// and -S/--suffix is a backup-name suffix.
var targetDirectoryValueFlags = map[string]bool{
	"-t": true, "--target-directory": true, "-S": true, "--suffix": true,
}

var tapGuardScriptFilesCmd = &cobra.Command{
	Use:   "script-files",
	Short: "Deny a new script file in a worktree and redirect to testable Go",
	Long: `Deny creating a new shell, Perl or Ruby script in a Gas Town worker's worktree.

Edit/Write/MultiEdit/NotebookEdit are denied when the target is inside the
session's own worktree, does not exist yet, and ends in .sh, .bash, .zsh, .pl
or .rb. Bash and Monitor are denied when a redirection, touch, tee, install,
cp, mv or ln would create such a file, judged in the directory the shell has
reached at that point of the line (so a relative target after a cd is checked
where the cd went). Editing a script that already exists is allowed, and
removing one is always allowed.

The guard fails closed: a hook payload it cannot read or parse, or a worker
session whose own worktree it cannot place, is denied rather than passed
through unjudged. A path that cannot be resolved is denied when its name ends
in a denied script suffix, since the request is still to create a script.

A new .py file is allowed with an advisory on stderr asking for the reason to
be recorded in the bead: Python is allowed when it genuinely is the best tool,
so it gets a reminder rather than a denial.

The denial redirects: ` + scriptFilePolicy + `

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
		// Fail closed (gt-b43ra): a payload the guard cannot read names no
		// call, and "I saw nothing" is not evidence that no script is being
		// written.
		printScriptFileUnjudgeable(stderr, "the hook payload could not be read")
		return NewSilentExit(2)
	}
	var hook scriptFileInput
	if err := json.Unmarshal(input, &hook); err != nil {
		printScriptFileUnjudgeable(stderr, "the hook payload could not be parsed")
		return NewSilentExit(2)
	}
	scope, worker, ok := resolveScriptFileScope(hook.Cwd, proc)
	if !worker {
		return nil // not a worker session — outside this guard's remit
	}
	if !ok {
		printScriptFileUnjudgeable(stderr, "the session's own worktree could not be resolved")
		return NewSilentExit(2)
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
	// namesScript is set for the command line being judged when its text names
	// a denied suffix: a target built at runtime is then refused, not allowed.
	namesScript bool
}

// resolveScriptFileScope identifies the worker session and its worktree.
// worker reports whether the session is one this guard polices at all (a
// polecat or a crew member); ok reports whether its own worktree could be
// named. A worker whose tree cannot be named is a refusal, not an allow: the
// guard exists to stop a new script, and not being able to see where the
// session stands is not evidence that no script is being written (gt-b43ra).
func resolveScriptFileScope(payloadCwd string, proc guardProcess) (scope scriptFileScope, worker, ok bool) {
	role, worker := scriptFileWorkerRole(proc)
	if !worker {
		return scriptFileScope{}, false, false
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
		return scriptFileScope{}, true, false
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
		if root, ok := gitWorktreeRoot(cwd); ok && (role == "" || isWorkerWorktreeForRole(root, role)) {
			worktree = root
		}
	}
	if worktree == "" {
		return scriptFileScope{}, true, false
	}
	return scriptFileScope{worktree: worktree, cwd: cwd, proc: proc}, true, true
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
	// No identifying variable: the guard is installed only in worker and crew
	// settings, so the session is judged as a worker (fail closed, gt-b43ra).
	return "", true
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
		// A path this guard cannot resolve is the polecat-paths guard's to
		// refuse for its own scope; here the script suffix in the name is
		// still a request to create a script, so that request is refused.
		return s.refuseUnresolvedTarget(raw, tool, stderr)
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
	s.namesScript = commandNamesScriptSuffix(command)
	return s.checkBashFrom(command, s.cwd, true, depth, stderr)
}

// commandNamesScriptSuffix reports whether a command line's text carries a
// denied suffix as a literal. A target the shell builds at runtime is judged
// only when it does: this is the documented limit of the Bash judgement.
func commandNamesScriptSuffix(command string) bool {
	for _, denied := range scriptFileExtensions {
		if strings.Contains(command, denied) {
			return true
		}
	}
	return false
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
	tokens := shellTokenize(spaceOutOutputRedirections(stripped))
	vars := shellVarAssignments(tokens)

	var segment []string
	start := 0
	// judge reads one segment in the directory the shell has reached at that
	// point, so "cd bin && echo hi > gen.sh" is judged in bin. The segment's
	// own verb and its redirections are judged against the same directory.
	judge := func(words []string, from int) error {
		dir, known := segmentWalkRoot(s.proc, tokens, from, base, vars)
		known = known && cwdKnown
		if !known {
			dir = base
		}
		if err := s.checkBashSegment(words, dir, known, depth, stderr); err != nil {
			return err
		}
		for _, target := range redirectFileTargets(words) {
			if err := s.judgeBashTarget(target, dir, known, "redirection", stderr); err != nil {
				return err
			}
		}
		return nil
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
		for _, target := range createdScriptTargets(base, args, dir) {
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
	target, ok := s.resolveTarget(raw, dir)
	if !ok {
		return s.refuseUnresolvedTarget(raw, via, stderr)
	}
	if !known && !fixedRootPath(raw) {
		// The shell's directory is unknown, so this relative target may name
		// any tree: a script-shaped name is refused rather than guessed,
		// because either guess is a hole a model can aim at.
		return s.refuseUnresolvedTarget(raw, via, stderr)
	}
	return s.judgeNewScript(target, via, stderr)
}

// refuseUnresolvedTarget is the fail-closed reading of a path the guard cannot
// place: a name ending in a denied script suffix is still a request to create
// a script, so it is refused; any other name is allowed, because the guard
// denies only what it can name (gt-b43ra).
func (s scriptFileScope) refuseUnresolvedTarget(raw, via string, stderr io.Writer) error {
	if !scriptFileNamed(raw) && !s.namesScript {
		return nil
	}
	printScriptFileRefusal(stderr, via, raw)
	return NewSilentExit(2)
}

// fixedRootPath reports whether raw names a path from a fixed root, so the
// directory the shell stands in does not change what it resolves to.
func fixedRootPath(raw string) bool {
	return strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "~")
}

// spaceOutOutputRedirections separates an output redirection from the words
// around it, so a target glued to its operator or its command ("echo
// hi>gen.sh", "cat >gen.sh") becomes a token of its own. shellTokenize leaves
// that glue inside one word, which kept redirectFileTargets from seeing the
// target at all (gt-b43ra).
//
// Quoted and escaped ">" stays untouched (it is data, not an operator), and an
// "&" that belongs to the operator ("&>log", "2>&1") stays attached to it.
func spaceOutOutputRedirections(command string) string {
	var b strings.Builder
	runes := []rune(command)
	var quote rune
	last := rune(0)
	write := func(r rune) {
		b.WriteRune(r)
		last = r
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			write(r)
			if r == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				write(runes[i])
				continue
			}
			if r == quote {
				quote = 0
			}
		case r == '\\' && i+1 < len(runes):
			write(r)
			i++
			write(runes[i])
		case r == '\'' || r == '"':
			quote = r
			write(r)
		case r == '>':
			if last != 0 && last != ' ' && last != '\t' && last != '\n' && last != '&' {
				write(' ')
			}
			for i < len(runes) && runes[i] == '>' {
				write('>')
				i++
			}
			i-- // the operator's last rune; the loop's i++ steps past it
			if i+1 < len(runes) && runes[i+1] == '|' {
				i++
				write('|')
			}
			if i+1 < len(runes) && runes[i+1] == '&' {
				i++
				write('&')
			}
			write(' ')
		default:
			write(r)
		}
	}
	return b.String()
}

// redirectFileTargets returns the files the output redirections in one
// segment's tokens create, in shell order. A redirection that reads ("<",
// "<<") or duplicates a descriptor (">&2", ">&-") creates no file and is
// skipped. Reading the redirects from the segment, rather than off the whole
// line, is what lets a redirect after a cd be judged in the directory the cd
// reached (gt-b43ra).
func redirectFileTargets(tokens []string) []string {
	var targets []string
	for i := 0; i < len(tokens); i++ {
		width := redirectionWidth(tokens[i])
		if width == 0 {
			continue
		}
		operator, glued := splitRedirection(tokens[i])
		target := glued
		if target == "" && width == 2 && i+1 < len(tokens) {
			target = tokens[i+1]
		}
		i += width - 1
		if !strings.Contains(operator, ">") || target == "" || isFileDescriptorWord(target) {
			continue
		}
		targets = append(targets, target)
	}
	return targets
}

// splitRedirection returns a redirection token's operator and the target glued
// to it ("2>>log" -> ">>", "log"); the target is "" when it is a word of its
// own ("<<- EOF").
func splitRedirection(token string) (operator, target string) {
	i := 0
	if strings.HasPrefix(token, "{") {
		end := strings.IndexByte(token, '}')
		if end < 0 {
			return "", ""
		}
		i = end + 1
	} else {
		for i < len(token) && token[i] >= '0' && token[i] <= '9' {
			i++
		}
	}
	if i+1 < len(token) && token[i] == '&' && token[i+1] == '>' {
		i++
	}
	if i >= len(token) || (token[i] != '<' && token[i] != '>') {
		return "", ""
	}
	start := i
	for i < len(token) && (token[i] == '<' || token[i] == '>') {
		i++
	}
	if i < len(token) && token[i] == '|' && token[start:i] == ">" {
		i++
	}
	if i < len(token) && token[i] == '&' {
		i++
	}
	return token[start:i], token[i:]
}

// createdScriptTargets returns the paths one creating verb names. touch and tee
// create every operand. install, cp, mv and ln create a destination: each
// operand under -t DIR, or the last operand — with each source's own name when
// that destination is an existing directory.
func createdScriptTargets(verb string, args []string, dir string) []string {
	args = withoutRedirections(args)
	if verb == "install" && (hasShortFlag(args, 'd') || slices.Contains(args, "--directory")) {
		return nil // install -d creates directories, not files
	}
	operands := scriptVerbOperands(verb, args)
	targetDir := ""
	if verb != "touch" && verb != "tee" { // touch -t takes a time, tee takes no -t
		targetDir = targetDirectoryFlag(args)
	}
	if targetDir != "" {
		targets := make([]string, 0, len(operands))
		for _, operand := range operands {
			targets = append(targets, filepath.Join(targetDir, filepath.Base(operand)))
		}
		return targets
	}
	switch verb {
	case "touch", "tee":
		return operands
	case "ln":
		switch len(operands) {
		case 0:
			return nil
		case 1:
			// "ln -s target" names the link after the target, in the cwd.
			return []string{filepath.Base(operands[0])}
		default:
			return operands[len(operands)-1:]
		}
	}
	if len(operands) == 0 {
		return nil
	}
	dest := operands[len(operands)-1]
	if len(operands) > 1 && isExistingDirectoryUnder(dir, dest) {
		targets := make([]string, 0, len(operands)-1)
		for _, source := range operands[:len(operands)-1] {
			targets = append(targets, filepath.Join(dest, filepath.Base(source)))
		}
		return targets
	}
	return []string{dest}
}

// withoutRedirections returns args with every redirection and its target
// removed, so an operand scan does not read a redirect's file as an operand.
// The redirections are judged separately, by redirectFileTargets.
func withoutRedirections(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		width := redirectionWidth(args[i])
		if width == 0 {
			out = append(out, args[i])
			continue
		}
		i += width - 1
	}
	return out
}

// isExistingDirectoryUnder reports whether path, read relative to dir when it is
// relative, names a directory that exists — which is what makes a copy's last
// operand a destination for the sources rather than the file they become.
func isExistingDirectoryUnder(dir, path string) bool {
	if !filepath.IsAbs(path) && dir != "" {
		path = filepath.Join(dir, path)
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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

// targetDirectoryFlag returns the directory -t/--target-directory names, or "".
func targetDirectoryFlag(args []string) string {
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

// printScriptFileUnjudgeable denies a call the guard cannot judge. It fails
// closed on purpose (gt-b43ra): passing an unchecked call through is the hole
// this guard exists to close, so "cannot tell" reads as "no".
func printScriptFileUnjudgeable(w io.Writer, why string) {
	fmt.Fprintf(w, "\n❌ NO NEW SCRIPTS: %s, so this call is denied rather than passed through unchecked.\n", why)
	printScriptFilePolicy(w)
	fmt.Fprintln(w, "   If every call is denied this way, escalate to your witness: the hook harness is not delivering the payload.")
}
