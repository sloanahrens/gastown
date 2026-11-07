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

	commonDir, gitDir, unresolved := g.repositoryConfigDirs()
	if unresolved != "" {
		fmt.Fprintf(&b, "\n  repository config files: not checked, %s", unresolved)
	} else {
		fmt.Fprintf(&b, "\n  repository=%s", commonDir)
	}

	home, _ := g.effectiveEnv("HOME")
	xdg, _ := g.effectiveEnv("XDG_CONFIG_HOME")
	// git reads $XDG_CONFIG_HOME/git/config, and $HOME/.config when that is unset.
	xdgDir := xdg
	if xdgDir == "" && home != "" {
		xdgDir = filepath.Join(home, ".config")
	}

	type configFile struct{ path, unchecked string }
	files := []configFile{
		{homeGitconfig(home), "$HOME/.gitconfig"},
		{xdgGitConfig(xdgDir), "$XDG_CONFIG_HOME/git/config"},
	}
	if unresolved == "" {
		files = append(files,
			configFile{path: filepath.Join(commonDir, "config")},
			configFile{path: filepath.Join(gitDir, "config.worktree")})
	}
	for _, f := range files {
		if f.path == "" {
			fmt.Fprintf(&b, "\n  %s: not checked, its base directory is unset", f.unchecked)
			continue
		}
		fmt.Fprintf(&b, "\n  %s: %s", f.path, g.describeConfigFile(f.path))
	}
	return b.String()
}

// repositoryConfigDirs names the directories git reads the repository
// configuration from: the shared directory holding "config", and the
// per-worktree directory holding "config.worktree". An explicit git directory
// is used as git uses it. Otherwise both are resolved from workDir's .git,
// which is a directory in an ordinary clone and a "gitdir: <path>" file in a
// linked worktree. When that entry does not resolve to a git directory, the
// returned reason says so and the report checks neither file, rather than
// naming a path git never reads.
func (g *Git) repositoryConfigDirs() (commonDir, gitDir, unresolved string) {
	if g.gitDir != "" {
		return g.gitDir, g.gitDir, ""
	}
	if g.workDir == "" {
		return "", "", "neither a git directory nor a working directory is set"
	}
	if dir, common, ok := worktreeGitDirs(g.workDir); ok {
		return common, dir, ""
	}
	dotGit := filepath.Join(g.workDir, ".git")
	if _, err := os.Lstat(dotGit); err != nil {
		return "", "", fmt.Sprintf("the git directory could not be resolved: %v", err)
	}
	return "", "", fmt.Sprintf("%s does not resolve to a git directory", dotGit)
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
// and the reason, never the file's contents. The open goes through
// g.openConfig so a test can drive the open-error report on a host whose user
// can open every file.
func (g *Git) describeConfigFile(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "absent"
		}
		return fmt.Sprintf("stat error: %v", err)
	}
	open := g.openConfig
	if open == nil {
		open = os.Open
	}
	f, err := open(path)
	if err != nil {
		return fmt.Sprintf("exists, size %d, open error: %v", info.Size(), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Sprintf("exists, size %d, open ok, close error: %v", info.Size(), err)
	}
	return fmt.Sprintf("exists, size %d, open ok", info.Size())
}
