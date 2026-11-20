package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// nodePruneKubeTimeout bounds one kubectl call of a node-prune run. The run is on
// the heartbeat, so a cluster that does not answer costs this much and no more.
const nodePruneKubeTimeout = 30 * time.Second

// kubectlNodePrune drives the node-prune Job with kubectl. It names no context, so
// in a pod kubectl uses the pod's service account, as kubectlUpdateJob does. The
// manifest is written under dir first, because update.RunFunc has no stdin.
type kubectlNodePrune struct {
	dir string
	run update.RunFunc
}

// ListJSON returns the gt namespace's Jobs, Deployments and Pods as kubectl's JSON.
// A non-zero exit is an error naming the verb and the status, not kubectl's output.
// The bound is a context deadline, not --request-timeout (see kubectlUpdateJob.CreateJob).
func (k kubectlNodePrune) ListJSON(ctx context.Context) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, nodePruneKubeTimeout)
	defer cancel()
	out, exit, err := k.run(bounded, "", nil, "kubectl", "-n", nodeImagePruneNamespace, "get", "job,deploy,pod", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("running kubectl get: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("kubectl get of the gt namespace exited %d", exit)
	}
	return []byte(out), nil
}

// nodePruneManifestName names the manifest file by its content. Two applies of one
// manifest share a name and hold the same bytes; two different manifests never do.
func nodePruneManifestName(manifest []byte) string {
	sum := sha256.Sum256(manifest)
	return "node-prune-" + hex.EncodeToString(sum[:8]) + ".yaml"
}

// Apply creates the node-prune Job from manifest. The manifest is written under the
// update directory and removed when Apply returns. A non-zero exit is an error naming
// the verb and the status, not kubectl's output.
func (k kubectlNodePrune) Apply(ctx context.Context, manifest []byte) error {
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		return fmt.Errorf("create the update directory %s: %w", k.dir, err)
	}
	path := filepath.Join(k.dir, nodePruneManifestName(manifest))
	// Registered before the write, so a partial write is removed too. A manifest that
	// cannot be removed stays under the update directory; the caller acts on the apply's
	// result, so a failed cleanup is not reported as a failed apply.
	defer os.Remove(path)
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		return fmt.Errorf("write the node-prune manifest: %w", err)
	}

	bounded, cancel := context.WithTimeout(ctx, nodePruneKubeTimeout)
	defer cancel()
	_, exit, err := k.run(bounded, "", nil, "kubectl", "create", "-f", path)
	if err != nil {
		return fmt.Errorf("running kubectl create: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("kubectl create of the node-prune Job exited %d", exit)
	}
	return nil
}
