package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// nodePruneKubeTimeout bounds the ListJSON and Apply calls of a node-prune run. The run
// is on the heartbeat, so a cluster that does not answer costs this much and no more.
const nodePruneKubeTimeout = 30 * time.Second

// kubectlNodePrune drives the node-prune Job with kubectl. It names no context, so
// in a pod kubectl uses the pod's service account, as kubectlUpdateJob does. The
// manifest is written under dir first, because update.RunFunc has no stdin.
type kubectlNodePrune struct {
	dir string
	run update.RunFunc
}

// nodePruneWaitTimeout bounds WaitComplete: the Job's 900 second deadline, kubectl's
// own --timeout of 960 seconds, and a 60 second margin, so a Job that outlives its
// deadline is answered by kubectl's timeout and not by the kill.
const nodePruneWaitTimeout = (nodeImagePruneDeadlineSec+60)*time.Second + 60*time.Second

// nodePruneCallTimeout bounds the Logs and Delete calls of a node-prune run.
const nodePruneCallTimeout = 120 * time.Second

// kubectlNodePrune is the cluster seam a node-prune run drives.
var _ nodePruneKube = kubectlNodePrune{}

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

// runBounded runs one kubectl call under a deadline of limit. A call that outlives it
// was killed and is an error naming the verb and the limit. update.ExecRun reports a
// killed process as its own error, not ctx.Err(), so the deadline's state is read too.
func (k kubectlNodePrune) runBounded(ctx context.Context, limit time.Duration, verb string, args ...string) (string, int, error) {
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, exit, err := k.run(bounded, "", nil, "kubectl", args...)
	if err == nil {
		return out, exit, nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return "", -1, fmt.Errorf("kubectl %s did not finish within its %ds deadline: %w", verb, int64(limit/time.Second), context.DeadlineExceeded)
	}
	return "", -1, fmt.Errorf("running kubectl %s: %w", verb, err)
}

// WaitComplete blocks until the node-prune Job completes, under nodePruneWaitTimeout.
// kubectl's own timeout is the Job's deadline plus 60 seconds. A non-zero exit is an
// error naming the verb and the status, not kubectl's output.
func (k kubectlNodePrune) WaitComplete(ctx context.Context, name string) error {
	timeout := "--timeout=" + strconv.Itoa(nodeImagePruneDeadlineSec+60) + "s"
	_, exit, err := k.runBounded(ctx, nodePruneWaitTimeout, "wait", "-n", nodeImagePruneNamespace, "wait", "--for=condition=complete", "job/"+name, timeout)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("kubectl wait for the node-prune Job exited %d", exit)
	}
	return nil
}

// Logs returns the node-prune Job's log, untrimmed, under nodePruneCallTimeout. A
// non-zero exit is an error naming the verb and the status, not kubectl's output.
func (k kubectlNodePrune) Logs(ctx context.Context, name string) ([]byte, error) {
	out, exit, err := k.runBounded(ctx, nodePruneCallTimeout, "logs", "-n", nodeImagePruneNamespace, "logs", "job/"+name)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, fmt.Errorf("kubectl logs of the node-prune Job exited %d", exit)
	}
	return []byte(out), nil
}

// Delete removes the node-prune Job, and succeeds when it is already gone, under
// nodePruneCallTimeout. A non-zero exit is an error naming the verb and the status,
// not kubectl's output.
func (k kubectlNodePrune) Delete(ctx context.Context, name string) error {
	_, exit, err := k.runBounded(ctx, nodePruneCallTimeout, "delete", "-n", nodeImagePruneNamespace, "delete", "job", name, "--ignore-not-found", "--wait=true")
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("kubectl delete of the node-prune Job exited %d", exit)
	}
	return nil
}
