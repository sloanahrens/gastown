package ominstaller

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/binaryinstall"
)

// newBinName is the temp file the build is written to, beside om. The install
// lock runs one install at a time, so one name serves, as keepTempName does.
const newBinName = ".om.new"

// The reasons install reports in its Result. The names are fixed: the receipts
// file and the rebuild_om alerts report them.
const (
	eventInstalled  = "installed"
	eventNoop       = "noop"
	eventRolledBack = "rolled_back"

	reasonNoSource         = "no-source"
	reasonDirty            = "dirty"
	reasonAlreadyInstalled = "already-installed"
	reasonBuildFailed      = "build-failed"
	reasonSign             = "sign"
	reasonKeepPrevious     = "keep-previous"
	reasonSwapFailed       = "swap-failed"
	reasonSmokeFailed      = "smoke-failed"
	reasonRollbackFailed   = "rollback-failed"
)

// install runs the install under the lock Run holds. It refuses before anything
// is written, does nothing when the installed om already is the built commit, and
// otherwise builds, signs, keeps the previous om, swaps the build over om and
// smoke-checks the result. Every failure before the swap leaves om as it was and
// removes the build; a failed smoke check rolls the previous om back.
func install(ctx context.Context, cfg Config) (Result, Exit) {
	src := cfg.SrcDir
	switch r := cfg.gitRepo(ctx, src); {
	case r.IsFail():
		return cfg.refuse(reasonNoSource, "no om clone at "+src)
	case r.IsUnknown():
		return cfg.refuse(reasonNoSource, "cannot tell whether "+src+" is a clone")
	}
	commit, err := cfg.gitHead(ctx, src)
	if err != nil {
		return cfg.refuse(reasonNoSource, src+" has no readable HEAD")
	}
	// Only tracked edits change what the build produces, so only they refuse.
	dirty, err := cfg.gitDirty(ctx, src)
	if err != nil {
		return cfg.refuse(reasonDirty, "cannot read the work tree status of "+src)
	}
	if dirty {
		return cfg.refuse(reasonDirty, src+" has uncommitted changes")
	}

	prev := cfg.probeVersion(ctx, cfg.OMPath)
	if prev != "" && strings.HasPrefix(commit, prev) {
		cfg.logf("[install-om] Installed om is already %s; nothing to do.", commit)
		return Result{Event: eventNoop, Commit: commit, Prev: prev, Reason: reasonAlreadyInstalled}, ExitInstalled
	}

	files := cfg.files()
	tmp := filepath.Join(filepath.Dir(cfg.OMPath), newBinName)
	if strings.HasPrefix(tmp, "-") {
		return cfg.fail("", reasonBuildFailed, "refusing to build to "+tmp+": a leading dash", commit, prev)
	}

	cfg.logf("[install-om] Building %s in %s", commit, src)
	if _, _, err := cfg.Exec(ctx, Command{
		Name: "go",
		Args: []string{"build", "-ldflags=-X main.version=" + commit, "-o", tmp, "--", "./cmd/om"},
		Dir:  src,
	}); err != nil {
		return cfg.fail(tmp, reasonBuildFailed, "go build failed for "+commit, commit, prev)
	}
	if err := files.Chmod(tmp, 0o755); err != nil {
		return cfg.fail(tmp, reasonBuildFailed, "cannot make "+tmp+" executable", commit, prev)
	}

	if err := sign(ctx, cfg, tmp); err != nil {
		return cfg.fail(tmp, reasonSign, err.Error(), commit, prev)
	}
	kept, err := keepPrevious(cfg, cfg.OMPath)
	if err != nil {
		return cfg.fail(tmp, reasonKeepPrevious, err.Error(), commit, prev)
	}
	if err := cfg.swap(tmp, cfg.OMPath); err != nil {
		// The swap's own error may carry a command's output, so only the step is named.
		return cfg.fail(tmp, reasonSwapFailed, "cannot rename the build over "+cfg.OMPath, commit, prev)
	}
	// Replace copied the build into place, so the temp file is spent.
	_ = files.Remove(tmp)

	if got := cfg.probeVersion(ctx, cfg.OMPath); got == "" || !strings.HasPrefix(commit, got) {
		return cfg.smokeFailed(commit, prev, kept, cfg.OMPath+" does not report the built commit "+commit)
	}

	cfg.logf("[install-om] In force: %s -> %s.", orNone(prev), commit)
	return Result{Event: eventInstalled, Commit: commit, Prev: prev}, ExitInstalled
}

// swap puts the build at src in place of dst, atomically. binaryinstall's Replace
// does it, so the immutable-flag and rename rules are the ones the gt installer
// already holds, and its commands and lock run through this run's seams. Config.Swap,
// when set, takes its place.
func (c Config) swap(src, dst string) error {
	if c.Swap != nil {
		return c.Swap(src, dst)
	}
	inst := binaryinstall.Installer{
		Files:    c.replaceFiles(),
		LockWait: c.LockWait,
		Warnf:    c.logf,
	}
	return inst.Replace(src, filepath.Dir(dst), filepath.Base(dst))
}

// replaceFiles is the seam binaryinstall's Replace runs its commands and lock
// through: the run's Exec and Acquire, so the flag commands are the ones the rest
// of the install runs.
func (c Config) replaceFiles() binaryinstall.Files {
	return binaryinstall.Files{
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, _, err := c.Exec(ctx, Command{Name: name, Args: args})
			return []byte(out), err
		},
		Lock: func(path string) (func(), error) { return c.Acquire(path, c.LockWait) },
	}
}

// probeVersion returns the commit om at omPath reports, or "" when it cannot say.
// SmokeWait bounds the run, so a hung om cannot hang the install; with no bound
// there is no probe, and the caller treats the answer as unknown.
func (c Config) probeVersion(ctx context.Context, omPath string) string {
	if c.SmokeWait <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, c.SmokeWait)
	defer cancel()
	return c.installedCommit(ctx, omPath)
}

// refuse ends the run as refused. Nothing has been written when it is called.
func (c Config) refuse(reason, msg string) (Result, Exit) {
	c.logf("[install-om] Refused: %s", msg)
	return Result{Event: eventRefused, Reason: reason}, ExitRefused
}

// fail ends the run as failed. tmp, when set, is the build to remove, so a failure
// before the swap leaves no temp behind. A leftover that cannot be removed is
// overwritten by the next build, so its error is not a failure of this run.
func (c Config) fail(tmp, reason, msg, commit, prev string) (Result, Exit) {
	c.logf("[install-om] FAILED: %s", msg)
	if tmp != "" {
		_ = c.files().Remove(tmp)
	}
	return Result{Event: eventFailed, Commit: commit, Prev: prev, Reason: reason}, ExitFailed
}

// orNone is s, or "none" when s is empty, for a log line.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
