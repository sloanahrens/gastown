// Package bdshim routes a bd command to the store that owns it: a Dolt rig
// keeps the real bd (bd.real, beside this binary), and a tracker rig answers
// through a verb handler registered here. cook and the formula verbs need no
// store without cook's --persist, so they reach bd.real from a tracker rig
// too; a tracker verb with no handler and no such rule is refused.
package bdshim

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	// TownRoot is the town the walk found, set when Tracker is true. A handler
	// opens the rig's store from it, so it never walks a second time and cannot
	// disagree with the route it was handed.
	TownRoot string
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
	// Now reads the wall clock a handler dates its decision with; nil is the
	// process's clock (time.Now). A test injects a fixed instant, so a handler
	// that tells a future deferral from a past one is not answering differently
	// on the day it runs.
	Now func() time.Time
	// Handlers is the tracker verb registry.
	Handlers map[string]Handler
	// Stdout takes a tracker handler's answer, in bd's shape; a command a
	// handler does not answer writes nothing here, since bd.real keeps the
	// process's own stdout.
	Stdout io.Writer
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

	// A flag that opens a store bd.real would work on moves the command off the
	// store the working directory names, and nothing here follows it, so while
	// a tracker rig exists it is refused rather than routed by the wrong
	// directory. It is checked before the passthrough below, because cook opens
	// a store with --persist and that store would be out of this routing's
	// reach.
	if flag := storeFlag(args); flag != "" && hasTrackerRoute(routes) {
		return Decision{}, fmt.Errorf("%s selects a store this shim cannot route while a tracker rig exists: run bd from the rig's own directory", flag)
	}

	// cook and the formula verbs read formula files and need no store
	// (docs/research/tracker/R1-cook-port.md), so bd.real answers them from a
	// tracker rig directory. They are decided before any bead id routes them
	// because neither acts on a bead: a cook's formula file or --var value can
	// open with a rig prefix without naming one, and a formula subcommand's
	// operand is a formula name or a path. Routing such an operand would send a
	// store-free command to the tracker, which has no handler for it.
	if passesThrough(commandPath(args)) {
		return d, nil
	}

	command := commandPath(args)
	candidates := idCandidates(command, args)
	ids := beadIDs(candidates)
	// An operand or a bead-naming flag value that opens with a tracker rig's
	// prefix names that rig even when IsBeadIDToken rejects its form: a form the
	// token check does not know must not slip through to bd.real from a Dolt
	// directory.
	ids = append(ids, trackerPrefixed(candidates, ids, routes)...)
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
			d.Tracker, d.Route, d.TownRoot = true, r, town
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
			d.Tracker, d.Route, d.TownRoot = true, r, town
			return d, nil
		}
	}
	return d, nil
}

// passThroughVerbs are the commands a tracker rig hands to bd.real instead of
// answering or refusing: cook and the formula verbs read formula files and the
// town overlay, and cook opens a store only with --persist, which the store
// guard above refuses (docs/research/tracker/R1-cook-port.md). mol, wisp and
// gate are not here: their handlers wait on recorded goldens, so those verbs
// stay refused until then (gt-p85pw.101).
var passThroughVerbs = map[string]bool{
	"cook":    true,
	"formula": true,
}

// passesThrough reports whether command is one a tracker rig hands to
// bd.real. A formula subcommand passes with its verb: commandPath opens the
// path with the verb and resolves an alias to the command it stands for.
func passesThrough(command string) bool {
	verb, _, _ := strings.Cut(command, " ")
	return passThroughVerbs[verb]
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

// storeFlag returns the first bd flag in args that opens a store bd.real
// would work on, or "" when there is none: -C, --directory and --db select
// one on any command, and --persist makes cook open the store it otherwise
// skips. A bool flag set to false (--persist=false) leaves the store closed,
// so it is not one; a value bd cannot parse is reported as opening it, since
// a store is then out of this routing's reach. A token after the -- separator
// is an operand and never a flag.
func storeFlag(args []string) string {
	command := commandPath(args)
	for _, arg := range args {
		if arg == "--" {
			return ""
		}
		name, value, inline := strings.Cut(arg, "=")
		switch name {
		case "-C", "--directory", "--db":
			return name
		case "--persist":
			if command != "cook" {
				continue
			}
			if inline && !persistOn(value) {
				continue
			}
			return name
		}
	}
	return ""
}

// persistOn reports whether cook's --persist value asks for a persistent
// molecule, which opens the store. A bare flag is true; only an explicit
// false turns it off, as bd's pflag parser reads a bool. A value it cannot
// parse is true: bd would reject it, and until it does the store it opens is
// out of this shim's reach.
func persistOn(value string) bool {
	on, err := strconv.ParseBool(value)
	return err != nil || on
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

// trackerPrefixed returns the candidates, not already ids, that open with a
// tracker rig's prefix.
func trackerPrefixed(candidates, ids []string, routes []beads.Route) []string {
	known := make(map[string]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}
	var out []string
	for _, candidate := range candidates {
		if known[candidate] {
			continue
		}
		for _, r := range routes {
			if r.BackendName() == beads.BackendTracker && r.Prefix != "" && strings.HasPrefix(candidate, r.Prefix) {
				out = append(out, candidate)
				break
			}
		}
	}
	return out
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
