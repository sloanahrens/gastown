package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// The packages the tools are built from, inside their checkouts. hm is the hm
// repo's main package; the two reporters are the devops repo's cmd/reporter and
// cmd/release-reporter.
const (
	HMPackage              = "."
	DevopsReporterPackage  = "./cmd/reporter"
	ReleaseReporterPackage = "./cmd/release-reporter"
)

// releaseReporterMinVersion is the MIN_VERSION the release-reporter plugin
// compares its binary's --version against.
const releaseReporterMinVersion = "v0.1.3"

// releaseReporterBase is the core version the release-reporter stamp is a
// pseudo-version of: the patch after releaseReporterMinVersion. A pre-release of
// the core it names sorts above MIN_VERSION whether a comparison reads the
// pre-release or only the core, which a stamp of MIN_VERSION itself would not.
const releaseReporterBase = "v0.1.4"

// releaseReporterVersionFlag stamps release-reporter's version. The symbol is
// the conventional main.version; the devops source is not in this checkout, so
// the in-container --version check is what confirms it names that variable.
const releaseReporterVersionFlag = "-X main.version="

// planVersion stands in for the release-reporter stamp in Plan, as planBuildID
// stands in for bd's build id: the stamp needs the commit time, which Plan does
// not read. It is one shell-safe token.
const planVersion = "pseudo_version_read_in_checkout"

// toolNames are the tools BuildTools builds, in the order it writes them. Each is
// a binary in the build context root and a COPY into /usr/local/bin.
var toolNames = []string{"hm", "devops-reporter", "release-reporter"}

