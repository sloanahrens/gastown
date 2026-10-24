package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/workspace"
)

// bdCreateStoreRefusal is the line a polecat reads when its bd create would
// file into the town store.
const bdCreateStoreRefusal = "file it in your rig: cd to your worktree"

// bdCreateNestingRefusal is the verdict for a nested shell past
// maxTestGuardNestDepth that may hold a bd create: the guard will not name the
// directory it runs in, so it cannot tell which store it would file into.
const bdCreateNestingRefusal = "shells nested too deep to name the store this bd create would file into; run it in its own Bash call"

// bdCreatePresenceDepth bounds bdCreateSomewhere's walk through nested shell
// text. It is larger than maxTestGuardNestDepth because the presence check is
// what the depth cap consults: stopping at the same depth would miss the
// create the cap exists to refuse.
const bdCreatePresenceDepth = 16

var tapGuardBdCreateStoreCmd = &cobra.Command{
	Use:   "bd-create-store",
	Short: "Block a polecat's bd create that would file into the town (hq) store",
	Long: `Block a polecat's bd create when the effective working directory resolves to
the town's beads store.

bd picks its store from the directory it runs in: inside a polecat worktree it
is the rig's store, at the town root (~/gt on a host, /sandbox/gt in a sandbox)
it is the town's hq store. A work bug filed there is invisible to the rig's
dispatcher. The effective directory is the hook payload's cwd moved by the cd
segments that carry to a later command on the same shell line — not one in a
subshell, pipeline or background job — and a cd this guard cannot place before
a bd create is refused. bd's -C/--directory, before or after the verb, moves
the directory the store is resolved from; one the guard cannot place is
refused the same way. bd create's --repo names the workspace the bead is
filed into, and bd's --db the database whose enclosing .beads directory is the
store; each overrides the directory and BEADS_DIR, both are judged when both are
given, and a value the guard cannot place is refused. BEADS_DIR names the store
instead when bd sees it: an inline assignment (env or command may prefix it), or
the value the shell leaves exported, from the process env or an earlier export on
the line that no later unset has cleared. BEADS_DB and BD_DB name a database the
way --db does, and yield to --db and --repo. A store variable the guard cannot
follow to this bd create (changed after &&, ||, a pipe, a group, a subshell or a
compound command, or by eval, source, set -a or read) refuses it, as does a
directory with no .beads above it. gt
escalate, gt ask and gt done create town beads inside gt itself and never reach
this hook.

Exit codes:
  0 - Allowed (not a polecat, not a bd create, or the store is the rig's)
  2 - BLOCKED`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return tapGuardBdCreateStore(os.Stdin, os.Stderr, realGuardProcess())
	},
}

func init() {
	tapGuardCmd.AddCommand(tapGuardBdCreateStoreCmd)
}

func tapGuardBdCreateStore(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
	if err != nil {
		// Unreadable payload: no command to judge, the same reading the other
		// guards give their own input errors.
		return nil
	}
	command := extractCommand(input)
	if command == "" || !isPolecatContext(proc) {
		return nil
	}
	cwd := payloadCwd(input)
	if !filepath.IsAbs(cwd) {
		if cwd, err = proc.getwd(); err != nil {
			return nil
		}
	}
	townRoot, err := workspace.Find(cwd)
	if err != nil || strings.TrimSpace(townRoot) == "" {
		// Outside a town there is no town store to confuse the rig's with.
		return nil
	}
	g := bdCreateStoreGuard{proc: proc, townStore: filepath.Join(townRoot, ".beads")}
	reason := g.evaluate(command, cwd, true, 0)
	if reason == "" {
		return nil
	}
	fmt.Fprintf(stderr, "\n❌ BD CREATE BLOCKED: %s\n   %s (a bead filed in the town store is invisible to your rig)\n\n", reason, bdCreateStoreRefusal)
	return NewSilentExit(2)
}

