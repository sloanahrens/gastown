package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// configReadMarker is the text git writes when it cannot read one of its
// configuration files. A call that failed with it is retried once.
const configReadMarker = "reading the configuration files"

// configReadRetryWait is the pause before the one retry. Each landing burst
// that prompted this (gt-n3slrv) lasted about a second.
const configReadRetryWait = time.Second

// WithConfigReadRetry returns a copy of g whose calls, when git fails with its
// configuration-files error, append a report of the files git reads to the
// stderr the call returns, wait configReadRetryWait, and run once more. The
// report is in stderr rather than the error because GitError renders stderr
// alone. The receiver is left as it was.
//
// Only the landing worker's handles opt in; every other Git keeps today's
// one-attempt behavior.
func (g *Git) WithConfigReadRetry() *Git {
	c := *g
	c.configRetry = true
	return &c
}

// withConfigReadRetry wraps base with the retry WithConfigReadRetry describes.
// A call whose error is not the configuration-files error is returned as base
// returned it. A retry that succeeds returns success with no report. A retry
// that fails returns its error, with the stderr of the last attempt plus a
// report for each of the two attempts.
func (g *Git) withConfigReadRetry(base runFunc) runFunc {
	return func(c gitCall) (string, string, error) {
		stdout, stderr, err := base(c)
		if err == nil || !strings.Contains(stderr, configReadMarker) {
			return stdout, stderr, err
		}
		first := g.configReadReport()

		wait := g.retryWait
		if wait == nil {
			wait = time.Sleep
		}
		wait(configReadRetryWait)

		stdout, stderr, err = base(c)
		if err == nil {
			return stdout, stderr, nil
		}
		return stdout, stderr + "\n" + first + "\n" + g.configReadReport(), err
	}
}

// configReadReport names the environment and files git reads for this handle,
// with what each file is and whether it opens at this instant. It never reads
// a file's contents.
func (g *Git) configReadReport() string {
	var b strings.Builder
	b.WriteString("git configuration read failed; state of the files git reads (no contents shown):")
	for _, key := range []string{"HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "XDG_CONFIG_HOME"} {
		value, ok := g.effectiveEnv(key)
		if !ok {
			value = "(unset)"
		}
		fmt.Fprintf(&b, "\n  %s=%s", key, value)
	}

	repo := g.gitDir
	if repo == "" {
		repo = filepath.Join(g.workDir, ".git")
	}
	fmt.Fprintf(&b, "\n  repository=%s", repo)

	home, _ := g.effectiveEnv("HOME")
	xdg, _ := g.effectiveEnv("XDG_CONFIG_HOME")
	// git reads $XDG_CONFIG_HOME/git/config, and $HOME/.config when that is unset.
	xdgDir := xdg
	if xdgDir == "" && home != "" {
		xdgDir = filepath.Join(home, ".config")
	}

	files := []struct{ path, unchecked string }{
		{homeGitconfig(home), "$HOME/.gitconfig"},
		{xdgGitConfig(xdgDir), "$XDG_CONFIG_HOME/git/config"},
		{filepath.Join(repo, "config"), ""},
		{filepath.Join(repo, "config.worktree"), ""},
	}
	for _, f := range files {
		if f.path == "" {
			fmt.Fprintf(&b, "\n  %s: not checked, its base directory is unset", f.unchecked)
			continue
		}
		fmt.Fprintf(&b, "\n  %s: %s", f.path, describeConfigFile(f.path))
	}
	return b.String()
}

// effectiveEnv is the value of key in the environment a call from g runs with:
// the last WithEnv entry that sets it, else unset when WithoutEnv removes it,
// else the process's own (see callEnv).
func (g *Git) effectiveEnv(key string) (string, bool) {
	for i := len(g.env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(g.env[i], "=")
		if ok && k == key {
			return v, true
		}
	}
	if containsKey(g.unset, key) {
		return "", false
	}
	return os.LookupEnv(key)
}

// homeGitconfig is $HOME/.gitconfig, or "" when HOME is unset.
func homeGitconfig(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gitconfig")
}

// xdgGitConfig is $XDG_CONFIG_HOME/git/config, or "" when that directory is
// unknown.
func xdgGitConfig(xdgDir string) string {
	if xdgDir == "" {
		return ""
	}
	return filepath.Join(xdgDir, "git", "config")
}

// describeConfigFile reports whether path exists, its size, and whether it
// opens for read now. An error is reported by its text, which names the path
// and the reason, never the file's contents.
func describeConfigFile(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "absent"
		}
		return fmt.Sprintf("stat error: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("exists, size %d, open error: %v", info.Size(), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Sprintf("exists, size %d, open ok, close error: %v", info.Size(), err)
	}
	return fmt.Sprintf("exists, size %d, open ok", info.Size())
}
