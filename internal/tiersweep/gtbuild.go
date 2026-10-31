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

// gtBinName is the file name the sweep's gt binary is built under, in the log
// directory.
const gtBinName = "gt"

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

// buildGT builds the gt binary once for the sweep, into the log directory, and
// returns its path. A failed build returns "": the sweep carries on with
// TIER_SWEEP_GT_BIN unset, so each package builds its own copy the way it did
// before this binary existed.
//
// The flags are the integration helper's: buildflags.LDFlags, and the icu4c
// cgo entries buildflags.ICUCgoEnv adds for whatever the sweep's own
// environment does not already set. A binary built with any other flags would
// not be the one internal/cmd's buildGTBinary makes, and the tests that use it
// would be exercising something else.
//
// The build's output goes to buildLogName in the log directory, the same place
// each package's test output goes. A failed build prints one stderr line that
// names that file rather than quoting it: the output is long, and a diagnostic
// that quotes another program's output can carry a secret into a log. A build
// whose log cannot be created does not run, and the sweep carries on as it does
// after a failed build.
func (s *Sweep) buildGT() string {
	bin := filepath.Join(s.Opts.LogDir, gtBinName)
	logPath := filepath.Join(s.Opts.LogDir, buildLogName)
	outFile, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(s.Stderr, "%s: build gt: %v; %s is unset for the tests\n", Name, err, EnvGTBin)
		return ""
	}
	defer outFile.Close()
	argv := []string{"go", "build", "-ldflags", buildflags.LDFlags, "-o", bin, "./cmd/gt"}
	env := append(append([]string(nil), s.Environ...), s.cgoEnv()(s.Environ)...)
	if code := s.buildExec()(".", env, argv, outFile, buildTimeout); code != 0 {
		reason := fmt.Sprintf("exited %d", code)
		if code == buildKilledCode {
			reason = "did not finish in " + buildTimeout.String()
		}
		fmt.Fprintf(s.Stderr, "%s: build gt: go build ./cmd/gt %s; its output is in %s; %s is unset for the tests\n",
			Name, reason, logPath, EnvGTBin)
		return ""
	}
	return bin
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