type bdCreateStoreGuard struct {
	proc      guardProcess
	townStore string
}

// evaluate judges command as a shell starting in cwd, one segment at a time.
// cwdKnown is false for a -c payload whose directory the walk could not place;
// nothing in that payload is then read against a directory the shell never
// named, and a bd create in it is refused as unplaceable.
//
// Segment directories come from segmentWalkRoot, so a cd in a subshell,
// pipeline or background job is not carried to the commands after it, and an
// "&&" list whose cd fails does not name a directory its remainder never
// reaches.
func (g bdCreateStoreGuard) evaluate(command, cwd string, cwdKnown bool, depth int) string {
	stripped := strings.TrimSpace(stripHeredocBodies(command))
	tokens := shellTokenize(stripped)
	vars := shellVarAssignments(tokens)
	env := g.newBdEnvState()
	judge := func(segment []string, start int) string {
		segment = commandSegment(segment)
		dir, known := segmentWalkRoot(g.proc, tokens, start, cwd, vars)
		known = known && cwdKnown
		if reason := g.judgeSegment(segment, dir, known, vars, env); reason != "" {
			return reason
		}
		payloads := segmentPayloads(tokens, start, segment)
		if depth >= maxTestGuardNestDepth {
			if bdCreateInScripts(payloads, depth) {
				return bdCreateNestingRefusal
			}
			return ""
		}
		if !known {
			dir = cwd
		}
		for _, payload := range payloads {
			if reason := g.evaluate(payload, dir, known, depth+1); reason != "" {
				return reason
			}
		}
		return ""
	}

	var segment []string
	start := 0
	sep := ""
	for i, tok := range tokens {
		if shellCommandSeparators[tok] {
			if reason := judge(segment, start); reason != "" {
				return reason
			}
			env.apply(segment, sep)
			segment = nil
			sep = tok
			start = i + 1
			continue
		}
		segment = append(segment, tok)
	}
	if reason := judge(segment, start); reason != "" {
		return reason
	}

	scripts := substitutionScripts(g.proc, stripped, cwd, cwdKnown)
	if depth >= maxTestGuardNestDepth {
		for _, script := range scripts {
			if bdCreateSomewhere(script.body, depth) {
				return bdCreateNestingRefusal
			}
		}
		return ""
	}
	for _, script := range scripts {
		if reason := g.evaluate(script.body, script.dir, script.known, depth+1); reason != "" {
			return reason
		}
	}
	return ""
}

