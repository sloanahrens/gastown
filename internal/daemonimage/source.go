package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// FetchSource checks out one pinned source into dir, an empty directory the
// caller made: git init, a depth-1 fetch of pin.Commit from pin.URL, and a
// detached checkout of FETCH_HEAD, each through run, then it reads HEAD through
// git and refuses a tree that is not the pinned commit. A fetch that fails leaves
// whatever git wrote in dir, and removing it is the caller's: the caller made it.
//
// run is given env as its whole environment, so authentication is the caller's
// (a git credential helper or an askpass the runner provides). No credential is
// an argument here: checkPinURL refuses a URL that carries one before any
// command runs, and no refusal echoes the URL.
func FetchSource(ctx context.Context, run Runner, git bdbuild.Git, dir string, env []string, pin Pin) error {
	if run == nil {
		return errors.New("daemonimage: no runner to fetch the source with")
	}
	if git == nil {
		return errors.New("daemonimage: no git to verify the fetched source with")
	}
	if err := checkFetchable(pin.Name, pin); err != nil {
		return err
	}
	if err := checkEmptyDir(dir); err != nil {
		return err
	}
	if env == nil {
		env = os.Environ()
	}
	for _, argv := range fetchArgv(pin) {
		if err := run(ctx, dir, env, argv); err != nil {
			return fmt.Errorf("daemonimage: fetching %s from its pin: %s failed: %w", pin.Name, strings.Join(argv, " "), err)
		}
	}
	out, err := git(dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("daemonimage: cannot read the commit fetched into %s: %w", dir, err)
	}
	head := strings.TrimSpace(string(out))
	if head != pin.Commit {
		return fmt.Errorf("daemonimage: the %s tree fetched from its pin is at %s, not the pinned %s", pin.Name, head, pin.Commit)
	}
	return nil
}

// fetchArgv is the three commands a fetch runs, in order. It is the one place
// they are written, so Plan prints exactly the argv FetchSource runs.
func fetchArgv(pin Pin) [][]string {
	return [][]string{
		{"git", "init"},
		{"git", "fetch", "--depth", "1", pin.URL, pin.Commit},
		{"git", "checkout", "--detach", "FETCH_HEAD"},
	}
}

// checkEmptyDir refuses a fetch directory that is missing or holds anything: a
// fetch reads and writes the tree git makes there, and what is already in the
// directory is not the fetch's to overwrite or remove.
func checkEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("daemonimage: cannot read the fetch directory %s: %w", dir, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("daemonimage: the fetch directory %s holds %d entries; a fetch needs an empty directory", dir, len(entries))
	}
	return nil
}

// checkFetchable refuses a pin a fetch cannot use: a URL that is not a
// credential-free http(s) repository URL, and a commit that is not the full sha
// the fetch asks for. No refusal echoes the URL, which may hold the credential it
// refuses.
func checkFetchable(name string, pin Pin) error {
	if err := checkPinURL(pin.URL); err != nil {
		return fmt.Errorf("daemonimage: %s pin URL: %w", name, err)
	}
	if !fullCommit.MatchString(pin.Commit) {
		return fmt.Errorf("daemonimage: %s pin commit %q is not a full 40-character hex sha", name, pin.Commit)
	}
	return nil
}
