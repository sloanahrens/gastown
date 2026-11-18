package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// verbsName is the name the gateway's policy file takes in b.Out: the one
// deploy/bd/Dockerfile.bd COPYs to the path the compose unit's --verbs names.
const verbsName = "verbs.json"

// verbsRelPath is the policy file in the gt checkout, beside the Dockerfile.bd
// whose COPY reads it from the build context.
const verbsRelPath = "deploy/bd/verbs.json"

// BuildGateway writes bdgw to b.Out and returns its path. It is built for
// linux/arm64 in a clean worktree of b.GTSrc at rev, so the gateway runs the
// code the rev names. The call also stages the gateway's policy file,
// deploy/bd/verbs.json at that commit, beside bdgw, so the image's policy and
// its binary name the same revision; the image's COPY of verbs.json reads only
// the build context. Staging follows BuildBinaries:
// every refusal comes before the build runs, and a failed call takes back out
// everything it placed, reporting anything it could not.
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
	// The policy is read at the commit bdgw is built from, not from the
	// working tree, so the image's policy and its binary name one revision. The
	// read comes before anything is staged and before the build runs, so a
	// commit without one is refused with nothing written and no binary built.
	policy, err := b.Git(b.GTSrc, "show", gtCommit+":"+verbsRelPath)
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot read the gateway's policy file %s at %s; the image's COPY of verbs.json reads it from the build context: %w", verbsRelPath, gtCommit, err)
	}
	stage, err := os.MkdirTemp(b.Out, ".daemonimage-gateway-")
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot stage the gateway binary in %s: %w", b.Out, err)
	}
	defer os.RemoveAll(stage)
	ops := b.fileOps()
	if err := ops.writeFile(filepath.Join(stage, verbsName), policy, 0o444); err != nil {
		return nil, fmt.Errorf("daemonimage: cannot stage %s in %s: %w", verbsName, stage, err)
	}
	build := binary{name: gwName, src: b.GTSrc, commit: gtCommit, pkg: BDGWPackage}
	if err := b.build(ctx, build, stage, bdbuild.Env(b.Environ)); err != nil {
		return nil, err
	}
	path := filepath.Join(b.Out, build.name)
	if err := ops.rename(filepath.Join(stage, build.name), path); err != nil {
		return nil, fmt.Errorf("daemonimage: cannot move %s into %s: %w", build.name, b.Out, err)
	}
	if err := ops.rename(filepath.Join(stage, verbsName), filepath.Join(b.Out, verbsName)); err != nil {
		err = fmt.Errorf("daemonimage: cannot move %s into %s: %w", verbsName, b.Out, err)
		if undoErr := ops.remove(path); undoErr != nil && !errors.Is(undoErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w; cannot take %s back out of %s: %w", err, build.name, b.Out, undoErr)
		}
		return nil, err
	}
	return []string{path}, nil
}