// judgeSegment judges one simple command, filed in dir (known false when the
// walk could not place it).
func (g bdCreateStoreGuard) judgeSegment(segment []string, dir string, known bool, vars map[string]string, env *bdEnvState) string {
	segment = commandSegment(segment)
	if !isBdCreateSegment(segment) {
		return ""
	}
	if env.unplaced {
		return bdCreateEnvUnplaced
	}
	bdDir, changesDir := bdDirectoryFlag(segment)
	if changesDir {
		// bd changes to the -C directory before resolving its store, so the
		// store is read from there, not from the shell's directory.
		path, placed := g.bdCreateBase(dir, known, bdDir, vars)
		if !placed {
			return bdCreateUnplaceable
		}
		dir, known = path, true
	}
	// bd create --repo names the workspace the bead is filed into, and bd --db
	// the database whose enclosing .beads directory is the store. --repo
	// outranks BEADS_DIR and the BEADS_DB/BD_DB environment, but not an explicit
	// --db: bd opens that store even when --repo is given, so a --repo that
	// files into the rig does not make a --db naming the town safe. Both are
	// judged, and a value the guard cannot place is refused.
	if repo, ok := bdValueFlag(segment, "--repo"); ok {
		path, placed := g.bdCreateBase(dir, known, repo, vars)
		if !placed {
			return bdCreateUnplaceable
		}
		if reason := g.judgeStore(path); reason != "" {
			return reason
		}
		if db, ok := bdValueFlag(segment, "--db"); ok {
			path, placed := g.bdCreateBase(dir, known, db, vars)
			if !placed {
				return bdCreateUnplaceable
			}
			return g.judgeDB(path)
		}
		return ""
	}
	if db, ok := bdValueFlag(segment, "--db"); ok {
		path, placed := g.bdCreateBase(dir, known, db, vars)
		if !placed {
			return bdCreateUnplaceable
		}
		return g.judgeDB(path)
	}
	if changesDir {
		// -C sets BEADS_DIR to the new directory, and bd looks at BEADS_DIR
		// before BEADS_DB, so neither an inline or process BEADS_DIR nor a
		// BEADS_DB/BD_DB can move the store off the -C directory.
		return g.judgeWorkdirStore(dir)
	}
	// BEADS_DB, then BD_DB, name a database the way --db does, ahead of the
	// directory. bd reads BEADS_DIR before BEADS_DB in one lookup and after it
	// in another, so a BEADS_DIR that is also set is judged too and either
	// naming the town refuses.
	beadsDir := g.bdEnvValue(segment, env, "BEADS_DIR")
	if envDB, set := g.envDBSelector(segment, env); set {
		path, placed := g.bdCreateBase(dir, known, envDB, vars)
		if !placed {
			return bdCreateUnplaceable
		}
		if reason := g.judgeDB(path); reason != "" {
			return reason
		}
		if beadsDir == "" {
			return ""
		}
	}
	return g.judgeCreate(dir, known, beadsDir)
}

// envDBSelector returns the value of the database-selecting variable bd would
// act on: BEADS_DB when it is non-empty, else BD_DB. An inline assignment
// replaces the process env's value for that name, and an empty value is an
// unset one, as bd reads it with os.Getenv.
func (g bdCreateStoreGuard) envDBSelector(segment []string, env *bdEnvState) (value string, set bool) {
	for _, name := range []string{"BEADS_DB", "BD_DB"} {
		if v := g.bdEnvValue(segment, env, name); v != "" {
			return v, true
		}
	}
	return "", false
}

// bdEnvValue returns the value bd reads for the environment variable name when
// a command runs in segment: an inline assignment on the segment, one that env
// or command prefixes still reach, else what the shell leaves exported. An
// inline or exported empty value is read as set and empty, as the shell replaces
// the env's value with it. An unset name is "".
func (g bdCreateStoreGuard) bdEnvValue(segment []string, env *bdEnvState, name string) string {
	if v, inline := inlineAssignment(segment, name); inline {
		return v
	}
	return env.exportedValue(name)
}

// bdCreateUnplaceable refuses a bd create whose store directory this guard
// cannot work out, rather than reading it against a directory the command
// never named.
const bdCreateUnplaceable = "a cd before this bd create has a target this guard cannot resolve; use a literal path, or run the bd create in its own Bash call"

// bdCreateEnvUnplaced refuses a bd create that runs after a store variable
// changed in a form the guard cannot follow to it.
const bdCreateEnvUnplaced = "a store variable (BEADS_DIR, BEADS_DB, BD_DB) changed earlier on this line in a form this guard cannot follow to this bd create; run the bd create in its own Bash call"

// bdCreateNoStore refuses a bd create whose directory has no .beads above it:
// bd finds no store there, so the guard cannot name the one it would file into.
const bdCreateNoStore = "no beads store is above this bd create's directory, so the guard cannot tell which store it would file into; run it from your worktree"

// bdCreateBase resolves the directory bd is told to run in (a -C value). A
// relative one is resolved against dir, as bd resolves it from the shell's
// directory, so an unplaced dir leaves it unplaceable. A value that still
// names an unexpanded variable cannot be placed either — it is refused, not
// read as a literal path name.
func (g bdCreateStoreGuard) bdCreateBase(dir string, known bool, target string, vars map[string]string) (string, bool) {
	target = resolveShellVar(target, vars)
	if target == "" {
		return "", false
	}
	path, ok := expandHomePath(g.proc, target)
	if !ok || strings.Contains(path, "$") {
		return "", false
	}
	if filepath.IsAbs(path) {
		return path, true
	}
	if !known {
		return "", false
	}
	return filepath.Join(dir, path), true
}

