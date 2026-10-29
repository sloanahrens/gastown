// Package bdshim routes a bd command to the store that owns it: a Dolt rig
// keeps the real bd (bd.real, beside this binary), and a tracker rig answers
// through a verb handler registered here. X3-X6 of gt-p85pw add the handlers;
// this skeleton ships none, so every tracker verb is refused.
package bdshim

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// realName is the Dolt binary a command for a Dolt rig passes through to. It
// sits beside the shim, so a PATH holding the shim still reaches bd.
const realName = "bd.real"

// beadsDirEnv is bd's own override for the store a command works on.
const beadsDirEnv = "BEADS_DIR"

// Handler answers one bd verb for a tracker rig and returns its exit code.
type Handler func(env Env, d Decision) int

// Decision is the store one bd command belongs to.
type Decision struct {
	// Verb is bd's subcommand, empty when argv names none.
	Verb string
	// Tracker says the verb runs in this binary; false passes the command to
	// bd.real unchanged.
	Tracker bool
	// Route is the tracker rig, set when Tracker is true.
	Route beads.Route
}

// Env is the process surface Main reads, so a test drives the real entry
// point with its own argv, environment and child process.
type Env struct {
	// Args is bd's argv, without the program name.
	Args []string
	// Getenv reads the environment bd would see; BEADS_DIR selects the store.
	Getenv func(string) string
	// Getwd reports the working directory a command without an id routes by.
	Getwd func() (string, error)
	// Executable is the shim's own path, beside which bd.real sits.
	Executable func() (string, error)
	// Run execs bd.real with the given path and argv; a child's exit status
	// rides the returned error as ExitCode.
	Run func(path string, argv []string) error
	// Handlers is the tracker verb registry.
	Handlers map[string]Handler
	// Stderr takes the shim's own messages, never bd's.
	Stderr io.Writer
}

// Main routes one bd command and returns the process exit code.
func Main(env Env) int {
	if env.Stderr == nil {
		env.Stderr = io.Discard
	}
	d, err := Decide(env.Getenv, env.Getwd, env.Args)
	if err != nil {
		fmt.Fprintf(env.Stderr, "bdshim: %v\n", err)
		return 1
	}
	if d.Tracker {
		if h, ok := env.Handlers[d.Verb]; ok {
			return h(env, d)
		}
		fmt.Fprintln(env.Stderr, Refusal(d.Verb, d.Route.Prefix))
		return 1
	}
	exe, err := env.Executable()
	if err != nil {
		fmt.Fprintf(env.Stderr, "bdshim: locating %s: %v\n", realName, err)
		return 1
	}
	if err := env.Run(filepath.Join(filepath.Dir(exe), realName), env.Args); err != nil {
		if code := exitCode(err); code >= 0 {
			return code
		}
		fmt.Fprintf(env.Stderr, "bdshim: %s: %v\n", realName, err)
		return 1
	}
	return 0
}

// Decide returns the store a command belongs to, or an error when the args
// name a command no single store can answer or the route table cannot be
// classified.
func Decide(getenv func(string) string, getwd func() (string, error), args []string) (Decision, error) {
	cwd, err := getwd()
	if err != nil {
		return Decision{}, fmt.Errorf("reading the working directory: %w", err)
	}
	d := Decision{Verb: verbOf(args)}

	// bd reads a relative BEADS_DIR against the working directory.
	workdir := cwd
	if dir := getenv(beadsDirEnv); dir != "" {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		workdir = dir
	}

	found, err := findTown(workdir)
	if err != nil {
		return Decision{}, err
	}
	if found.table == "" {
		return d, nil // no route table: no rig here is flipped, bd.real answers
	}
	routes, err := loadRoutes(found.table)
	if err != nil {
		return Decision{}, err
	}
	town := filepath.Dir(found.table)

	// A flag that selects the store bd opens moves the command off the store the
	// working directory names, and nothing here follows it, so while a tracker
	// rig exists it is refused rather than routed by the wrong directory.
	if flag := storeFlag(args); flag != "" && hasTrackerRoute(routes) {
		return Decision{}, fmt.Errorf("%s selects a store this shim cannot route while a tracker rig exists: run bd from the rig's own directory", flag)
	}

	command := commandPath(args)
	ids := idTokens(command, args)
	// An operand that opens with a tracker rig's prefix names that rig even
	// when IsBeadIDToken rejects its form: a form the token check does not know
	// must not slip through to bd.real from a Dolt directory.
	ids = append(ids, trackerPrefixed(operands(command, args), ids, routes)...)
	named := make(map[string]beads.Route, len(ids))
	for _, id := range ids {
		for _, r := range routes {
			if r.Prefix == beads.ExtractPrefix(id) || (r.BackendName() == beads.BackendTracker && r.Prefix != "" && strings.HasPrefix(id, r.Prefix)) {
				named[routeBeadsDir(town, r)] = r
				break
			}
		}
	}
	if len(named) > 1 {
		for _, r := range named {
			if r.BackendName() == beads.BackendTracker {
				return Decision{}, fmt.Errorf("the args name beads in %d stores, one of them tracker; nothing here splits a command across stores", len(named))
			}
		}
	}
	for _, r := range named {
		if r.BackendName() == beads.BackendTracker {
			d.Tracker, d.Route = true, r
			return d, nil
		}
	}
	if len(ids) > 0 {
		return d, nil // an id no route names reaches the store bd answers for it today
	}

	// The command names no bead (list, ready, create), so the store the
	// working directory sits in decides.
	store := ""
	if found.home != "" {
		// The redirect a polecat worktree or crew directory keeps in its
		// .beads points at the rig's store.
		store = samePath(beads.ResolveBeadsDir(found.home))
	}
	for _, r := range routes {
		if r.BackendName() == beads.BackendTracker && samePath(routeBeadsDir(town, r)) == store {
			d.Tracker, d.Route = true, r
			return d, nil
		}
	}
	return d, nil
}

