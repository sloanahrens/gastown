// Package daemonimage builds the gastown daemon's container artifacts on the
// host: the linux/arm64 gt, bd and om binaries the image runs (gt-qpeei.9), and
// the image built from them (gt-qpeei.16). Every command it runs is one injected
// call, so the gate proves what a build runs without Docker, a running daemon or
// a cluster.
package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// The package each binary is built from, inside its source tree. gt and bd are
// pinned to the polecat image flow's own sources: a test keeps GTPackage equal
// to the package polecat-image builds, and BDPackage to bdbuild.Package.
const (
	GTPackage = "./cmd/gt"
	BDPackage = "./cmd/bd"
	OMPackage = "./cmd/om"
)

// The -X stamps the gt and bd binaries are built with. gt reports rev, the commit
// its image tag names: it falls back to the VCS stamp of the tree it was built
// in (internal/cmd.resolveCommitHash), which in the image build is the caller's
// checkout and not that commit. bd reports its commit the way bdbuild stamps it.
const (
	gtCommitFlag = "-X github.com/steveyegge/gastown/internal/cmd.Commit="
	bdCommitFlag = "-X main.Commit="
)

// Runner runs one argv in dir with env as the whole environment it runs with; it
// is the package's only way to run anything.
type Runner func(ctx context.Context, dir string, env, argv []string) error

// Builder builds the daemon image's binaries.
type Builder struct {
	// Run runs every build.
	Run Runner
	// Git resolves the commits the gt and bd stamps name, in their checkouts.
	Git bdbuild.Git
	// Environ is the host environment the builds start from; nil reads the
	// host's. bdbuild.Env keeps the variables that locate the toolchain and drops
	// the ones that would change what is compiled.
	Environ []string
	// Out is the absolute directory the three binaries are written to. A build
	// runs in its source checkout, so a relative Out would be read against that
	// checkout, not the caller's directory: BuildBinaries refuses one.
	Out string
	// GTSrc is the gt checkout, read as it stands: the caller owes one at rev
	// with no changes, the rule `make polecat-image` refuses a dirty tree for
	// (gt-9fz0k).
	GTSrc string
	// BDSrc is the beads checkout the image's bd is built from: the BD_SRC the
	// polecat image takes.
	BDSrc string
	// BDCommit is the beads commit the image's bd is built from and stamped with:
	// the commit bdbuild.Commit picks for the polecat image.
	BDCommit string
	// OMSrc is the om checkout the image's om is built from: the clone the
	// rebuild_om job keeps at <town>/.runtime/om-src.
	OMSrc string
}

// BuildBinaries writes gt, bd and om to b.Out and returns their paths in that
// order. Every refusal comes before the first build runs, so a refused call
// writes nothing; a build that fails returns its error and no paths.
func (b Builder) BuildBinaries(ctx context.Context, rev string) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the commits with")
	}
	if b.Out == "" {
		return nil, errors.New("daemonimage: no output directory for the binaries")
	}
	if !filepath.IsAbs(b.Out) {
		return nil, fmt.Errorf("daemonimage: output directory %q is not absolute; a build runs in its source checkout", b.Out)
	}
	if rev = strings.TrimSpace(rev); rev == "" {
		return nil, errors.New("daemonimage: no rev to stamp gt with")
	}
	if !isCommit(rev) {
		return nil, fmt.Errorf("daemonimage: rev %q is not a 7+ character hex commit", rev)
	}
	for _, src := range []struct{ name, dir string }{{"gt", b.GTSrc}, {"bd", b.BDSrc}, {"om", b.OMSrc}} {
		if src.dir == "" {
			return nil, fmt.Errorf("daemonimage: no %s source checkout named", src.name)
		}
	}
	bdCommit := strings.TrimSpace(b.BDCommit)
	if bdCommit == "" {
		return nil, errors.New("daemonimage: no bd commit to stamp bd with")
	}
	if !isCommit(bdCommit) {
		return nil, fmt.Errorf("daemonimage: bd commit %q is not a 7+ character hex commit", bdCommit)
	}
	gtCommit, err := b.resolve(b.GTSrc, rev)
	if err != nil {
		return nil, err
	}
	bdCommit, err = b.resolve(b.BDSrc, bdCommit)
	if err != nil {
		return nil, err
	}
	env := bdbuild.Env(b.Environ)
	builds := []struct {
		name, src, pkg, ldflags string
	}{
		{name: "gt", src: b.GTSrc, pkg: GTPackage, ldflags: gtCommitFlag + gtCommit},
		{name: "bd", src: b.BDSrc, pkg: BDPackage, ldflags: bdCommitFlag + bdCommit},
		{name: "om", src: b.OMSrc, pkg: OMPackage},
	}
	paths := make([]string, 0, len(builds))
	for _, build := range builds {
		path := filepath.Join(b.Out, build.name)
		argv := []string{"go", "build", "-trimpath"}
		if build.ldflags != "" {
			argv = append(argv, "-ldflags", build.ldflags)
		}
		argv = append(argv, "-o", path, build.pkg)
		if err := b.Run(ctx, build.src, env, argv); err != nil {
			return nil, fmt.Errorf("daemonimage: building %s from %s failed: %w", build.name, build.pkg, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// resolve returns the full sha commit names in src, and refuses one src does
// not hold, so a stamp never names a commit the build did not read.
func (b Builder) resolve(src, commit string) (string, error) {
	out, err := b.Git(src, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("daemonimage: %s does not hold commit %s: %w", src, commit, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isCommit reports whether s has the shape of an abbreviated or full commit, so
// git never reads it as an option or a revision expression.
func isCommit(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
