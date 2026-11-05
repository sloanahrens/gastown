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
// repo's cmd/hm (the repo root has no Go files); the two reporters are the
// devops repo's cmd/reporter and cmd/release-reporter.
const (
	HMPackage              = "./cmd/hm"
	DevopsReporterPackage  = "./cmd/reporter"
	ReleaseReporterPackage = "./cmd/release-reporter"
)

// releaseReporterVersionShape is the form of version run.sh accepts from
// release-reporter: vMAJOR.MINOR.PATCH with an optional pre-release. run.sh is not
// in this repo; BuildTools reads the binary's version against this pattern before
// the binary is placed, so a version run.sh would refuse fails the build instead.
var releaseReporterVersionShape = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?$`)

// toolNames are the tools BuildTools builds, in the order it writes them. Each is
// a binary in the build context root and a COPY into /usr/local/bin.
var toolNames = []string{"hm", "devops-reporter", "release-reporter"}

// BuildTools writes hm, devops-reporter and release-reporter to b.Out and returns
// their paths in that order. Each is built for linux/arm64 in a clean worktree of
// its pinned commit, as BuildBinaries builds gt, bd and om: hm from HMSrc at
// HMCommit, and both reporters from DevopsSrc at DevopsCommit. A checkout that is
// not at its commit, or has uncommitted changes, is refused before any build runs.
// release-reporter's version is the one the go command stamps from the checkout;
// it is read back from the built binary and refused unless it has the form run.sh
// accepts. Staging and the move into b.Out follow BuildBinaries.
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
	hmCommit, err := b.pinnedCommit(pin{what: "hm", src: b.HMSrc, commit: b.HMCommit, srcFlag: "--hm-src"})
	if err != nil {
		return nil, err
	}
	devopsCommit, err := b.pinnedCommit(pin{what: "devops", src: b.DevopsSrc, commit: b.DevopsCommit, srcFlag: "--devops-src"})
	if err != nil {
		return nil, err
	}
	builds := []binary{
		{name: "hm", src: b.HMSrc, commit: hmCommit, pkg: HMPackage},
		{name: "devops-reporter", src: b.DevopsSrc, commit: devopsCommit, pkg: DevopsReporterPackage},
		{name: "release-reporter", src: b.DevopsSrc, commit: devopsCommit, pkg: ReleaseReporterPackage},
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
		flag, dir, name, commit string
	}{
		{"--hm-src", b.HMSrc, "hm", b.HMCommit},
		{"--devops-src", b.DevopsSrc, "devops", b.DevopsCommit},
	} {
		if strings.TrimSpace(p.dir) == "" {
			return fmt.Errorf("daemonimage: %s is required: the tool is built from that checkout", p.flag)
		}
		if strings.TrimSpace(p.commit) == "" {
			return fmt.Errorf("daemonimage: no %s pin in %s: the tool is built at that commit and nothing else", p.name, b.pinsFile())
		}
		if !isCommit(strings.TrimSpace(p.commit)) {
			return fmt.Errorf("daemonimage: %s pin %q is not a 7+ character hex commit", p.name, p.commit)
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
// check that reads release-reporter's version back from its binary.
func (b Builder) toolPlanLines() []string {
	hm := strings.Join(goBuildArgv(filepath.Join(b.Out, "hm"), HMPackage, ""), " ")
	devops := strings.Join(goBuildArgv(filepath.Join(b.Out, "devops-reporter"), DevopsReporterPackage, ""), " ")
	release := strings.Join(goBuildArgv(filepath.Join(b.Out, "release-reporter"), ReleaseReporterPackage, ""), " ")
	return []string{
		fmt.Sprintf("build hm: %s   (clean worktree of %s at %s)", hm, b.HMSrc, strings.TrimSpace(b.HMCommit)),
		fmt.Sprintf("build devops-reporter: %s   (clean worktree of %s at %s)", devops, b.DevopsSrc, strings.TrimSpace(b.DevopsCommit)),
		fmt.Sprintf("build release-reporter: %s   (clean worktree of %s at %s)", release, b.DevopsSrc, strings.TrimSpace(b.DevopsCommit)),
		fmt.Sprintf("check release-reporter: go version -m %s   (refused unless vMAJOR.MINOR.PATCH[-pre])", filepath.Join(b.Out, "release-reporter")),
	}
}
