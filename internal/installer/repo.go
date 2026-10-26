package installer

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Repo is the checkout one run reads and fast-forwards: the git reads target
// resolution and the refusals make. Every field is a seam; GitRepo fills them
// for a real checkout.
type Repo struct {
	// Fetch updates the remote refs; a failure is a warning, not an end.
	Fetch func() error
	// Resolve is REF as a full commit hash, or "" when the checkout has no
	// such commit.
	Resolve func(ref string) string
	// CommitTime is when ref merged.
	CommitTime func(ref string) (time.Time, error)
	// IsAncestor reports whether ancestor is in descendant's history; a git
	// that cannot answer counts as no.
	IsAncestor func(ancestor, descendant string) bool
	// Dirty reports the tracked edits outside .beads/, which are the only ones
	// that change what a build produces (gt-50k). A status git cannot give is an
	// error: an undescribable checkout must not pass the refusal gate as clean.
	Dirty func() (bool, error)
	// Branch is the checked-out branch, "" when detached.
	Branch func() string
	// FastForward moves the checkout to ref without a reset; a checkout that
	// cannot is diverged, and a reset is a human's call (gt-4g1m).
	FastForward func(ref string) error
	// Head is the commit the checkout is on.
	Head func() (string, error)
	// AheadOf is how many commits HEAD has that ref lacks. A count git cannot
	// give is an error, not zero: a divergence read that failed must not pass
	// the refusal gate as up to date.
	AheadOf func(ref string) (int, error)
}

// GitRepo is the real Repo for the checkout at dir, running git through run.
func GitRepo(dir string, run Exec) Repo {
	if run == nil {
		run = combinedOutput
	}
	git := func(args ...string) (string, error) {
		out, err := run("git", append([]string{"-C", dir}, args...)...)
		return string(out), err
	}
	// git fetch and merge can fork a detached auto-maintenance process that
	// inherits the install lock and would hold it past the run; the calls that
	// can trigger one disable it (gt-1jgci).
	gitQuietMaintenance := func(args ...string) (string, error) {
		full := append([]string{"-C", dir, "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)
		out, err := run("git", full...)
		return string(out), err
	}
	return Repo{
		Fetch: func() error {
			_, err := gitQuietMaintenance("fetch", "origin", "--quiet")
			return err
		},
		Resolve: func(ref string) string {
			if ref == "" {
				return ""
			}
			// --end-of-options keeps a caller-supplied ref (--sha, the daemon's
			// state.json) from being read as a git flag.
			out, err := git("rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
			if err != nil {
				return ""
			}
			return strings.TrimSpace(out)
		},
		CommitTime: func(ref string) (time.Time, error) {
			out, err := git("log", "-1", "--format=%ct", "--end-of-options", ref)
			if err != nil {
				return time.Time{}, err
			}
			seconds, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(seconds, 0).UTC(), nil
		},
		IsAncestor: func(ancestor, descendant string) bool {
			_, err := git("merge-base", "--is-ancestor", ancestor, descendant)
			return err == nil
		},
		Dirty: func() (bool, error) {
			out, err := git("status", "--porcelain", "--untracked-files=no", "--", ".", ":(exclude).beads")
			if err != nil {
				return false, err
			}
			return strings.TrimSpace(out) != "", nil
		},
		Branch: func() string {
			out, _ := git("branch", "--show-current")
			return strings.TrimSpace(out)
		},
		FastForward: func(ref string) error {
			_, err := gitQuietMaintenance("merge", "--ff-only", ref, "--quiet")
			return err
		},
		Head: func() (string, error) {
			out, err := git("rev-parse", "HEAD")
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(out), nil
		},
		AheadOf: func(ref string) (int, error) {
			out, err := git("rev-list", "--count", "--end-of-options", ref+"..HEAD")
			if err != nil {
				return 0, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(out))
			if err != nil {
				return 0, err
			}
			return n, nil
		},
	}
}

// combinedOutput is the Exec a seam that was given none runs.
func combinedOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
