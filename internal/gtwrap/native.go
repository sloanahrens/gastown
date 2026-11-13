package gtwrap

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// nativeBdTool and nativeGtTool are the tools whose subcommands are narrowed: bd
// to hooks run, and gt to the host command set below.
const (
	nativeBdTool = "bd"
	nativeGtTool = "gt"
)

// gtHostCommands are the gt command paths that run on the Mac because each reads
// or prints something only the Mac has, so the daemon pod would answer with its
// own paths or refuse. town forward plist prints the launchd job whose log paths
// are under the Mac's ~/gt/daemon; maint plan and maint apply read the Mac's
// Docker contexts, worktrees, backups and disk (gt-aea89k).
var gtHostCommands = []string{"town forward plist", "maint plan", "maint apply"}

// Native is a tool the config runs on the Mac instead of in the daemon image. The
// Mac has what the image does not: the tool's own credentials, the caller's
// worktree, and the host's Docker contexts and file paths. A call it covers never
// reaches the target.
type Native struct {
	// Tool is the tool the entry is for. Its config key is native.<Tool>.
	Tool string
	// Path is the tool's binary on the Mac. It must be absolute.
	Path string
	// Subcommands are command paths: one or more words the call's arguments must
	// open with. Any other call of the tool reaches the target.
	Subcommands []string
}

// Covers reports whether a call of the tool with args runs natively. For bd only
// hooks run is native, since the beads git hooks read the Mac's files. For gt only
// a path in the host command set runs natively, so a gt entry cannot make any
// other call native; every other gt call reaches the target.
func (n Native) Covers(args []string) bool {
	switch n.Tool {
	case nativeBdTool:
		return len(args) > 1 && args[0] == "hooks" && args[1] == "run" && slices.Contains(n.Subcommands, "hooks")
	case nativeGtTool:
		return coversAny(args, n.Subcommands, GTHostCommand)
	}
	return coversAny(args, n.Subcommands, nil)
}

// coversAny reports whether args opens with one of paths, where path passes
// allowed when it is set.
func coversAny(args, paths []string, allowed func(string) bool) bool {
	for _, path := range paths {
		if allowed != nil && !allowed(path) {
			continue
		}
		if opensWith(args, path) {
			return true
		}
	}
	return false
}

// opensWith reports whether args opens with the words of path. A path of no words
// covers nothing, so an empty entry runs no call.
func opensWith(args []string, path string) bool {
	words := strings.Fields(path)
	if len(words) == 0 || len(args) < len(words) {
		return false
	}
	return slices.Equal(args[:len(words)], words)
}

// GTHostCommand reports whether path is a gt command path that runs on the Mac.
func GTHostCommand(path string) bool {
	return slices.Contains(gtHostCommands, path)
}

// GTHostCommandList renders the gt host command set for a refusal that names it.
func GTHostCommandList() string {
	return strings.Join(gtHostCommands, ", ")
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

// CheckNotSelf refuses a Path that is the wrapper running now, which would run
// itself and never stop, or a wrapper it cannot find or read to compare
// (gt-qq5kv4 cutover 7); Check has already passed, so Path is an absolute,
// readable file.
func (n Native) CheckNotSelf(stat func(string) (fs.FileInfo, error), executable func() (string, error)) error {
	key := fmt.Sprintf("native.%s.path", n.Tool)
	if executable == nil {
		return fmt.Errorf("gtwrap: %s %q cannot be compared with this wrapper: no way to find it", key, n.Path)
	}
	self, err := executable()
	if err != nil {
		return fmt.Errorf("gtwrap: %s %q cannot be compared with this wrapper: find it: %w", key, n.Path, err)
	}
	selfInfo, err := stat(self)
	if err != nil {
		return fmt.Errorf("gtwrap: %s %q cannot be compared with this wrapper: read %s: %w", key, n.Path, self, err)
	}
	pathInfo, err := stat(n.Path)
	if err != nil {
		return fmt.Errorf("gtwrap: %s %q cannot be read: %w", key, n.Path, err)
	}
	if os.SameFile(selfInfo, pathInfo) {
		return fmt.Errorf("gtwrap: %s %q is this wrapper itself and would run it again; point it at the real %s", key, n.Path, n.Tool)
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