// Refusal is the message for a tracker verb this binary has no handler for.
// X7 pins its text, so this is the only place that builds one.
func Refusal(verb, prefix string) string {
	if verb == "" {
		verb = "(none)"
	}
	return fmt.Sprintf("bdshim: verb %s is not implemented for tracker rig %s", verb, prefix)
}

// town is what one walk up from a working directory finds.
type town struct {
	// table is the .beads directory holding the town's route table.
	table string
	// home is the nearest directory holding a .beads, whose store the command
	// works on when it names no bead.
	home string
}

// findTown walks up from start collecting the nearest .beads directory and the
// first route table above it. A directory it cannot read fails rather than
// being skipped: a skipped ancestor could hide the rig's flip, and the command
// would reach the Dolt store instead.
func findTown(start string) (town, error) {
	dir := filepath.Clean(start)
	if filepath.Base(dir) == ".beads" {
		dir = filepath.Dir(dir)
	}
	var found town
	for {
		store := filepath.Join(dir, ".beads")
		info, err := os.Stat(store)
		switch {
		case err == nil && !info.IsDir():
			// A .beads that is not a directory names no store, and walking past
			// it could hide the rig's route table, so it is refused.
			return town{}, fmt.Errorf("%s is not a directory", store)
		case err == nil:
			if found.home == "" {
				found.home = dir
			}
			// Lstat, so a table that exists but cannot be opened is read (and
			// refused by loadRoutes) rather than walked past.
			if _, err := os.Lstat(filepath.Join(store, beads.RoutesFileName)); err == nil {
				found.table = store
				return found, nil
			}
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return town{}, fmt.Errorf("looking for a store in %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return found, nil
		}
		dir = parent
	}
}

// loadRoutes reads the town's route table through beads.LoadRoutes, the one
// parser a town has, and rejects a backend the shim cannot classify: the
// backend is this binary's whole branch, and a guess would send a flipped
// rig's command to the Dolt store.
func loadRoutes(table string) ([]beads.Route, error) {
	routes, err := beads.LoadRoutes(table)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(table, beads.RoutesFileName), err)
	}
	for _, r := range routes {
		switch r.BackendName() {
		case beads.BackendDolt, beads.BackendTracker:
		default:
			return nil, fmt.Errorf("%s: prefix %s names backend %q, want %s or %s",
				filepath.Join(table, beads.RoutesFileName), r.Prefix, r.Backend, beads.BackendDolt, beads.BackendTracker)
		}
	}
	return routes, nil
}

// routeBeadsDir is the store a route names.
func routeBeadsDir(town string, r beads.Route) string {
	if r.Path == "." {
		return filepath.Join(town, ".beads")
	}
	return filepath.Join(town, r.Path, ".beads")
}

// samePath resolves symlinks before comparing, so a worktree reached through
// a symlinked root matches the route's own path.
func samePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// storeFlag returns the first bd flag in args that selects the store bd opens
// (-C, --directory or --db, bare or with =value), or "" when there is none. A
// token after the -- separator is an operand and never a flag.
func storeFlag(args []string) string {
	for _, arg := range args {
		if arg == "--" {
			return ""
		}
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "-C", "--directory", "--db":
			return name
		}
	}
	return ""
}

