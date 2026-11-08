// Package callercwd is the one reader of the working directory gt acts for.
//
// gt normally runs where the caller stands, so os.Getwd answers. When it runs
// as a target somewhere else (the Mac wrapper execs into the daemon container,
// gt-yjzepk.10), the caller's Mac path can be absent from that process; the
// wrapper names it in GT_CALLER_CWD.
//
// The variable's only job is identity and display: who am I, which seat's
// context to print, which worktree a git command should act in. It is a
// statement by the caller, so an agent can set it itself; no guard, hook check
// or enforcement code may read it, and those resolve the real process
// directory instead. TestNoDirectGetwd pins the boundary: a non-test file
// outside this package that calls os.Getwd must be an allowlisted real site
// (guard, hook check, permission or trust decision, refusal, throttle or
// cooldown), and every other call goes through Getwd or Require.
//
// Resolution uses Getwd, which trusts the variable without checking the
// directory exists; commands that run git or write there use Require, which
// refuses when the directory is not present in this process.
package callercwd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// EnvCallerCwd names the caller's working directory when gt runs elsewhere.
const EnvCallerCwd = "GT_CALLER_CWD"

// Getwd returns the caller's working directory: GT_CALLER_CWD when it is set
// to an absolute, clean path, else the process working directory.
//
// A set but unusable value is refused instead of falling back to the process
// directory: the caller is somewhere else, and silently acting on this
// process's directory is the failure this package exists to stop.
func Getwd() (string, error) {
	value, set := os.LookupEnv(EnvCallerCwd)
	if !set {
		return os.Getwd()
	}
	if err := validate(value); err != nil {
		return "", err
	}
	return value, nil
}

// Exists reports whether the caller's directory is present in this process.
// A directory that is not there is (false, nil); an unusable variable or a
// stat failure other than absence is an error, which Exists never reads as
// absent.
func Exists() (bool, error) {
	path, err := Getwd()
	if err != nil {
		return false, err
	}
	info, statErr := os.Stat(path)
	if statErr == nil {
		return info.IsDir(), nil
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("checking the caller's directory %s: %w", path, statErr)
}

// Require returns the caller's directory, refusing when it is not a directory
// present in this process. A refusal never falls back to the process directory.
func Require() (string, error) {
	path, err := Getwd()
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", notPresent(path)
		}
		return "", fmt.Errorf("checking the caller's directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", notPresent(path)
	}
	return path, nil
}

func notPresent(path string) error {
	return fmt.Errorf("the caller's directory %s is not present here", path)
}

// validate refuses a GT_CALLER_CWD value that cannot name one directory.
func validate(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is set but empty; unset it to use the process directory", EnvCallerCwd)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not start or end with whitespace: %q", EnvCallerCwd, value)
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path: %q", EnvCallerCwd, value)
	}
	if filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean path (no \".\" or \"..\" segments and no trailing separator): %q", EnvCallerCwd, value)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character: %q", EnvCallerCwd, value)
		}
	}
	return nil
}