// judgeCreate refuses a bd create whose store is the town's. beadsDir is the
// BEADS_DIR bd reads ("": none); a relative one is resolved against the
// invocation's directory, as the shell resolves it, so an unknown directory
// leaves it unplaceable.
func (g bdCreateStoreGuard) judgeCreate(dir string, known bool, beadsDir string) string {
	if beadsDir == "" {
		if !known {
			return bdCreateUnplaceable
		}
		return g.judgeWorkdirStore(dir)
	}
	path, ok := expandHomePath(g.proc, beadsDir)
	if !ok || strings.Contains(path, "$") {
		return bdCreateUnplaceable
	}
	if !filepath.IsAbs(path) {
		if !known {
			return bdCreateUnplaceable
		}
		path = filepath.Join(dir, path)
	}
	return g.judgeStore(path)
}

// bdCreateTownRefusal is the verdict for a bd create whose store is the town's.
const bdCreateTownRefusal = "this bd create resolves to the town (hq) store"

// judgeStore refuses a workspace directory (the one holding .beads, or the
// .beads directory itself) whose store, through its redirect, is the town's.
func (g bdCreateStoreGuard) judgeStore(workspace string) string {
	if sameDir(beads.ResolveBeadsDir(workspace), g.townStore) {
		return bdCreateTownRefusal
	}
	return ""
}

// judgeDB judges bd's --db value. bd maps it to a store with
// resolveCommandBeadsDir: the first ancestor of the path's parent, nearest
// first, that holds a .beads directory, whether or not the path exists. So a
// file in .beads, the dolt directory under it, and a made-up name beside the
// town's .beads all select the town's store. The path is read as written and
// with its parent's symlinks resolved, since bd stats and canonicalizes both.
func (g bdCreateStoreGuard) judgeDB(dbPath string) string {
	candidates := []string{dbPath}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(dbPath)); err == nil {
		candidates = append(candidates, filepath.Join(parent, filepath.Base(dbPath)))
	}
	for _, path := range candidates {
		for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if info, err := os.Stat(filepath.Join(dir, ".beads")); err == nil && info.IsDir() {
				if reason := g.judgeStore(dir); reason != "" {
					return reason
				}
				break
			}
		}
	}
	return ""
}

// bdCreateVerbs are the bd subcommands that file a new bead.
var bdCreateVerbs = map[string]bool{"create": true, "new": true, "q": true}

// isBdCreateSegment reports whether segment's command word is bd and its
// subcommand one of bdCreateVerbs, so `bd list` and `gt escalate "bd create"`
// (which runs no bd) are not judged.
func isBdCreateSegment(segment []string) bool {
	tokens := bdCloseCommandTokens(segment)
	if len(tokens) == 0 || !isBdCommandWord(tokens[0]) {
		return false
	}
	rest := tokens[1:]
	if i := bdSubcommandIndex(rest); i >= 0 {
		return bdCreateVerbs[rest[i]]
	}
	return false
}

// inlineAssignment returns the value the last leading assignment of name in
// segment gives, and whether there was one. It reads past the clause and group
// openers bdCloseCommandTokens steps over, so `then BEADS_DIR=<town> bd create`
// is read too. Last wins, as the shell applies repeated assignments in order.
func inlineAssignment(segment []string, name string) (value string, ok bool) {
	tokens := segment
	for len(tokens) > 0 {
		tok := tokens[0]
		switch {
		case bdCloseLeadWords[tok] || tok == "command":
			tokens = tokens[1:]
			continue
		case filepath.Base(tok) == "env":
			// env's flags (-u NAME, -i, ...) are not assignments; the words after
			// them are, so `env BEADS_DB=<town> bd create` is read too.
			tokens = bdCloseSkipEnvFlags(tokens[1:])
			continue
		}
		if v, found := strings.CutPrefix(tok, name+"="); found {
			value, ok = v, true
		} else if !isEnvAssignment(tok) {
			break
		}
		tokens = tokens[1:]
	}
	return value, ok
}

