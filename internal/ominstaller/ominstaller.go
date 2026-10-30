// Package ominstaller builds om from the job-owned clone and puts it in force.
// Run is the entry point; the rebuild_om daemon job calls it in process. Run
// refuses an empty daemon directory, then takes the install lock and, under it,
// calls install, which does these steps in order:
//
//  1. refuses when there is no clone, the clone is dirty or its HEAD is
//     unreadable (`gitRepo`, `gitDirty`, `gitHead`);
//  2. does nothing when the om at OMPath already reports the built commit
//     (`probeVersion`);
//  3. builds the clone into a temp file beside om (`install`);
//  4. signs the build (`sign`);
//  5. keeps the om at OMPath as om.prev beside it (`keepPrevious`);
//  6. swaps the build over om (`swap`);
//  7. smoke-checks that om reports the built commit, and rolls back if it does
//     not (`probeVersion`, `smokeFailed`).
//
// Run appends one receipt to the daemon directory for each outcome it reaches
// after the start time is read. Two outcomes write none, because there is no
// receipts file to write to or no start time to record: an empty daemon
// directory (refused, no-daemon-dir) and a clock that panics twice (failed,
// clock).
package ominstaller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/installlock"
)

// Exit is the status Run returns. The daemon's rebuild_om job maps it, as it
// maps the gt installer's codes.
type Exit int

const (
	// ExitInstalled means om was installed, or the installed om already is the built sha.
	ExitInstalled Exit = 0
	// ExitFailed means the build, the swap or the smoke check failed. A failed smoke check is rolled back.
	ExitFailed Exit = 1
	// ExitRefused means there is no clone, the clone is dirty, or its HEAD cannot be read. om is untouched.
	ExitRefused Exit = 2
	// ExitBusy means another install held the lock for the whole wait.
	ExitBusy Exit = 3
)

const (
	eventFailed  = "failed"
	eventRefused = "refused"

	reasonUnexpected  = "unexpected"
	reasonLockBusy    = "lock-busy"
	reasonNoDaemonDir = "no-daemon-dir"
	reasonClock       = "clock"

	lockName = "install-om.lock"
)

// Result is the outcome Run reports on its last stdout line. An empty field
// prints as "-".
type Result struct {
	Event  string
	Commit string
	Prev   string
	Reason string
}

