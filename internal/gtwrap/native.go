package gtwrap

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

// Native is a tool the config runs on the Mac instead of in the daemon image. The
// Mac holds the tool's own credentials and reads the caller's worktree, which the
// image does not have, so a call it covers never reaches the target.
type Native struct {
	// Tool is the tool the entry is for. Its config key is native.<Tool>.
	Tool string
	// Path is the tool's binary on the Mac. It must be absolute.
	Path string
	// Subcommands are the first arguments that select the native run. Any other
	// call of the tool reaches the target.
	Subcommands []string
}

// Covers reports whether a call of the tool with args runs natively.
func (n Native) Covers(args []string) bool {
	return len(args) > 0 && slices.Contains(n.Subcommands, args[0])
}

// Check reports why Path cannot run: it is empty, relative, unreadable, or not an
// executable file. The reason names the config key, so the operator can find it.
// A call a Check refuses never falls back to the target, since the target would
// read the wrong tree.
func (n Native) Check(stat func(string) (fs.FileInfo, error)) error {
	key := fmt.Sprintf("native.%s.path", n.Tool)
	switch {
	case n.Path == "":
		return fmt.Errorf("gtwrap: %s is empty in the target config file; set it to the absolute path of the %s binary on this Mac", key, n.Tool)
	case !filepath.IsAbs(n.Path):
		return fmt.Errorf("gtwrap: %s %q is relative; set it to an absolute path", key, n.Path)
	}
	info, err := stat(n.Path)
	if err != nil {
		return fmt.Errorf("gtwrap: %s %q cannot be read: %w", key, n.Path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("gtwrap: %s %q is not an executable file", key, n.Path)
	}
	return nil
}

// Process runs a native call on the Mac. The real one is OSProcess; a test
// records the call.
type Process interface {
	// Run runs argv with the caller's stdin, stdout and stderr attached, in dir,
	// with env as its whole environment, and returns the status it exited with.
	// An error means it did not run at all.
	Run(argv []string, env []string, dir string) (exit int, err error)
}

// OSProcess runs argv on the host. The environment is passed through whole: the
// pod filters of Plan do not apply to a native call.
type OSProcess struct{}

// Run runs argv with the wrapper's own stdin, stdout and stderr.
func (OSProcess) Run(argv []string, env []string, dir string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitStatus(cmd.Run())
}