// bdStoreEnvNames are the variables bd reads to pick its store.
var bdStoreEnvNames = []string{"BEADS_DIR", "BEADS_DB", "BD_DB"}

// bdCompoundWords open, close or branch a command the shell may not run past,
// so a store variable set after one is not known to be set when bd runs.
var bdCompoundWords = map[string]bool{"case": true, "esac": true, "for": true, "in": true, "select": true, "function": true, "fi": true, "done": true, ")": true}

// bdAssigningBuiltins are the builtins that set the variables they are given.
var bdAssigningBuiltins = map[string]bool{"read": true, "mapfile": true, "readarray": true, "printf": true, "getopts": true, "let": true, "declare": true, "typeset": true, "local": true, "readonly": true}

func isBdStoreEnvName(name string) bool {
	for _, store := range bdStoreEnvNames {
		if name == store {
			return true
		}
	}
	return false
}

// shellVar is a store variable as the shell leaves it: its value, and whether
// bd sees it, which needs it exported.
type shellVar struct {
	value    string
	exported bool
}

// bdEnvState is the shell's view of bdStoreEnvNames as a line runs in order. A
// name absent from vars is unset. conditional is set once a command may not run
// (after &&, ||, a pipe, a group, a subshell or a compound command). unplaced is
// set when a store variable changes in a way apply does not follow; the store bd
// uses is then unknown, so a bd create is refused.
type bdEnvState struct {
	vars        map[string]shellVar
	conditional bool
	unplaced    bool
}

// newBdEnvState seeds the state from the process env bd is started with, whose
// variables are all exported.
func (g bdCreateStoreGuard) newBdEnvState() *bdEnvState {
	env := &bdEnvState{vars: make(map[string]shellVar)}
	for _, name := range bdStoreEnvNames {
		if v := g.proc.getenv(name); v != "" {
			env.vars[name] = shellVar{value: v, exported: true}
		}
	}
	return env
}

// exportedValue returns the value bd reads for name from the environment: ""
// when the shell does not export it.
func (env *bdEnvState) exportedValue(name string) string {
	if v := env.vars[name]; v.exported {
		return v.value
	}
	return ""
}

// apply moves env past segment, the simple command that follows the separator
// sep ("" at the line's start). Only a store variable's export, unset or bare
// assignment changes what bd reads. The guard follows one the shell surely runs;
// one it may not run, or cannot read, leaves the state unplaced.
func (env *bdEnvState) apply(segment []string, sep string) {
	if sep == "&&" || sep == "||" || sep == "|" || sep == "&" || bdCompoundOpens(segment) {
		env.conditional = true
	}
	words := bdCloseCommandTokens(commandSegment(segment))
	if !bdStoreEffect(segment, words) {
		return
	}
	if env.conditional || !bdFollowable(words) {
		env.unplaced = true
		return
	}
	switch {
	case len(words) == 0:
		for _, tok := range segment {
			if name, value, _ := strings.Cut(tok, "="); isEnvAssignment(tok) && isBdStoreEnvName(name) {
				v := env.vars[name]
				v.value = value
				env.vars[name] = v
			}
		}
	case words[0] == "export":
		env.export(words[1:])
	case words[0] == "unset":
		for _, name := range words[1:] {
			delete(env.vars, name)
		}
	}
}

