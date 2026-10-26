// Command gt-installer is scripts/install-gt.sh in Go (gt-tpg0p, decision Q3:
// make install builds and runs this program). Nothing calls it yet; the
// Makefile switch is the last bead (gt-tpg0p.14).
//
//	gt-installer [--sha REF] [--source manual|rebuild-gt|post-land]
//	             [--slot-role ROLE] [--slot-timeout SECONDS] [--dry-run]
//	             [--no-image-first]
//
// The install sections land one bead at a time: target resolution, the no-op
// path and the refusals are here (gt-tpg0p.10). Until the build section lands
// (gt-tpg0p.11), a run that gets that far ends in the exit guard as
// failed/unexpected with status 1, so a caller wired to this program early
// fails loudly rather than believing an install happened.
package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/installer"
)

func main() {
	// Every gt this process runs is the installer's unless the caller named an
	// actor, so the usage log and escalations never read "unknown" (gt-kyik6).
	if os.Getenv("BD_ACTOR") == "" {
		_ = os.Setenv("BD_ACTOR", "installer")
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// environment is what a run reads from the process: its variables, its working
// directory and its home. A test supplies its own, so it never rewrites the
// test process's environment.
type environment struct {
	getenv func(string) string
	getwd  func() (string, error)
	home   func() (string, error)
}

// run is the process's one way out. The deferred OnExit is the script's EXIT
// trap: whatever happens, the last line states a result and the status is 0-3.
func run(args []string, stdout, stderr io.Writer) int {
	return runWith(args, stdout, stderr, environment{os.Getenv, os.Getwd, os.UserHomeDir})
}

// runWith is run against env, so a test can point every directory at a
// temporary tree.
func runWith(args []string, stdout, stderr io.Writer, env environment) (code int) {
	cfg, err := installer.Parse(args)
	c := &installer.Contract{
		Stdout:   stdout,
		Stderr:   stderr,
		Expected: cfg.SHA,
		Source:   cfg.Source,
		Start:    time.Now(),
	}
	defer func() { code = int(c.OnExit(installer.Exit(code))) }()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return int(installer.ExitFailed)
	}
	home, err := env.home()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return int(installer.ExitFailed)
	}
	cwd, err := env.getwd()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return int(installer.ExitFailed)
	}
	paths, err := installer.ResolvePaths(env.getenv, home, cwd)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return int(installer.ExitFailed)
	}
	c.Receipts = paths.Receipts()
	c.GT = paths.GT()

	// --dry-run prints the gateway plan and changes nothing; that step lands in
	// gt-tpg0p.9, so until then the guard ends the run before the checkout is
	// read.
	if cfg.DryRun {
		return int(installer.ExitFailed)
	}

	r := installer.NewRun(cfg, c, paths)
	if code, ended := r.ResolveTarget(); ended {
		return int(code)
	}
	if code, ended := r.Noop(); ended {
		return int(code)
	}
	if code, ended := r.Refuse(); ended {
		return int(code)
	}
	if err := r.KeepPrevious(); err != nil {
		fmt.Fprintln(stderr, err)
		return int(installer.ExitFailed)
	}
	// The build, install, signing and smoke checks land in gt-tpg0p.11.
	return int(installer.ExitFailed)
}
