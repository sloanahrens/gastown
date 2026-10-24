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
refused the same way. BEADS_DIR, in the process env or as an inline
assignment, names the store instead. gt escalate, gt ask and gt done create
town beads inside gt itself and never reach this hook.

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
	judge := func(segment []string, start int) string {
		segment = commandSegment(segment)
		dir, known := segmentWalkRoot(g.proc, tokens, start, cwd, vars)
		known = known && cwdKnown
		if reason := g.judgeSegment(segment, dir, known, vars); reason != "" {
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
func (g bdCreateStoreGuard) judgeSegment(segment []string, dir string, known bool, vars map[string]string) string {
	segment = commandSegment(segment)
	if !isBdCreateSegment(segment) {
		return ""
	}
	if bdDir, ok := bdDirectoryFlag(segment); ok {
		// bd changes to the -C directory before resolving its store, so the
		// store is read from there, not from the shell's directory.
		path, placed := g.bdCreateBase(dir, known, bdDir, vars)
		if !placed {
			return bdCreateUnplaceable
		}
		dir, known = path, true
	}
	return g.judgeCreate(dir, known, inlineBeadsDir(segment))
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
			beadsDir = dir
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
	beadsDir = path
	if sameDir(beads.ResolveBeadsDir(beadsDir), g.townStore) {
		return "this bd create resolves to the town (hq) store"
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

// inlineBeadsDir returns the BEADS_DIR a segment's leading assignments name,
// looking past the clause and group openers bdCloseCommandTokens steps over so
// `then BEADS_DIR=<town> bd create` is read too.
func inlineBeadsDir(segment []string) string {
	for _, tok := range segment {
		if bdCloseLeadWords[tok] {
			continue
		}
		if value, ok := strings.CutPrefix(tok, "BEADS_DIR="); ok {
			return value
		}
		if !isEnvAssignment(tok) {
			return ""
		}
	}
	return ""
}

// bdDirectoryFlag returns the directory a bd create's -C/--directory names,
// wherever it sits relative to the verb, in the forms pflag accepts: a
// separate value, --directory=value, -C=value and the glued -Cvalue. A
// repeated flag is read last-wins, as pflag sets it. ok is false only when the
// segment names no such flag; a flag with no value reaches the caller as the
// empty value, which it refuses rather than reading the create from the
// shell's directory.
func bdDirectoryFlag(segment []string) (value string, ok bool) {
	tokens := bdCloseCommandTokens(segment)
	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "--" {
			break
		}
		switch {
		case tok == "-C" || tok == "--directory":
			value, ok = "", true
			if i+1 < len(tokens) && tokens[i+1] != "--" {
				value = tokens[i+1]
				i++
			}
		case strings.HasPrefix(tok, "--directory="):
			value, ok = strings.TrimPrefix(tok, "--directory="), true
		case strings.HasPrefix(tok, "-C"):
			value, ok = strings.TrimPrefix(strings.TrimPrefix(tok, "-C"), "="), true
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