// Line is the machine-readable last line of the install-om command.
func (r Result) Line() string {
	return fmt.Sprintf("install-om: RESULT %s %s %s %s",
		dashIfEmpty(r.Event), dashIfEmpty(r.Commit), dashIfEmpty(r.Prev), dashIfEmpty(r.Reason))
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Command is one external command the installer runs through Config.Exec.
type Command struct {
	Name string
	Args []string
	Dir  string
	// Stdin is written to the command's standard input. Empty leaves it at
	// /dev/null. The keychain unlock sends the password here, never in Args.
	Stdin string
}

// ExecFunc runs a Command and returns what it wrote to stdout and stderr.
type ExecFunc func(ctx context.Context, c Command) (stdout, stderr string, err error)

// AcquireFunc takes the exclusive install lock on path, waiting up to wait, and
// returns the release. It returns installlock.ErrBusy when the wait ran out.
type AcquireFunc func(path string, wait time.Duration) (release func(), err error)

// Config is what an install run needs from its caller. Exec and Acquire are the
// seams: a test replaces them, and the production wiring sets them to the real
// command runner and installlock.Acquire.
type Config struct {
	// OMPath is the om binary the install replaces, the --om-path equivalent.
	// Empty takes INSTALL_OM_PATH, else ~/go/bin/om.
	OMPath string
	// TownRoot is the town the clone and signing directory sit under. Empty takes
	// GT_TOWN_ROOT, else the outermost town above the working directory.
	TownRoot string
	// SrcDir is the om clone the build reads. Empty takes INSTALL_OM_SRC_DIR, else
	// <town>/.runtime/om-src.
	SrcDir string
	// SignDir holds the signing identity. Empty takes INSTALL_OM_SIGN_DIR, else
	// <town>/.runtime/signing.
	SignDir string
	// SignIdentity is the keychain identity om is signed with. Empty takes
	// INSTALL_OM_SIGN_IDENTITY, else gastown-dev-unattended.
	SignIdentity string

	// DaemonDir holds the install lock and install-om-receipts.jsonl.
	DaemonDir string
	// Source is the receipt's source: manual or rebuild-om.
	Source string
	// LockWait is how long a run waits for a peer holding the lock. Zero takes
	// INSTALL_OM_LOCK_WAIT, else 300 s.
	LockWait time.Duration
	// SignWait bounds the codesign step. Zero takes INSTALL_OM_SIGN_TIMEOUT, else 30 s.
	SignWait time.Duration
	// SmokeWait bounds the om version smoke check. Zero takes INSTALL_OM_SMOKE_TIMEOUT,
	// else 30 s.
	SmokeWait time.Duration

	// Exec runs every external command the install makes.
	Exec ExecFunc
	// Acquire takes the install lock: the real one is installlock.Acquire.
	Acquire AcquireFunc
	// Files is the filesystem the sign and keep steps use. Nil is the real one.
	Files Files
	// Swap puts the build at src in place of dst, atomically. Nil is binaryinstall's
	// Replace, over Exec and Acquire; a test replaces it, as the swap is tested in
	// binaryinstall.
	Swap func(src, dst string) error
	// Now is the clock for the receipt timestamp and duration. Nil uses time.Now.
	Now func() time.Time
	// Logf receives the notices a run does not fail for. Nil discards them.
	Logf func(format string, args ...any)
	// Stderr receives a receipt the run could not write when Logf is nil. Nil is os.Stderr.
	Stderr io.Writer
}

// Run takes the install lock and runs the install, and returns the result and
// its exit status. It writes one receipt for the outcome, except the two the
// package doc names. It never panics: a panic in a seam, or any path not
// anticipated here, is a failed run with the reason "unexpected". A clock that panics twice fails the run with the reason
// "clock" and writes no receipt, since there is no start time to record.
func Run(ctx context.Context, cfg Config) (res Result, exit Exit) {
	start, ok := cfg.startTime()
	if !ok {
		cfg.logf("[install-om] FAILED: the clock panicked twice; no receipt written")
		return Result{Event: eventFailed, Reason: reasonClock}, ExitFailed
	}
	defer func() {
		if p := recover(); p != nil {
			res, exit = Result{Event: eventFailed, Reason: reasonUnexpected}, ExitFailed
		}
		cfg.writeReceipt(res, start)
	}()

	if cfg.DaemonDir == "" {
		return cfg.refuse(reasonNoDaemonDir, "no daemon directory for the install lock and receipts")
	}
	release, err := cfg.Acquire(lockPath(cfg.DaemonDir), cfg.LockWait)
	if errors.Is(err, installlock.ErrBusy) {
		cfg.logf("[install-om] Another install held the lock for %ds; not installing.", int64(cfg.LockWait/time.Second))
		return Result{Event: eventRefused, Reason: reasonLockBusy}, ExitBusy
	}
	if err != nil {
		cfg.logf("[install-om] could not take the install lock: %v", err)
		return Result{Event: eventFailed, Reason: reasonUnexpected}, ExitFailed
	}
	defer release()

	return install(ctx, cfg)
}

func lockPath(daemonDir string) string {
	return filepath.Join(daemonDir, lockName)
}

func (c Config) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// startTime reads the clock for the run's start. A clock that panics is read
// once more; if that panics too, ok is false and the run writes no receipt.
func (c Config) startTime() (start time.Time, ok bool) {
	if start, ok = c.readClock(); ok {
		return start, true
	}
	return c.readClock()
}

// readClock is now, with a panic reported as ok false.
func (c Config) readClock() (t time.Time, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return c.now(), true
}

func (c Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}
