package tiersweep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/buildflags"
	"github.com/steveyegge/gastown/internal/util"
)

// gtBinName is the file name the sweep's gt binary is built under, in the
// directory buildGT makes for it.
const gtBinName = "gt"

// gtBinDirName is the directory under a runner's work tree that holds the
// sweep's gt binary: the runner deletes the tree when the run ends, so the
// binary goes with it (gt-pnzx3m).
const gtBinDirName = "gt-bin"

// gtBinTempPattern names the temporary directory a host sweep builds its gt
// binary in. A host sweep has no work tree to put it in, so it removes the
// directory itself once the integration tier has run (gt-pnzx3m).
const gtBinTempPattern = "tier-sweep-gt."

// buildLogName is the file the build's output is kept in, in the log directory,
// so a failed build can be read after the sweep has finished.
const buildLogName = "gt-build.log"

// buildTimeout bounds the sweep's one go build. Warm it links in about 8 s,
// and the sweep has every package still ahead of it: a build that has not
// finished in buildTimeout is stuck on something the sweep will not wait out,
// and the packages behind it are what the run is for (gt-2daks).
const buildTimeout = 10 * time.Minute

// buildKilledCode is what a build buildTimeout stopped reports. It is the
// status timeout(1) gives a command it killed, so a build that ran out of time
// is not mistaken for one that failed on its own.
const buildKilledCode = 124

// BuildExec runs the sweep's one go build under a deadline: a command still
// running at timeout is killed, with the compilers it started, and reports
// buildKilledCode. ProcessBuildExec in production. A nil BuildExec runs the
// build through Exec, which takes no deadline; that is the test path, and
// cmd/tier-sweep wires ProcessBuildExec so no real sweep runs unbounded.
type BuildExec func(dir string, env []string, argv []string, out io.Writer, timeout time.Duration) int

// ProcessBuildExec is the production BuildExec.
var ProcessBuildExec BuildExec = func(dir string, env []string, argv []string, out io.Writer, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed argv, the go build this file names
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	// Its own process group, so the deadline reaches the compiler and linker
	// go started rather than only go itself (util.SetProcessGroup installs the
	// kill on the context's cancel).
	util.SetProcessGroup(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return buildKilledCode
	}
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(out, "%s: %v\n", argv[0], err)
	return 127
}

// buildGT builds the gt binary once for the sweep and returns its path with the
// cleanup that removes the directory holding it. The directory is under the
// runner's work tree when it gave one, and a temporary directory otherwise,
// because the binary is 66 MB and the daemon keeps each run's log directory:
// the binary is never one of the logs (gt-pnzx3m).
//
// A failed build returns "": the sweep carries on with TIER_SWEEP_GT_BIN unset,
// so each package builds its own copy the way it did before this binary
// existed. The cleanup is never nil, so the caller defers it on every path.
//
// The flags are the integration helper's: buildflags.LDFlags, and the icu4c
// cgo entries buildflags.ICUCgoEnv adds for whatever the sweep's own
// environment does not already set. A binary built with any other flags would
// not be the one internal/cmd's buildGTBinary makes, and the tests that use it
// would be exercising something else.
//
// The build's output goes to buildLogName in the log directory, the same place
// each package's test output goes: that one is a log, and it is kept with them.
// A failed build prints one stderr line that names that file rather than
// quoting it: the output is long, and a diagnostic that quotes another
// program's output can carry a secret into a log. A build whose log cannot be
// created does not run, and the sweep carries on as it does after a failed
// build.
func (s *Sweep) buildGT() (string, func()) {
	logPath := filepath.Join(s.Opts.LogDir, buildLogName)
	outFile, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(s.Stderr, "%s: build gt: %v; %s is unset for the tests\n", Name, err, EnvGTBin)
		return "", func() {}
	}
	defer outFile.Close()
	dir, err := s.gtBinDir()
	if err != nil {
		fmt.Fprintf(s.Stderr, "%s: build gt: %v; %s is unset for the tests\n", Name, err, EnvGTBin)
		return "", func() {}
	}
	bin := filepath.Join(dir, gtBinName)
	argv := []string{"go", "build", "-ldflags", buildflags.LDFlags, "-o", bin, "./cmd/gt"}
	env := append(append([]string(nil), s.Environ...), s.cgoEnv()(s.Environ)...)
	if code := s.buildExec()(".", env, argv, outFile, buildTimeout); code != 0 {
		reason := fmt.Sprintf("exited %d", code)
		if code == buildKilledCode {
			reason = "did not finish in " + buildTimeout.String()
		}
		fmt.Fprintf(s.Stderr, "%s: build gt: go build ./cmd/gt %s; its output is in %s; %s is unset for the tests\n",
			Name, reason, logPath, EnvGTBin)
		return "", s.gtBinCleanup(dir)
	}
	return bin, s.gtBinCleanup(dir)
}

// gtBinDir makes the directory the sweep's gt binary is built in. Under the
// runner's work tree it is a child of the tree, which the runner deletes when
// the run ends; os.Mkdir rather than MkdirAll refuses a missing work tree and an
// entry already at the path, so the sweep only ever deletes a directory it made
// in this run. A host sweep makes a temporary directory instead, and removes it
// itself.
func (s *Sweep) gtBinDir() (string, error) {
	if s.Opts.WorkDir != "" {
		dir := filepath.Join(s.Opts.WorkDir, gtBinDirName)
		if err := os.Mkdir(dir, 0o755); err != nil {
			return "", fmt.Errorf("make %s: %w", dir, err)
		}
		return dir, nil
	}
	mkdirTemp := s.MkdirTemp
	if mkdirTemp == nil {
		mkdirTemp = os.MkdirTemp
	}
	dir, err := mkdirTemp("", gtBinTempPattern)
	if err != nil {
		return "", fmt.Errorf("make a temporary directory: %w", err)
	}
	return dir, nil
}

// gtBinCleanup returns the call that removes the directory the gt binary was
// built in, once the packages that use the binary have run. A removal that
// fails is one stderr line and changes no verdict, as the log directory's own
// removal is (gt-xk9my).
func (s *Sweep) gtBinCleanup(dir string) func() {
	return func() {
		removeAll := s.RemoveAll
		if removeAll == nil {
			removeAll = os.RemoveAll
		}
		if err := removeAll(dir); err != nil {
			fmt.Fprintf(s.Stderr, "%s: remove %s: %v (%s)\n", Name, dir, err, sweepGoesOn)
		}
	}
}

// cgoEnv is the seam the build's cgo entries come through: the run's CgoEnv
// when it wired one, and buildflags.ICUCgoEnv otherwise.
func (s *Sweep) cgoEnv() func([]string) []string {
	if s.CgoEnv != nil {
		return s.CgoEnv
	}
	return buildflags.ICUCgoEnv
}

// buildExec is the seam the build runs through: the run's BuildExec when it
// wired one, and Exec otherwise.
func (s *Sweep) buildExec() BuildExec {
	if s.BuildExec != nil {
		return s.BuildExec
	}
	return func(dir string, env []string, argv []string, out io.Writer, _ time.Duration) int {
		return s.Exec(dir, env, argv, out)
	}
}
