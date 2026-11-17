package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// bdRealName is the name the real bd takes in the gt-bd image's build context:
// the one deploy/bd/Dockerfile.bd copies to /usr/local/bin/bd.real.
const bdRealName = "bd.real"

// gatewayContextNames are the files StageGatewayContext writes to b.Out and
// returns, in the order deploy/bd/Dockerfile.bd copies them.
var gatewayContextNames = []string{bdRealName, gwName, verbsName}

// StageGatewayContext stages the gt-bd image's build context in b.Out: bd.real,
// bdgw and verbs.json, the three files deploy/bd/Dockerfile.bd copies, and
// returns their paths in that order and the full bd commit bd.real was stamped
// with. bd.real is built in a clean worktree of the bd source at b.BDCommit;
// bdgw is BuildGateway's build in a clean worktree of b.GTSrc at rev, which
// reads verbs.json at that same commit, so the gateway and its policy name one
// revision and no working tree reaches the context. Every refusal comes before
// the first build runs, and a failed call takes back out every file it moved.
func (b Builder) StageGatewayContext(ctx context.Context, rev string) ([]string, string, error) {
	if b.Run == nil {
		return nil, "", errors.New("daemonimage: no runner to stage the gt-bd build context with")
	}
	if b.Git == nil {
		return nil, "", errors.New("daemonimage: no git to resolve the commits with")
	}
	rev, bdPin, err := b.checkGatewayContextInputs(rev)
	if err != nil {
		return nil, "", err
	}
	stage, err := os.MkdirTemp(b.Out, ".daemonimage-gateway-context-")
	if err != nil {
		return nil, "", fmt.Errorf("daemonimage: cannot stage the gt-bd build context in %s: %w", b.Out, err)
	}
	defer os.RemoveAll(stage)
	bdSrc, bdCleanup, err := b.source(ctx, "bd", b.BDSrc)
	if err != nil {
		return nil, "", err
	}
	defer bdCleanup()
	bdCommit, err := b.resolve(bdSrc, bdPin)
	if err != nil {
		return nil, "", err
	}
	// BuildGateway stages bdgw and the policy into a staging directory of its
	// own; pointing it at this call's stage keeps every file out of b.Out until
	// all three exist.
	gateway := b
	gateway.Out = stage
	if _, err := gateway.BuildGateway(ctx, rev); err != nil {
		return nil, "", err
	}
	if err := b.buildBDOnly(ctx, bdSrc, bdCommit, stage, bdbuild.Env(b.Environ)); err != nil {
		return nil, "", err
	}
	paths, err := b.moveGatewayContext(stage)
	if err != nil {
		return nil, "", err
	}
	return paths, bdCommit, nil
}

// checkGatewayContextInputs refuses the inputs StageGatewayContext cannot stage
// from, before anything runs, and returns rev and the bd pin trimmed. Both must
// be full commits, the form a pin takes (pins.ParsePins), so the context's bd
// label names one commit rather than an abbreviation another commit can claim.
func (b Builder) checkGatewayContextInputs(rev string) (string, string, error) {
	if b.Out == "" {
		return "", "", errors.New("daemonimage: no output directory for the gt-bd build context")
	}
	if !filepath.IsAbs(b.Out) {
		return "", "", fmt.Errorf("daemonimage: output directory %q is not absolute; a build runs in a worktree, not in the caller's directory", b.Out)
	}
	if rev = strings.TrimSpace(rev); rev == "" {
		return "", "", errors.New("daemonimage: no rev to stage the gt-bd build context from")
	}
	if !fullCommit.MatchString(rev) {
		return "", "", fmt.Errorf("daemonimage: rev %q is not a full 40-character lowercase hex commit", rev)
	}
	if b.GTSrc == "" {
		return "", "", errors.New("daemonimage: no gt source checkout named")
	}
	bdPin := strings.TrimSpace(b.BDCommit)
	if bdPin == "" {
		return "", "", errors.New("daemonimage: no bd commit to build bd.real from")
	}
	if !fullCommit.MatchString(bdPin) {
		return "", "", fmt.Errorf("daemonimage: bd commit %q is not a full 40-character lowercase hex commit", bdPin)
	}
	for _, p := range []struct{ name, dir string }{{"output directory", b.Out}, {"gt source", b.GTSrc}, {"bd source", b.BDSrc}} {
		if p.dir == "" {
			continue // fetched from its pin; the fetch refuses a pin that cannot be used
		}
		if err := refuseCanonicalClone(p.name, p.dir); err != nil {
			return "", "", err
		}
	}
	return rev, bdPin, nil
}

// buildBDOnly builds the image's real bd into stage as bd.real, from src at
// commit, and writes nothing else there: one clean worktree and one go build,
// stamped as bdBinary stamps it.
func (b Builder) buildBDOnly(ctx context.Context, src, commit, stage string, env []string) error {
	return b.build(ctx, bdBinary(src, commit, bdRealName), stage, env)
}

// moveGatewayContext moves bd.real, bdgw and verbs.json from stage into b.Out in
// gatewayContextNames order and returns their paths. A move that fails takes
// back out every file this call already moved, reporting anything it could not,
// so a caller never reads a clean refusal over a context this call left changed.
func (b Builder) moveGatewayContext(stage string) ([]string, error) {
	ops := b.fileOps()
	paths := make([]string, 0, len(gatewayContextNames))
	for _, name := range gatewayContextNames {
		path := filepath.Join(b.Out, name)
		if err := ops.rename(filepath.Join(stage, name), path); err != nil {
			err = fmt.Errorf("daemonimage: cannot move %s into %s: %w", name, b.Out, err)
			if undoErr := undoGatewayContext(ops, paths); undoErr != nil {
				return nil, fmt.Errorf("%w; %w", err, undoErr)
			}
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// undoGatewayContext takes back out of b.Out every file moveGatewayContext
// moved, and reports the ones it could not.
func undoGatewayContext(ops fileOps, paths []string) error {
	var failed []string
	for i := len(paths) - 1; i >= 0; i-- {
		if err := ops.remove(paths[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) != 0 {
		return fmt.Errorf("daemonimage: cannot take the staged build context back out: %s", strings.Join(failed, "; "))
	}
	return nil
}
