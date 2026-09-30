package convoy

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
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

	// Run answers the town's bd calls in process; nil runs the bd on PATH.
	Run beads.BDRunner
	// gtRun runs the town's gt notice children; nil runs the gt on PATH.
	gtRun gtRunner
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

// bdJSON runs bd in dir with the town's environment.
func (t Town) bdJSON(dir string, args ...string) ([]byte, error) {
	return beads.RunBdJSONWith(beads.BdJSONOptions{Env: t.Env, Run: t.Run}, dir, args...)
}

func (t Town) bdJSONAllowStale(dir string, args ...string) ([]byte, error) {
	return beads.RunBdJSONWith(beads.BdJSONOptions{Env: t.Env, AllowStale: true, Run: t.Run}, dir, args...)
}

func (t Town) bdJSONAutoCommit(dir string, args ...string) ([]byte, error) {
	return beads.RunBdJSONWith(beads.BdJSONOptions{Env: t.Env, AutoCommit: true, Run: t.Run}, dir, args...)
}

// bd builds a bd command that starts from the town's environment.
func (t Town) bd(args ...string) *beads.BdCmd {
	c := beads.NewBdCmd(args...)
	if t.Env != nil {
		c.WithEnv(t.Env)
	}
	return c.Via(t.Run).Stderr(t.warnWriter())
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