// BuildTools writes hm, devops-reporter and release-reporter to b.Out and returns
// their paths in that order. Each is built for linux/arm64 in a clean worktree of
// its pinned commit, as BuildBinaries builds gt, bd and om: hm from HMSrc at
// HMCommit, and both reporters from DevopsSrc at DevopsCommit. A checkout that is
// not at its commit, or has uncommitted changes, is refused before any build runs.
// Staging and the move into b.Out follow BuildBinaries.
func (b Builder) BuildTools(ctx context.Context) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build the tools with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the commits with")
	}
	if err := b.checkToolInputs(); err != nil {
		return nil, err
	}
	hmCommit, err := b.pinnedCommit(pin{what: "hm", src: b.HMSrc, commit: b.HMCommit, srcFlag: "--hm-src", commitFlag: "--hm-commit"})
	if err != nil {
		return nil, err
	}
	devopsCommit, err := b.pinnedCommit(pin{what: "devops", src: b.DevopsSrc, commit: b.DevopsCommit, srcFlag: "--devops-src", commitFlag: "--devops-commit"})
	if err != nil {
		return nil, err
	}
	if len(devopsCommit) < 12 {
		return nil, fmt.Errorf("daemonimage: git printed %q for --devops-commit %s, too short to stamp release-reporter with", devopsCommit, b.DevopsCommit)
	}
	when, err := b.commitTime(b.DevopsSrc, devopsCommit)
	if err != nil {
		return nil, err
	}
	builds := []binary{
		{name: "hm", src: b.HMSrc, commit: hmCommit, pkg: HMPackage},
		{name: "devops-reporter", src: b.DevopsSrc, commit: devopsCommit, pkg: DevopsReporterPackage},
		{name: "release-reporter", src: b.DevopsSrc, commit: devopsCommit, pkg: ReleaseReporterPackage, ldflags: releaseReporterVersionFlag + releaseReporterVersion(when, devopsCommit)},
	}
	stage, err := os.MkdirTemp(b.Out, ".daemonimage-tools-")
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot stage the tools in %s: %w", b.Out, err)
	}
	defer os.RemoveAll(stage)
	env := bdbuild.Env(b.Environ)
	for _, build := range builds {
		if err := b.build(ctx, build, stage, env); err != nil {
			return nil, err
		}
	}
	paths := make([]string, 0, len(builds))
	for _, build := range builds {
		path := filepath.Join(b.Out, build.name)
		if err := os.Rename(filepath.Join(stage, build.name), path); err != nil {
			return nil, fmt.Errorf("daemonimage: cannot move %s into %s: %w", build.name, b.Out, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// checkToolInputs refuses the inputs BuildTools cannot build from, before anything
// runs. It reads nothing from the host, so Plan refuses exactly what a build would.
func (b Builder) checkToolInputs() error {
	if b.Out == "" {
		return errors.New("daemonimage: no output directory for the tools")
	}
	if !filepath.IsAbs(b.Out) {
		return fmt.Errorf("daemonimage: output directory %q is not absolute; a build runs in a worktree, not in the caller's directory", b.Out)
	}
	for _, p := range []struct {
		flag, dir, commitFlag, commit string
	}{
		{"--hm-src", b.HMSrc, "--hm-commit", b.HMCommit},
		{"--devops-src", b.DevopsSrc, "--devops-commit", b.DevopsCommit},
	} {
		if strings.TrimSpace(p.dir) == "" {
			return fmt.Errorf("daemonimage: %s is required: the tool is built from that checkout", p.flag)
		}
		if strings.TrimSpace(p.commit) == "" {
			return fmt.Errorf("daemonimage: %s is required: the tool is built at that commit and nothing else", p.commitFlag)
		}
		if !isCommit(strings.TrimSpace(p.commit)) {
			return fmt.Errorf("daemonimage: %s %q is not a 7+ character hex commit", p.commitFlag, p.commit)
		}
		if err := refuseCanonicalClone(p.flag, p.dir); err != nil {
			return err
		}
	}
	return refuseCanonicalClone("output directory", b.Out)
}

// pin is one checkout a tool is built from: the directory, the commit it is
// pinned to, and the two flags that name them in a refusal.
type pin struct {
	what, src, commit, srcFlag, commitFlag string
}

// pinnedCommit returns the full sha of p's commit after checking that the
// checkout's HEAD is that commit and its worktree has no changes. The build reads
// the commit in a clean worktree either way; a checkout that has drifted from its
// pin is refused because it is not the code the flags name.
func (b Builder) pinnedCommit(p pin) (string, error) {
	commit := strings.TrimSpace(p.commit)
	out, err := b.Git(p.src, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("daemonimage: %s %s is not a commit of %s: %w", p.commitFlag, commit, p.src, err)
	}
	full := strings.TrimSpace(string(out))
	head, err := b.Git(p.src, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot read HEAD of the %s checkout %s: %w", p.what, p.src, err)
	}
	if got := strings.TrimSpace(string(head)); got != full {
		return "", fmt.Errorf("daemonimage: the %s checkout %s is at %s, not %s %s; check that commit out there", p.what, p.src, got, p.commitFlag, commit)
	}
	status, err := b.Git(p.src, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot read the status of the %s checkout %s: %w", p.what, p.src, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return "", fmt.Errorf("daemonimage: the %s checkout %s has uncommitted changes; %s is built from a clean checkout at %s", p.what, p.src, p.srcFlag, commit)
	}
	return full, nil
}

// commitTime returns the committer time git reports for commit in src.
func (b Builder) commitTime(src, commit string) (time.Time, error) {
	out, err := b.Git(src, "show", "-s", "--format=%ct", commit)
	if err != nil {
		return time.Time{}, fmt.Errorf("daemonimage: cannot read the commit time of %s in %s: %w", commit, src, err)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("daemonimage: git in %s printed %q as the commit time of %s, not a unix time", src, strings.TrimSpace(string(out)), commit)
	}
	return time.Unix(secs, 0), nil
}

// releaseReporterVersion is the version release-reporter is stamped with: a Go
// pseudo-version of the devops commit, releaseReporterBase with the commit's UTC
// time and its first twelve characters. It has the vMAJOR.MINOR.PATCH[-pre] form
// the bead names as the one run.sh accepts, and its core is above
// releaseReporterMinVersion.
func releaseReporterVersion(committed time.Time, commit string) string {
	return fmt.Sprintf("%s-0.%s-%s", releaseReporterBase, committed.UTC().Format("20060102150405"), commit[:12])
}

// toolPlanLines are Plan's lines for BuildTools: the argv each build runs, with the
// release-reporter stamp left as planVersion, since it needs the commit time.
func (b Builder) toolPlanLines() []string {
	hm := strings.Join(goBuildArgv(filepath.Join(b.Out, "hm"), HMPackage, ""), " ")
	devops := strings.Join(goBuildArgv(filepath.Join(b.Out, "devops-reporter"), DevopsReporterPackage, ""), " ")
	release := strings.Join(goBuildArgv(filepath.Join(b.Out, "release-reporter"), ReleaseReporterPackage, releaseReporterVersionFlag+planVersion), " ")
	return []string{
		fmt.Sprintf("build hm: %s   (clean worktree of %s at %s)", hm, b.HMSrc, strings.TrimSpace(b.HMCommit)),
		fmt.Sprintf("build devops-reporter: %s   (clean worktree of %s at %s)", devops, b.DevopsSrc, strings.TrimSpace(b.DevopsCommit)),
		fmt.Sprintf("build release-reporter: %s   (clean worktree of %s at %s)", release, b.DevopsSrc, strings.TrimSpace(b.DevopsCommit)),
	}
}
