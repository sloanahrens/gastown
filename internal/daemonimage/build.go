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
)

// The cross-compile every binary in the image is built with: the image is
// linux/arm64 whatever host builds it, and CGO off links nothing of the host's
// into it.
const (
	TargetOS   = "linux"
	TargetArch = "arm64"
	TargetCGO  = "0"
)

// The package each binary is built from, inside its source tree. gt and bd are
// pinned to the polecat image flow's own sources: a test keeps GTPackage equal
// to the package polecat-image builds, and BDPackage to bdbuild.Package.
const (
	GTPackage = "./cmd/gt"
	BDPackage = "./cmd/bd"
	OMPackage = "./cmd/om"
)

// TargetEnv is what every build is given on top of the caller's own environment.
func TargetEnv() []string {
	return []string{"CGO_ENABLED=" + TargetCGO, "GOOS=" + TargetOS, "GOARCH=" + TargetArch}
}

// Runner runs one argv in dir with env set on top of the caller's environment;
// it is the package's only way to run anything.
type Runner func(ctx context.Context, dir string, env, argv []string) error

// Builder builds the daemon image's binaries.
type Builder struct {
	// Run runs every build.
	Run Runner
	// Out is the directory the three binaries are written to.
	Out string
	// GTSrc is the gt checkout, read as it stands: the caller owes one at rev
	// with no changes, the rule `make polecat-image` refuses a dirty tree for
	// (gt-9fz0k).
	GTSrc string
	// BDSrc is the beads checkout the image's bd is built from: the BD_SRC the
	// polecat image takes.
	BDSrc string
	// OMSrc is the om checkout the image's om is built from: the clone the
	// rebuild_om job keeps at <town>/.runtime/om-src.
	OMSrc string
}

// gtCommitFlag makes the built gt report rev. gt falls back to the VCS stamp of
// the tree it was built in (internal/cmd.resolveCommitHash), which in the image
// build is the caller's checkout and not the rev the image is tagged with.
const gtCommitFlag = "-X github.com/steveyegge/gastown/internal/cmd.Commit="

// BuildBinaries writes gt, bd and om to b.Out and returns their paths in that
// order; a build that fails returns its error and no paths.
func (b Builder) BuildBinaries(ctx context.Context, rev string) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build with")
	}
	if b.Out == "" {
		return nil, errors.New("daemonimage: no output directory for the binaries")
	}
	if rev = strings.TrimSpace(rev); rev == "" {
		return nil, errors.New("daemonimage: no rev to stamp gt with")
	}
	builds := []struct {
		name, src, pkg string
		extra          []string
	}{
		{name: "gt", src: b.GTSrc, pkg: GTPackage, extra: []string{"-ldflags", gtCommitFlag + rev}},
		{name: "bd", src: b.BDSrc, pkg: BDPackage},
		{name: "om", src: b.OMSrc, pkg: OMPackage},
	}
	paths := make([]string, 0, len(builds))
	for _, build := range builds {
		if build.src == "" {
			return nil, fmt.Errorf("daemonimage: no %s source checkout named", build.name)
		}
		path := filepath.Join(b.Out, build.name)
		argv := append([]string{"go", "build", "-trimpath"}, build.extra...)
		argv = append(argv, "-o", path, build.pkg)
		if err := b.Run(ctx, build.src, TargetEnv(), argv); err != nil {
			return nil, fmt.Errorf("daemonimage: building %s from %s failed: %w", build.name, build.pkg, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}