// hasTrackerRoute reports whether any route in the table is a tracker rig.
func hasTrackerRoute(routes []beads.Route) bool {
	for _, r := range routes {
		if r.BackendName() == beads.BackendTracker {
			return true
		}
	}
	return false
}

// trackerPrefixed returns the operands, not already ids, that open with a
// tracker rig's prefix.
func trackerPrefixed(ops, ids []string, routes []beads.Route) []string {
	known := make(map[string]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}
	var out []string
	for _, op := range ops {
		if known[op] {
			continue
		}
		for _, r := range routes {
			if r.BackendName() == beads.BackendTracker && r.Prefix != "" && strings.HasPrefix(op, r.Prefix) {
				out = append(out, op)
				break
			}
		}
	}
	return out
}

// valueFlags are the bd flags that take their value as the next token: every
// one bd 1.2.2 documents, long and short (`bd <command> --help`, recorded in
// testdata/bd-flags.txt). Any other flag is boolean, so the token after it
// stays an operand: `bd show --json bt-abc` names bt-abc. A flag missing here
// turns its value into an id, and an id no route names stops the command
// reaching its working directory's store (gt-w8j7v).
var valueFlags = map[string]bool{
	"--acceptance": true, "--actor": true, "--add": true,
	"--add-label": true, "--addr": true, "--after": true,
	"--age": true, "--agents-file": true, "--agents-profile": true,
	"--agents-template": true, "--allowed-host": true, "--append-notes": true,
	"--as": true, "--as-of": true, "--assignee": true,
	"--at": true, "--attach": true, "--attach-type": true,
	"--attempts": true, "--auth-token-file": true, "--author": true,
	"--await-id": true, "--backend": true, "--batch-size": true,
	"--before": true, "--blocks": true, "--blocked-by": true,
	"--body-file": true, "--branch": true, "--check": true,
	"--closed-after": true, "--closed-before": true, "--conflicting-file": true,
	"--context": true, "--coordinator": true, "--created-after": true,
	"--created-before": true, "--database": true, "--days": true,
	"--db": true, "--defer": true, "--defer-after": true,
	"--defer-before": true, "--depends-on": true, "--deps": true,
	"--desc-contains": true, "--description": true, "--design": true,
	"--design-file": true, "--destroy-token": true, "--direction": true,
	"--directory": true, "--doc": true, "--docs-root": true,
	"--dolt-auto-commit": true, "--due": true, "--due-after": true,
	"--due-before": true, "--error": true, "--estimate": true,
	"--event-actor": true, "--event-category": true, "--event-payload": true,
	"--event-target": true, "--exclude-label": true, "--exclude-owner": true,
	"--exclude-type": true, "--exit-code": true, "--external-contains": true,
	"--external-ref": true, "--file": true, "--for": true,
	"--format": true, "--from": true, "--from-file": true,
	"--gate-result": true, "--gate-tail": true, "--gate-tail-file": true,
	"--graph": true, "--group": true, "--has-metadata-key": true,
	"--holder": true, "--id": true, "--ids-file": true,
	"--if-assignee": true, "--if-status": true, "--include": true,
	"--input": true, "--issue": true, "--issue-id": true,
	"--key": true, "--kind": true, "--label": true,
	"--label-any": true, "--label-pattern": true, "--label-regex": true,
	"--labels": true, "--landed-commit": true, "--limit": true,
	"--max-age": true, "--max-depth": true, "--max-memories": true,
	"--max-memory-chars": true, "--max-rows": true, "--mem-profile": true,
	"--merged-into": true, "--message": true, "--metadata": true,
	"--metadata-field": true, "--method": true, "--migration": true,
	"--mode": true, "--model": true, "--mol": true,
	"--mol-type": true, "--notes": true, "--notes-contains": true,
	"--of": true, "--offset": true, "--older-than": true,
	"--om-findings": true, "--om-score": true, "--om-verdict": true,
	"--orchestrator-duplicates-threshold": true, "--output": true, "--parent": true,
	"--patch-id": true, "--path": true, "--pattern": true,
	"--payload": true, "--prefix": true, "--priority": true,
	"--priority-max": true, "--priority-min": true, "--prompt": true,
	"--query": true, "--range": true, "--reason": true,
	"--reason-file": true, "--ref": true, "--ref-kind": true,
	"--remote": true, "--remove-label": true, "--repo": true,
	"--response": true, "--role": true, "--route": true,
	"--search-path": true, "--server-host": true, "--server-port": true,
	"--server-socket": true, "--server-user": true, "--session": true,
	"--set-labels": true, "--set-metadata": true, "--since": true,
	"--skills": true, "--sort": true, "--source": true,
	"--source-db": true, "--spec": true, "--spec-id": true,
	"--status": true, "--storage-class": true, "--strategy": true,
	"--summary": true, "--table": true, "--threshold": true,
	"--tier": true, "--timeout": true, "--title": true,
	"--title-contains": true, "--to": true, "--tool-name": true,
	"--type": true, "--unset-metadata": true, "--until": true,
	"--updated-after": true, "--updated-before": true, "--var": true,
	"--waits-for": true, "--waits-for-gate": true, "--wisp-type": true,
	"--with": true, "--workers": true, "-C": true,
	"-a": true, "-b": true, "-d": true,
	"-e": true, "-f": true, "-i": true,
	"-l": true, "-m": true, "-n": true,
	"-o": true, "-p": true, "-r": true,
	"-s": true, "-t": true,
}

