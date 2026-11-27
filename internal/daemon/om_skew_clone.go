package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/constants"
)

// omSkewSrcDir is the om-skew check's own clone under the town; it is never
// rebuild_om's .runtime/om-src, which nothing in the pod keeps (gt-wm7cwr).
const omSkewSrcDir = ".runtime/om-skew-src"

// omSkewRefreshClone refreshes that clone from the om rig's origin, read from
// the rig's bare repository so no read moves a ref, and returns its directory.
func (d *Daemon) omSkewRefreshClone(townRoot string) (string, error) {
	bare := constants.RigBareRepoPath(filepath.Join(townRoot, omSkewRig))
	url, err := d.omSkewGit("--git-dir="+bare, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("git --git-dir=%s remote get-url origin: no url", bare)
	}
	src := filepath.Join(townRoot, filepath.FromSlash(omSkewSrcDir))
	if err := d.skewRefreshClone("om", src, url); err != nil {
		return "", err
	}
	return src, nil
}
