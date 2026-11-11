package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// The packages the tools are built from, inside their checkouts. hm is the hm
// repo's cmd/hm (the repo root has no Go files); the reporters and the cloud
// check are the devops repo's cmd/reporter, cmd/release-reporter and
// cmd/cloudcheck.
const (
	HMPackage               = "./cmd/hm"
	DevopsReporterPackage   = "./cmd/reporter"
	ReleaseReporterPackage  = "./cmd/release-reporter"
	DevopsCloudCheckPackage = "./cmd/cloudcheck"
)

// CloudCheckAppsName is the app list the cloud check reads from beside its
// binary (cmd/cloudcheck resolvePaths), staged from the devops checkout at the
// pinned commit. CloudCheckAppsPath is where that checkout holds it.
const (
	CloudCheckAppsName = "cloud-check.apps"
	CloudCheckAppsPath = "ops/cloud-check.apps"
)

// releaseReporterVersionShape is the form of version run.sh accepts from
// release-reporter: vMAJOR.MINOR.PATCH with an optional pre-release. run.sh is not
// in this repo; BuildTools reads the binary's version against this pattern before
// the binary is placed, so a version run.sh would refuse fails the build instead.
var releaseReporterVersionShape = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?$`)

// toolNames are the tools BuildTools builds, in the order it writes them. Each is
// a binary in the build context root and a COPY into /usr/local/bin.
var toolNames = []string{"hm", "devops-reporter", "release-reporter", "cloudcheck"}

// BuildTools writes hm, devops-reporter, release-reporter and cloudcheck to
// b.Out and returns their paths in that order, and stages the cloud check's app
// list beside its binary. Each is built for linux/arm64 in a clean worktree of
// its pinned commit, as BuildBinaries builds gt, bd and om: hm from HMSrc at
// HMCommit, and the devops binaries from DevopsSrc at DevopsCommit. A source
// with no checkout named is fetched from its pin into b.Out first, and the
// fetched tree is removed when the call returns. A checkout that is not at its
// commit, or has uncommitted changes, is refused before any build runs.
// release-reporter's version is the one the go command stamps from the
// checkout; it is read back from the built binary and refused unless it has the
// form run.sh accepts. Staging and the move into b.Out follow BuildBinaries.
func (b Builder) BuildTools(ctx context.Context) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build the tools with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the commits with")
	}
	if b.Go == nil {
		return nil, errors.New("daemonimage: no go to read the release-reporter version with")
	}
	if err := b.checkToolInputs(); err != nil {
		return nil, err
	}
	hmSrc, hmCleanup, err := b.source(ctx, "hm", b.HMSrc)
	if err != nil {
		return nil, err
	}
	defer hmCleanup()
	devopsSrc, devopsCleanup, err := b.source(ctx, "devops", b.DevopsSrc)
	if err != nil {
		return nil, err
	}
	defer devopsCleanup()
	hmCommit, err := b.pinnedCommit(pin{what: "hm", src: hmSrc, commit: b.HMCommit, srcFlag: "--hm-src"})
	if err != nil {
		return nil, err
	}
	devopsCommit, err := b.pinnedCommit(pin{what: "devops", src: devopsSrc, commit: b.DevopsCommit, srcFlag: "--devops-src"})
	if err != nil {
		return nil, err
	}
	builds := []binary{
		{name: "hm", src: hmSrc, commit: hmCommit, pkg: HMPackage},
		{name: "devops-reporter", src: devopsSrc, commit: devopsCommit, pkg: DevopsReporterPackage},
		{name: "release-reporter", src: devopsSrc, commit: devopsCommit, pkg: ReleaseReporterPackage},
		{name: "cloudcheck", src: devopsSrc, commit: devopsCommit, pkg: DevopsCloudCheckPackage},
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
	if err := b.checkReleaseReporterVersion(stage, env); err != nil {
		return nil, err
	}
	// The app list is data, not a build: it is read from the pinned commit so
	// it moves with the binary, and it lands beside the binary because that is
	// where cmd/cloudcheck looks for it.
	apps, err := b.Git(devopsSrc, "show", devopsCommit+":"+CloudCheckAppsPath)
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot read %s at %s: %w", CloudCheckAppsPath, devopsCommit, err)
	}
	if err := os.WriteFile(filepath.Join(stage, CloudCheckAppsName), apps, 0o644); err != nil { //nolint:gosec // the app list is public configuration, not a credential
		return nil, fmt.Errorf("daemonimage: cannot stage %s: %w", CloudCheckAppsName, err)
	}
	paths := make([]string, 0, len(builds))
	for _, build := range builds {
		path := filepath.Join(b.Out, build.name)
		if err := os.Rename(filepath.Join(stage, build.name), path); err != nil {
			return nil, fmt.Errorf("daemonimage: cannot move %s into %s: %w", build.name, b.Out, err)
		}
		paths = append(paths, path)
	}
	if err := os.Rename(filepath.Join(stage, CloudCheckAppsName), filepath.Join(b.Out, CloudCheckAppsName)); err != nil {
		return nil, fmt.Errorf("daemonimage: cannot move %s into %s: %w", CloudCheckAppsName, b.Out, err)
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
		flag, dir, name, commit string
	}{
		{"--hm-src", b.HMSrc, "hm", b.HMCommit},
		{"--devops-src", b.DevopsSrc, "devops", b.DevopsCommit},
	} {
		if strings.TrimSpace(p.commit) == "" {
			return fmt.Errorf("daemonimage: no %s pin in %s: the tool is built at that commit and nothing else", p.name, b.pinsFile())
		}
		if !isCommit(strings.TrimSpace(p.commit)) {
			return fmt.Errorf("daemonimage: %s pin %q is not a 7+ character hex commit", p.name, p.commit)
		}
		if strings.TrimSpace(p.dir) == "" {
			continue // fetched from its pin; the fetch refuses a pin that cannot be used
		}
		if err := refuseCanonicalClone(p.flag, p.dir); err != nil {
			return err
		}
	}
	return refuseCanonicalClone("output directory", b.Out)
}

// pinsFile names the file the commits come from in a refusal: the caller's pins
// file, or the repository's own when a Builder is used without one.
func (b Builder) pinsFile() string {
	if b.PinsPath != "" {
		return b.PinsPath
	}
	return PinsPath
}

// pin is one checkout a tool is built from: the directory, the commit it is
// pinned to, and the flag that names the directory in a refusal.
type pin struct {
	what, src, commit, srcFlag string
}

// pinnedCommit returns the full sha of p's commit after checking that the
// checkout's HEAD is that commit and its worktree has no changes. The build reads
// the commit in a clean worktree either way; a checkout that has drifted from its
// pin is refused because it is not the code the pin names.
func (b Builder) pinnedCommit(p pin) (string, error) {
	commit := strings.TrimSpace(p.commit)
	out, err := b.Git(p.src, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("daemonimage: %s pin %s is not a commit of %s: %w", p.what, commit, p.src, err)
	}
	full := strings.TrimSpace(string(out))
	head, err := b.Git(p.src, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot read HEAD of the %s checkout %s: %w", p.what, p.src, err)
	}
	if got := strings.TrimSpace(string(head)); got != full {
		return "", fmt.Errorf("daemonimage: the %s checkout %s is at %s, not its pin %s; check that commit out there", p.what, p.src, got, commit)
	}
	status, err := b.Git(p.src, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot read the status of the %s checkout %s: %w", p.what, p.src, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return "", fmt.Errorf("daemonimage: the %s checkout %s has uncommitted changes; %s is built from a clean checkout at its pin %s", p.what, p.src, p.what, commit)
	}
	return full, nil
}

// checkReleaseReporterVersion reads the version the go command stamped into the
// release-reporter binary built in stage, and refuses it unless it has the form
// releaseReporterVersionShape accepts. The stamp is the main module's version,
// which go version -m prints on its mod line; no stamp is needed on the command
// line, and a build with VCS stamping off reads (devel).
func (b Builder) checkReleaseReporterVersion(stage string, env []string) error {
	path := filepath.Join(stage, "release-reporter")
	out, err := b.Go(stage, env, "version", "-m", path)
	if err != nil {
		return fmt.Errorf("daemonimage: cannot read the version of release-reporter at %s: %w", path, err)
	}
	version := mainModuleVersion(string(out))
	switch {
	case version == "":
		return fmt.Errorf("daemonimage: release-reporter at %s carries no main module version; go version -m names none", path)
	case version == "(devel)":
		return fmt.Errorf("daemonimage: release-reporter at %s is a (devel) build, which carries no version from its checkout", path)
	case !releaseReporterVersionShape.MatchString(version):
		return fmt.Errorf("daemonimage: release-reporter at %s is version %q, not vMAJOR.MINOR.PATCH[-pre], the form run.sh accepts", path, version)
	}
	return nil
}

// mainModuleVersion returns the version on the first mod line of go version -m
// output, which is the main module's, or "" when there is no such line or it has
// no version field.
func mainModuleVersion(info string) string {
	for _, line := range strings.Split(info, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "mod" {
			continue
		}
		if len(fields) < 3 {
			return ""
		}
		return fields[2]
	}
	return ""
}

// toolPlanLines are Plan's lines for BuildTools: the argv each build runs, and the
// check that reads release-reporter's version back from its binary. hmSrc and
// devopsSrc are the directories the tools are built from: the named checkouts, or
// the directories their pins are fetched into.
func (b Builder) toolPlanLines(hmSrc, devopsSrc string) []string {
	hm := strings.Join(goBuildArgv(filepath.Join(b.Out, "hm"), HMPackage, ""), " ")
	devops := strings.Join(goBuildArgv(filepath.Join(b.Out, "devops-reporter"), DevopsReporterPackage, ""), " ")
	release := strings.Join(goBuildArgv(filepath.Join(b.Out, "release-reporter"), ReleaseReporterPackage, ""), " ")
	cloud := strings.Join(goBuildArgv(filepath.Join(b.Out, "cloudcheck"), DevopsCloudCheckPackage, ""), " ")
	return []string{
		fmt.Sprintf("build hm: %s   (clean worktree of %s at %s)", hm, hmSrc, strings.TrimSpace(b.HMCommit)),
		fmt.Sprintf("build devops-reporter: %s   (clean worktree of %s at %s)", devops, devopsSrc, strings.TrimSpace(b.DevopsCommit)),
		fmt.Sprintf("build release-reporter: %s   (clean worktree of %s at %s)", release, devopsSrc, strings.TrimSpace(b.DevopsCommit)),
		fmt.Sprintf("build cloudcheck: %s   (clean worktree of %s at %s)", cloud, devopsSrc, strings.TrimSpace(b.DevopsCommit)),
		fmt.Sprintf("check release-reporter: go version -m %s   (refused unless vMAJOR.MINOR.PATCH[-pre])", filepath.Join(b.Out, "release-reporter")),
		fmt.Sprintf("stage %s: git show %s:%s -> %s", CloudCheckAppsName, strings.TrimSpace(b.DevopsCommit), CloudCheckAppsPath, filepath.Join(b.Out, CloudCheckAppsName)),
	}
}