// boolFlags are the flags a command reads as a boolean, or as a value only
// after an =, although another command reads a value for the same one: -f is
// --file on create and --force on close. The command decides, so a token after
// one of these stays an operand.
var boolFlags = map[string]map[string]bool{
	"admin cleanup": {"-f": true},
	"close":         {"-f": true},
	"compact":       {"-f": true},
	"delete":        {"-f": true},
	"doctor":        {"-i": true},
	"flatten":       {"-f": true},
	"gate check":    {"-e": true},
	"gate discover": {"-n": true},
	"gate list":     {"-a": true},
	"gc":            {"-f": true},
	"list":          {"--deps": true, "-r": true},
	"mol show":      {"-p": true},
	"mol wisp gc":   {"-f": true},
	"orphans":       {"-f": true},
	"preflight":     {"--check": true},
	"prune":         {"-f": true},
	"purge":         {"-f": true},
	"query":         {"-a": true, "-r": true},
	"search":        {"-r": true},
	"setup":         {"--check": true},
}

// takesValue reports whether command reads the token after flag as its value.
func takesValue(command, flag string) bool {
	return valueFlags[flag] && !boolFlags[command][flag]
}

// verbOf returns bd's subcommand: the first operand. A global value flag is
// skipped with its value, so `bd --db hq list` names list.
func verbOf(args []string) string {
	if ops := operands("", args); len(ops) > 0 {
		return ops[0]
	}
	return ""
}

// commandPath is the bd command args open with: the verb, and the subverb when
// a flag's meaning differs below it. -f is a value flag on create and a
// boolean on close, so the command, not the verb alone, decides.
func commandPath(args []string) string {
	ops := operands("", args)
	if len(ops) == 0 {
		return ""
	}
	path := ops[0]
	for n := 2; n <= len(ops); n++ {
		if command := strings.Join(ops[:n], " "); boolFlags[command] != nil {
			path = command
		}
	}
	return path
}

// idTokens returns the operands that can name a bead. A token after a value
// flag is that flag's value and never an id, so `bd create --title gt-x` names
// no bead and `--parent gt-x` does not route its own create.
func idTokens(command string, args []string) []string {
	var ids []string
	for _, arg := range operands(command, args) {
		if beads.IsBeadIDToken(arg) && beads.ExtractPrefix(arg) != "" {
			ids = append(ids, arg)
		}
	}
	return ids
}

// operands returns the arguments bd reads as operands: not a flag, not the
// value of a value flag, and not the -- separator. Past -- every token is one.
func operands(command string, args []string) []string {
	var ops []string
	pastSeparator := false
	valueNext := false // the token before this one is a value flag, so this one is its value
	for _, arg := range args {
		switch {
		case valueNext:
			valueNext = false
		case pastSeparator:
			ops = append(ops, arg)
		case arg == "--":
			pastSeparator = true
		case isFlag(arg):
			name, _, inline := strings.Cut(arg, "=")
			valueNext = !inline && takesValue(command, name)
		default:
			ops = append(ops, arg)
		}
	}
	return ops
}

// isFlag reports whether arg is bd's flag rather than an operand.
func isFlag(arg string) bool {
	return len(arg) > 1 && arg[0] == '-'
}

// exitCode returns the status err carries, or -1 when it carries none, such
// as when bd.real could not be started.
func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}
