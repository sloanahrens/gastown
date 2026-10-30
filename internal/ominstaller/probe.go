package ominstaller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// headSHA matches the full commit name git prints for HEAD.
var headSHA = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// versionLine matches the "om version <sha>" line the smoke check reads, as the
// script does.
var versionLine = regexp.MustCompile(`^\s*om version ([0-9a-f]{7,40})(\s|$)`)

// gitRepo reports whether dir is inside a git work tree. An error means git
// could not answer, and a caller refuses on it.
func (c Config) gitRepo(ctx context.Context, dir string) (bool, error) {
	out, _, err := c.Exec(ctx, Command{Name: "git", Args: []string{"rev-parse", "--is-inside-work-tree"}, Dir: dir})
	if err != nil {
		return false, fmt.Errorf("git cannot check for a work tree: %w", err)
	}
	return strings.TrimSpace(out) == "true", nil
}

// gitHead returns the commit HEAD names in dir. A HEAD that cannot be read is an
// error, so an install never builds a commit it cannot name.
func (c Config) gitHead(ctx context.Context, dir string) (string, error) {
	out, _, err := c.Exec(ctx, Command{Name: "git", Args: []string{"rev-parse", "--verify", "--quiet", "HEAD^{commit}"}, Dir: dir})
	if err != nil {
		return "", fmt.Errorf("git cannot read HEAD: %w", err)
	}
	sha := strings.TrimSpace(out)
	if !headSHA.MatchString(sha) {
		return "", errors.New("git printed no commit for HEAD")
	}
	return sha, nil
}

// gitDirty reports whether dir has an uncommitted edit to a tracked file.
// Untracked files do not change what the build produces, so they do not count.
// A git that cannot run is an error, never a clean tree.
func (c Config) gitDirty(ctx context.Context, dir string) (bool, error) {
	out, _, err := c.Exec(ctx, Command{Name: "git", Args: []string{"status", "--porcelain", "--untracked-files=no"}, Dir: dir})
	if err != nil {
		return false, fmt.Errorf("git cannot report the work tree status: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "??") {
			return true, nil
		}
	}
	return false, nil
}

// installedCommit returns the sha om at omPath reports on its version line, or
// "" when om cannot run or prints none. The previous commit is then unknown,
// which is recorded as such and does not stop an install.
func (c Config) installedCommit(ctx context.Context, omPath string) string {
	out, _, err := c.Exec(ctx, Command{Name: omPath, Args: []string{"version"}})
	if err != nil {
		return ""
	}
	return parseVersionSHA(out)
}

// parseVersionSHA returns the sha on the first "om version <sha>" line of out, or
// "" when there is none.
func parseVersionSHA(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if m := versionLine.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}