// export applies an export's operands: NAME=value sets the value and exports
// it, NAME exports the value the shell already holds, and -n clears the export.
func (env *bdEnvState) export(args []string) {
	unexport := false
	for _, word := range args {
		if word == "-n" {
			unexport = true
			continue
		}
		name, value, hasValue := strings.Cut(word, "=")
		if !isBdStoreEnvName(name) {
			continue
		}
		v := env.vars[name]
		if hasValue {
			v.value = value
		}
		v.exported = !unexport
		env.vars[name] = v
	}
}

// bdStoreEffect reports whether a simple command, segment with words after its
// leading assignments, may change a store variable or the shell in a way the
// guard cannot read. A command word the guard does not know is not an effect.
func bdStoreEffect(segment, words []string) bool {
	if len(words) == 0 {
		for _, tok := range segment {
			if name, _, _ := strings.Cut(tok, "="); isEnvAssignment(tok) && isBdStoreEnvName(name) {
				return true
			}
		}
		return false
	}
	switch name := words[0]; {
	case name == "eval" || name == "source" || name == ".":
		return true
	case name == "set":
		return bdSetsAllExport(words[1:])
	case name == "export" || name == "unset" || bdAssigningBuiltins[name]:
		return bdNamesStoreVar(words[1:])
	}
	return false
}

// bdFollowable reports whether apply follows a store effect with these words: a
// bare assignment, or an export (-n only) or unset with no flags.
func bdFollowable(words []string) bool {
	if len(words) == 0 {
		return true
	}
	if words[0] != "export" && words[0] != "unset" {
		return false
	}
	for _, word := range words[1:] {
		if strings.HasPrefix(word, "-") && !(words[0] == "export" && word == "-n") {
			return false
		}
	}
	return true
}

// bdCompoundOpens reports whether segment holds a group, subshell, conditional
// or loop word, after which the shell may not run the commands that follow.
func bdCompoundOpens(segment []string) bool {
	for _, tok := range segment {
		if bdCloseLeadWords[tok] || bdCompoundWords[tok] {
			return true
		}
	}
	return false
}

// bdSetsAllExport reports whether set's operands turn allexport on or off (-a,
// or -o allexport), which exports every later assignment.
func bdSetsAllExport(args []string) bool {
	for _, word := range args {
		if word == "allexport" {
			return true
		}
		if (strings.HasPrefix(word, "-") || strings.HasPrefix(word, "+")) && strings.Contains(word[1:], "a") {
			return true
		}
	}
	return false
}

// bdNamesStoreVar reports whether one of args is a store variable, as NAME or
// NAME=value.
func bdNamesStoreVar(args []string) bool {
	for _, word := range args {
		if name, _, _ := strings.Cut(word, "="); isBdStoreEnvName(name) {
			return true
		}
	}
	return false
}

// judgeWorkdirStore judges the store bd resolves from directory dir. bd walks up
// from dir to the nearest .beads, so a town subdirectory with no .beads of its
// own resolves to the town's store.
func (g bdCreateStoreGuard) judgeWorkdirStore(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if info, err := os.Stat(filepath.Join(d, ".beads")); err == nil && info.IsDir() {
			return g.judgeStore(d)
		}
		if filepath.Dir(d) == d {
			return bdCreateNoStore
		}
	}
}

// bdDirectoryFlag returns the directory a bd create's -C/--directory names.
func bdDirectoryFlag(segment []string) (value string, ok bool) {
	return bdValueFlag(segment, "--directory", "-C")
}

