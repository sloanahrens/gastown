package version

import (
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/util"
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
	drift := FormulaDrift{}
	info := CheckStaleBinaryFresh(repoDir)
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

	files, err := formulaSourceDiff(repoDir, info.BinaryCommit, info.RepoCommit)
	if err != nil {
		drift.Reason = err.Error()
		return drift
	}
	drift.Checked = true
	drift.Files = files
	return drift
}

// formulaSourceDiff lists the files under the formula source dir that differ
// between two commits. Names come back repository-root-relative.
func formulaSourceDiff(repoDir, from, to string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", from+".."+to, "--", formulaSourceDir)
	cmd.Dir = repoDir
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
