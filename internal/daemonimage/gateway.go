package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// BDGWPackage is the package the bd gateway image's entrypoint is built from,
// inside the gt checkout. The image runs the real bd as bd, so bdgw is the only
// binary BuildGateway builds (docs/design/bd-container.md, Architecture).
const BDGWPackage = "./cmd/bdgw"

// gwName is the name bdgw takes in b.Out: the one deploy/bd/Dockerfile.bd COPYs.
const gwName = "bdgw"

// BuildGateway writes bdgw to b.Out and returns its path. It is built for
// linux/arm64 in a clean worktree of b.GTSrc at rev, so the gateway runs the
// code the rev names. Staging follows BuildBinaries: every refusal comes before
// the build runs, and a failed call leaves nothing in b.Out.
func (b Builder) BuildGateway(ctx context.Context, rev string) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build the gateway with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the commits with")
	}
	rev, err := b.checkOutAndRev(rev, "the gateway binary")
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
		return nil, fmt.Errorf("daemonimage: cannot stage the gateway binary in %s: %w", b.Out, err)
	}
	defer os.RemoveAll(stage)
	build := binary{name: gwName, src: b.GTSrc, commit: gtCommit, pkg: BDGWPackage}
	if err := b.build(ctx, build, stage, bdbuild.Env(b.Environ)); err != nil {
		return nil, err
	}
	path := filepath.Join(b.Out, build.name)
	if err := os.Rename(filepath.Join(stage, build.name), path); err != nil {
		return nil, fmt.Errorf("daemonimage: cannot move %s into %s: %w", build.name, b.Out, err)
	}
	return []string{path}, nil
}
