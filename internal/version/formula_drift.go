package version

import (
	"fmt"
	"strings"
)

// formulaSourceDir is where the formulas embedded into the binary live in the
// gt source tree, relative to the repository root.
const formulaSourceDir = "internal/formula/formulas"

// FormulaDrift reports whether formula fixes merged to a build branch are
// missing from the copies embedded in this binary.
//
// A sync copies the formulas embedded in the running binary (internal/formula/
// embed.go), not the checkout, so a fix merged after the last build is
// undeliverable until a rebuild — and sync alone cannot tell. This is that
// second stage's check: it names the formula files the build is missing.
type FormulaDrift struct {
	CompareRef    string   // build-branch ref the binary was compared against
	BinaryCommit  string   // commit the binary was built from
	RepoCommit    string   // commit of CompareRef
	CommitsBehind int      // commits the binary is behind CompareRef
	Files         []string // formula files changed on CompareRef since the build
	Checked       bool     // false when drift could not be determined
	Reason        string   // why the check did not run, when !Checked
}

// CheckEmbeddedFormulaDrift compares this binary's build commit against the
// build-branch ref resolved from repoDir and lists the formula source files
// that changed in between. Callers should treat !Checked as unknown, not as
// "no drift": the check needs both a build commit and a source checkout.
//
// It reads the remote-tracking ref through CheckStaleBinaryFresh, whose bounded
// fetch is the point here: a cached origin/main that happens to match the
// binary yields a confident "nothing pending" while the real branch has moved
// on, which is the exact false reassurance gt-dt7r was filed about. Reporting
// unknown costs a few seconds; reporting fresh costs weeks.
func CheckEmbeddedFormulaDrift(repoDir string) FormulaDrift {
	return newChecker().checkFormulaDrift(repoDir)
}

func (c checker) checkFormulaDrift(repoDir string) FormulaDrift {
	drift := FormulaDrift{}
	info := c.checkStaleFresh(repoDir)
	drift.CompareRef = info.CompareRef
	drift.BinaryCommit = info.BinaryCommit
	drift.RepoCommit = info.RepoCommit
	drift.CommitsBehind = info.CommitsBehind

	switch {
	case info.Skipped:
		drift.Reason = info.SkipReason
		return drift
	case info.Error != nil:
		drift.Reason = info.Error.Error()
		return drift
	case !info.IsStale:
		// Binary matches the build ref, so nothing under the formula source
		// dir can be missing from it.
		drift.Checked = true
		return drift
	}

	// Only a binary behind the build ref is missing formulas. IsForward cannot
	// be the gate: it reads a failed ancestry probe as "not an ancestor", so a
	// broken git would pass for "binary ahead or diverged" and report no drift.
	behind, err := c.isBehind(repoDir, info.BinaryCommit, info.RepoCommit)
	if err != nil {
		drift.Reason = err.Error()
		return drift
	}
	if !behind {
		// Binary is ahead of or diverged from the build ref, so it is not
		// behind it. Diffing would list the binary's own formula changes as
		// missing and tell the operator to rebuild for nothing.
		drift.Checked = true
		return drift
	}

	files, err := c.formulaSourceDiff(repoDir, info.BinaryCommit, info.RepoCommit)
	if err != nil {
		drift.Reason = err.Error()
		return drift
	}
	drift.Checked = true
	drift.Files = files
	return drift
}

// isBehind reports whether binary is an ancestor of ref, the only state in which
// the binary can be missing anything ref holds. It asks merge-base for the best
// common ancestor and checks that it is binary: when binary is an ancestor of
// ref, that is binary itself. A git failure is returned as an error, so the
// caller can fail closed. `merge-base --is-ancestor` cannot be used, because
// its "not an ancestor" exit and its failure exit both arrive as errors.
func (c checker) isBehind(repoDir, binary, ref string) (bool, error) {
	out, err := c.output(repoDir, "merge-base", binary, ref)
	if err != nil {
		return false, fmt.Errorf("cannot establish whether binary commit %s is behind %s: %w", binary, ref, err)
	}
	return commitsMatch(strings.TrimSpace(string(out)), binary), nil
}

// formulaSourceDiff lists the files under the formula source dir that differ
// between two commits. Names come back repository-root-relative.
func (c checker) formulaSourceDiff(repoDir, from, to string) ([]string, error) {
	out, err := c.output(repoDir, "diff", "--name-only", from+".."+to, "--", formulaSourceDir)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
