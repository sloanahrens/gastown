package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// The packages the bd gateway image's binaries are built from, inside the gt
// checkout. bdgw is the image's entrypoint and bdshim is the bd on its PATH
// (docs/design/bd-container.md, Architecture).
const (
	BDGWPackage   = "./cmd/bdgw"
	BDShimPackage = "./cmd/bdshim"
)

// BuildGateway writes bdgw and bdshim, the two binaries deploy/bd/Dockerfile.bd
// copies in, to b.Out and returns their paths in that order. Each is built for
// linux/arm64 in a clean worktree of b.GTSrc at rev, so the gateway runs the code
// the rev names. Staging follows BuildBinaries: every refusal comes before the
// first build runs, and a failed call leaves nothing in b.Out.
func (b Builder) BuildGateway(ctx context.Context, rev string) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build the gateway with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the commits with")
	}
	rev, err := b.checkOutAndRev(rev, "the gateway binaries")
	if err != nil {
		return nil, err
	}
	if b.GTSrc == "" {
		return nil, errors.New("daemonimage: no gt source checkout named")
	}
	for _, p := range []struct{ name, dir string }{{"output directory", b.Out}, {"gt source", b.GTSrc}} {
		if err := refuseCanonicalClone(p.name, p.dir); err != nil {
			return nil, err
		}
	}
	gtCommit, err := b.resolve(b.GTSrc, rev)
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(b.Out, ".daemonimage-gateway-")
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot stage the gateway binaries in %s: %w", b.Out, err)
	}
	defer os.RemoveAll(stage)
	env := bdbuild.Env(b.Environ)
	builds := []binary{
		{name: "bdgw", src: b.GTSrc, commit: gtCommit, pkg: BDGWPackage},
		{name: "bdshim", src: b.GTSrc, commit: gtCommit, pkg: BDShimPackage},
	}
	for _, build := range builds {
		if err := b.build(ctx, build, stage, env); err != nil {
			return nil, err
		}
	}
	return b.moveGateway(builds, stage)
}

// moveGateway moves each staged gateway binary into b.Out and returns their paths
// in builds order. A move that fails removes the binaries already moved, so a
// failed call leaves no partial pair behind; a removal that fails too is joined
// to the error, so the caller sees the partial pair it could not clear.
func (b Builder) moveGateway(builds []binary, stage string) ([]string, error) {
	ops := b.fileOps()
	var moved []string
	for _, build := range builds {
		path := filepath.Join(b.Out, build.name)
		if err := ops.rename(filepath.Join(stage, build.name), path); err != nil {
			return nil, undoGateway(ops, moved, fmt.Errorf("daemonimage: cannot move %s into %s: %w", build.name, b.Out, err))
		}
		moved = append(moved, path)
	}
	return moved, nil
}

// undoGateway removes the gateway binaries a failed move already placed and
// returns cause, with any removal error joined to it.
func undoGateway(ops fileOps, moved []string, cause error) error {
	for _, p := range moved {
		if err := ops.remove(p); err != nil {
			cause = errors.Join(cause, fmt.Errorf("daemonimage: cannot remove %s after the failed move: %w", p, err))
		}
	}
	return cause
}
