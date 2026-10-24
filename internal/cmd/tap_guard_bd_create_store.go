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
given, and a value the guard cannot place is refused. BEADS_DIR, in the process
env, as an inline assignment (env or command may prefix it) or exported earlier
on the line, names the store instead, and so do BEADS_DB and BD_DB, database
paths read as --db is and yielding to --db and --repo. gt
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
	exported := shellExportedAssignments(tokens)
	judge := func(segment []string, start int) string {
		segment = commandSegment(segment)
		dir, known := segmentWalkRoot(g.proc, tokens, start, cwd, vars)
		known = known && cwdKnown
		if reason := g.judgeSegment(segment, dir, known, vars, exported); reason != "" {
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
	for i, tok := range tokens {
		if shellCommandSeparators[tok] {
			if reason := judge(segment, start); reason != "" {
				return reason
			}
			segment = nil
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
func (g bdCreateStoreGuard) judgeSegment(segment []string, dir string, known bool, vars map[string]string, exported map[string]string) string {
	segment = commandSegment(segment)
	if !isBdCreateSegment(segment) {
		return ""
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
	beadsDir := g.bdEnvValue(segment, exported, "BEADS_DIR")
	if envDB, set := g.envDBSelector(segment, exported); set {
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
func (g bdCreateStoreGuard) envDBSelector(segment []string, exported map[string]string) (value string, set bool) {
	for _, name := range []string{"BEADS_DB", "BD_DB"} {
		if v := g.bdEnvValue(segment, exported, name); v != "" {
			return v, true
		}
	}
	return "", false
}

// bdEnvValue returns the value bd reads for the environment variable name when
// a command runs in segment: an inline assignment on the segment, one that env
// or command prefixes still reach, else an export earlier on the same line,
// else the process env. An inline or exported empty value is read as set and
// empty, as the shell replaces the env's value with it. An unset name is "".
func (g bdCreateStoreGuard) bdEnvValue(segment []string, exported map[string]string, name string) string {
	if v, inline := inlineAssignment(segment, name); inline {
		return v
	}
	if v, ok := exported[name]; ok {
		return v
	}
	return g.proc.getenv(name)
}

// bdCreateUnplaceable refuses a bd create whose store directory this guard
// cannot work out, rather than reading it against a directory the command
// never named.
const bdCreateUnplaceable = "a cd before this bd create has a target this guard cannot resolve; use a literal path, or run the bd create in its own Bash call"

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
// inline BEADS_DIR ("": none); a relative one is resolved against the
// invocation's directory, as the shell resolves it, so an unknown directory
// leaves it unplaceable.
func (g bdCreateStoreGuard) judgeCreate(dir string, known bool, beadsDir string) string {
	if beadsDir == "" {
		beadsDir = g.proc.getenv("BEADS_DIR")
		if beadsDir == "" {
			if !known {
				return bdCreateUnplaceable
			}
			return g.judgeWorkdirStore(dir)
		}
	}
	path, ok := expandHomePath(g.proc, beadsDir)
	if !ok {
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

// shellExportedAssignments returns the NAME=value assignments that an export
// earlier on the same line marks for export, so a bd create that follows runs
// with them. A plain assignment that is not exported is left out: bd never sees
// it. A later export of the same name wins, as the shell applies them in order.
func shellExportedAssignments(tokens []string) map[string]string {
	exported := make(map[string]string)
	start := 0
	for i := 0; i <= len(tokens); i++ {
		if i < len(tokens) && !shellCommandSeparators[tokens[i]] {
			continue
		}
		words := bdCloseCommandTokens(tokens[start:i])
		if len(words) > 0 && words[0] == "export" {
			for _, word := range words[1:] {
				if isEnvAssignment(word) {
					name, value, _ := strings.Cut(word, "=")
					exported[name] = value
				}
			}
		}
		start = i + 1
	}
	return exported
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
			return ""
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
