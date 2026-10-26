package cmd

import (
	"os"
	"os/exec"
	"strings"
)

// guardProcess is the process state the tap guards read: the environment,
// the working directory, the home and temp directories, the host's
// well-known temp dirs, the origin remote and the host's CPU idle share. The
// cobra RunE functions pass realGuardProcess(); tests pass their own, so they
// need no t.Setenv, t.Chdir or package-variable swaps and can run in parallel.
type guardProcess struct {
	getenv    func(string) string
	lookupEnv func(string) (string, bool)
	getwd     func() (string, error)
	homeDir   func() (string, error)
	tempDir   func() string
	// hostTemp are the well-known host temp dirs scratchRoots adds beside
	// $TMPDIR (hostTempScratchDirs in production).
	hostTemp []string
	// checkoutBound is the directory the checkout walk stops at, inclusive; ""
	// walks to the filesystem root (production). A test sets its fake root, so
	// the walk never reads the real filesystem above it (gt-b43ra).
	checkoutBound string
	// originURL is `git remote get-url origin` for the process's cwd.
	originURL func() (string, error)
	// cpuIdle samples the percent of the host's CPU that is idle.
	cpuIdle func() (float64, bool)
}

// realGuardProcess is the running process's state.
func realGuardProcess() guardProcess {
	return guardProcess{
		getenv:    os.Getenv,
		lookupEnv: os.LookupEnv,
		getwd:     os.Getwd,
		homeDir:   os.UserHomeDir,
		tempDir:   os.TempDir,
		hostTemp:  hostTempScratchDirs,
		originURL: gitOriginURL,
		cpuIdle:   actualHostCPUIdle,
	}
}

// gitOriginURL is the origin remote's URL for the process's cwd.
func gitOriginURL() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// guardSession is what one dangerous-command guard run judges a command
// against: the process it reads, and the town root resolved once from it
// ("" when not inside a town — see currentTownRoot).
type guardSession struct {
	proc     guardProcess
	townRoot string
}