// bdCreateValueFlags are the flags of bd create, and the global flags bd
// accepts beside it, that take their value as the next token (--flag value or
// -x value). bdValueFlag steps over that value, so one that starts with -C or
// spells another flag, as in `--title -Cfix`, is not read as a flag of its
// own. Each name is in `bd create --help`; a boolean flag stays out, since
// stepping over the token after it would hide a real -C.
var bdCreateValueFlags = map[string]bool{
	"--acceptance": true, "--append-notes": true, "-a": true, "--assignee": true,
	"--body-file": true, "--context": true, "--defer": true, "--deps": true,
	"-d": true, "--description": true, "--design": true, "--design-file": true,
	"--due": true, "-e": true, "--estimate": true, "--event-actor": true,
	"--event-category": true, "--event-payload": true, "--event-target": true,
	"--external-ref": true, "-f": true, "--file": true, "--graph": true,
	"--id": true, "-l": true, "--labels": true, "--metadata": true,
	"--mol-type": true, "--notes": true, "--parent": true, "-p": true,
	"--priority": true, "--repo": true, "--skills": true, "--spec-id": true,
	"-s": true, "--status": true, "--storage-class": true, "--title": true,
	"-t": true, "--type": true, "--waits-for": true, "--waits-for-gate": true,
	"--wisp-type": true, "--actor": true, "--database": true, "--db": true,
	"--directory": true, "-C": true, "--dolt-auto-commit": true,
	"--mem-profile": true,
}

// bdValueFlag returns the value a bd create segment gives a flag, wherever it
// sits relative to the verb, in the forms pflag accepts: a separate value,
// --long=value, and for a flag with a shorthand, -C=value and the glued
// -Cvalue. The value of any other flag in bdCreateValueFlags is stepped over,
// as pflag consumes it, so it is never read as a flag. A repeated flag is read
// last-wins, as pflag sets it. ok is false only when the segment names no such
// flag; a flag with no value reaches the caller as the empty value, which it
// refuses rather than reading the create from the shell's directory.
func bdValueFlag(segment []string, long string, short ...string) (value string, ok bool) {
	tokens := bdCloseCommandTokens(segment)
	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "--" {
			break
		}
		switch {
		case tok == long || len(short) > 0 && tok == short[0]:
			value, ok = "", true
			if i+1 < len(tokens) && tokens[i+1] != "--" {
				value = tokens[i+1]
				i++
			}
		case strings.HasPrefix(tok, long+"="):
			value, ok = strings.TrimPrefix(tok, long+"="), true
		case len(short) > 0 && strings.HasPrefix(tok, short[0]):
			value, ok = strings.TrimPrefix(strings.TrimPrefix(tok, short[0]), "="), true
		case bdCreateValueFlags[tok]:
			i++ // the flag's value, not a flag of its own
		}
	}
	return value, ok
}

// bdCreateInScripts reports whether any of the nested command lines holds a bd
// create, the reading the depth cap needs before it refuses a shell it will
// not walk.
func bdCreateInScripts(scripts []string, depth int) bool {
	for _, script := range scripts {
		if bdCreateSomewhere(script, depth) {
			return true
		}
	}
	return false
}

// bdCreateSomewhere reports whether command, or a shell payload or command
// substitution it runs, spells a bd create in any segment. It does not follow
// cd — it answers one question, so a deeply nested command that runs no bd
// create is allowed rather than blocked for its nesting alone.
func bdCreateSomewhere(command string, depth int) bool {
	if depth > bdCreatePresenceDepth {
		return false
	}
	stripped := strings.TrimSpace(stripHeredocBodies(command))
	tokens := shellTokenize(stripped)
	var segment []string
	judge := func() bool {
		segment = commandSegment(segment)
		if isBdCreateSegment(segment) {
			return true
		}
		return bdCreateInScripts(shellCPayloads(segment), depth+1)
	}
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			if judge() {
				return true
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	if judge() {
		return true
	}
	return bdCreateInScripts(commandSubstitutions(stripped), depth+1)
}

// sameDir compares two directories, following symlinks that exist (a macOS
// temp dir is under /var, a symlink to /private/var).
func sameDir(a, b string) bool {
	if r, err := filepath.EvalSymlinks(a); err == nil {
		a = r
	}
	if r, err := filepath.EvalSymlinks(b); err == nil {
		b = r
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
