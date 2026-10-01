package convoy

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
)

// Town is the town a convoy operation runs against.
type Town struct {
	// Root is the town root; bd runs from it.
	Root string
	// Env is the base environment for the bd and gt children; nil is
	// os.Environ(). The daemon sets it to its routing environment.
	Env []string
	// Out receives the progress lines a command prints; nil discards them.
	Out io.Writer
	// Warn receives warnings; nil discards them.
	Warn io.Writer

	// Open opens the issue store at dir (the town root or a rig directory);
	// nil opens bd pinned to dir's database, starting from Env. Unit tests
	// answer from beadsfake databases.
	Open func(dir string) Store
	// Issues answers the tracked-issue lookups, which bd routes to each
	// ID's rig by prefix; nil is bd at the town root.
	Issues beads.Client
	// Prefixes maps rigs to the session prefixes the stranded scan probes
	// assignees' sessions under; nil gives every rig session.DefaultPrefix.
	Prefixes *session.PrefixRegistry
	// gtRun runs the town's gt notice children; nil runs the gt on PATH.
	gtRun gtRunner
}

// Store is the issue store surface the convoy operations use: the shared
// Client plus the JSONL export and the raw dependency query.
// *beads.Beads and beadsfake.Fake implement it.
type Store interface {
	beads.Client
	Export(path string) error
	SQLCSV(query beadsql.Query) ([][]string, error)
}

// gtRunner runs gt with args from dir with exactly env (nil inherits the
// process environment).
type gtRunner func(dir string, env []string, args ...string) error

// runGT runs the gt on PATH.
func runGT(dir string, env []string, args ...string) error {
	cmd := exec.Command("gt", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.Run()
}

// StdTown is a Town whose output goes to the terminal: what the CLI uses.
func StdTown(root string) Town {
	return Town{Root: root, Out: os.Stdout, Warn: os.Stderr}
}

// store opens the issue store at dir.
func (t Town) store(dir string) Store {
	if t.Open != nil {
		return t.Open(dir)
	}
	return beads.NewPinned(beads.ResolveBeadsDir(dir), beads.WithEnv(t.Env))
}

func (t Town) outWriter() io.Writer {
	if t.Out == nil {
		return io.Discard
	}
	return t.Out
}

func (t Town) warnWriter() io.Writer {
	if t.Warn == nil {
		return io.Discard
	}
	return t.Warn
}

func (t Town) printf(format string, args ...any) {
	fmt.Fprintf(t.outWriter(), format, args...)
}

func (t Town) warnf(format string, args ...any) {
	style.FprintWarning(t.warnWriter(), format, args...)
}

// rootDir returns the town root, accepting the town's .beads directory too.
func (t Town) rootDir() string {
	if filepath.Base(filepath.Clean(t.Root)) == ".beads" {
		return filepath.Dir(filepath.Clean(t.Root))
	}
	return t.Root
}
