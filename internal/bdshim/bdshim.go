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

	ids := idTokens(args)
	named := make(map[string]beads.Route, len(ids))
	for _, id := range ids {
		for _, r := range routes {
			if r.Prefix == beads.ExtractPrefix(id) {
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
		case err == nil && info.IsDir():
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

// verbOf returns bd's subcommand: the first token that is not a flag. A global
// flag written with a separate value shifts it, and the shim then refuses
// naming that value, which is loud where a guessed verb would be silent.
func verbOf(args []string) string {
	for _, arg := range args {
		if arg != "--" && !isFlag(arg) {
			return arg
		}
	}
	return ""
}

// idTokens returns the arguments that can name a bead. A token right after a
// flag is that flag's value and never an id, so `bd create --title gt-x` names
// no bead and a `--parent gt-x` does not route its own create.
func idTokens(args []string) []string {
	var ids []string
	value := false // the token before this one was a flag, so this one is its value
	for _, arg := range args {
		switch {
		case arg == "--": // past -- every token is positional
			value = false
		case isFlag(arg):
			_, _, inline := strings.Cut(arg, "=")
			value = !inline
		case value:
			value = false
		case beads.IsBeadIDToken(arg) && beads.ExtractPrefix(arg) != "":
			ids = append(ids, arg)
		}
	}
	return ids
}

// isFlag reports whether arg is bd's flag rather than a value or a verb.
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
