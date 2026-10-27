// Command gt-installer is scripts/install-gt.sh in Go (gt-tpg0p, decision Q3:
// make install builds and runs this program). Nothing calls it yet; the
// Makefile switch is the last bead (gt-tpg0p.14).
//
//	gt-installer [--sha REF] [--source manual|rebuild-gt|post-land]
//	             [--slot-role ROLE] [--slot-timeout SECONDS] [--dry-run]
//	             [--no-image-first]
//
// The install sections land one bead at a time: the OpenShell gateway step
// (gt-tpg0p.9), target resolution, the no-op path and the refusals
// (gt-tpg0p.10), and the build, install, signing, smoke and syncs
// (gt-tpg0p.11). The sandbox image step (gt-tpg0p.12) and the Makefile switch
// (gt-tpg0p.14) are still to land.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// directory, its home and its own executable. A test supplies its own, so it
// never rewrites the test process's environment.
type environment struct {
	getenv func(string) string
	getwd  func() (string, error)
	home   func() (string, error)
	exe    func() (string, error)
	// newRun builds the run itself, so a test hands the whole flow commands
	// that are recorded rather than started and a filesystem under a temporary
	// directory. Without it any test that reached the build, install, signing or
	// smoke step would run make, gt and codesign for real (gt-tpg0p.11).
	newRun func(cfg installer.Config, c *installer.Contract, paths installer.Paths) *installer.Run
}

// run is runWith against the process's own environment.
func run(args []string, stdout, stderr io.Writer) int {
	return runWith(args, stdout, stderr, environment{
		getenv: os.Getenv,
		getwd:  os.Getwd,
		home:   os.UserHomeDir,
		exe:    os.Executable,
	})
}

// runWith is run against env, so a test can point every directory at a
// temporary tree. The deferred OnExit is the script's EXIT trap: whatever
// happens, the last line states a result and the status is 0-3.
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
	// The gateway template falls back to the directory beside this program (the
	// script's $SCRIPT_DIR). A program that cannot name its own leaves the
	// fallback unset rather than guessing: the step then has only the checkout's
	// template and warns when that is missing too (gt-tpg0p.9).
	if exe, err := env.exe(); err == nil {
		paths.SelfDir = filepath.Dir(exe)
	}
	c.Receipts = paths.Receipts()
	c.GT = paths.GT()

	newRun := env.newRun
	if newRun == nil {
		newRun = installer.NewRun
	}
	r := newRun(cfg, c, paths)
	r.Signing = installer.ResolveSigning(env.getenv, paths.TownRoot)
	// --dry-run prints the gateway plan and changes nothing, before the checkout
	// is read.
	if cfg.DryRun {
		r.GatewayStep(installer.GatewayDryRun)
		return int(c.Finish(installer.Result{Event: "dry-run", Commit: cfg.SHA}, installer.ExitOK))
	}
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
	if code, ended := r.Build(); ended {
		return int(code)
	}
	// The sandbox image is built and published here, before the swap; that step
	// is gt-tpg0p.12.
	if code, ended := r.Install(); ended {
		return int(code)
	}
	// Signing runs on what install-local just put in place, before the smoke
	// check runs it, and never changes the outcome (gt-426fo.1).
	r.Sign()
	if code, ended := r.Smoke(); ended {
		return int(code)
	}
	r.Syncs()
	r.GatewayStep(installer.GatewayInstall)
	return int(r.Finalize())
}
