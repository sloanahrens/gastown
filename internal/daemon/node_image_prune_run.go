package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// nodePruneDeleteTimeout bounds the Job's delete, which runs after the caller's
// context may be canceled, so a hung delete cannot block the run forever.
const nodePruneDeleteTimeout = 60 * time.Second

// nodePruneKube is the cluster seam a node-prune run drives. ListJSON reads the gt
// namespace's Jobs, Deployments and Pods; the rest apply, wait for, read and delete the Job.
type nodePruneKube interface {
	ListJSON(ctx context.Context) ([]byte, error)
	Apply(ctx context.Context, manifest []byte) error
	WaitComplete(ctx context.Context, name string) error
	Logs(ctx context.Context, name string) ([]byte, error)
	Delete(ctx context.Context, name string) error
}

// runNodeImagePrune runs the node-prune Job name on the control node and returns
// the report its log gives. Without apply the Job only plans.
//
// Once the apply was attempted, the Job is deleted on every path. The apply may
// have created the Job before it returned an error, and the next heartbeat would
// otherwise meet AlreadyExists for the same name.
func runNodeImagePrune(ctx context.Context, kube nodePruneKube, name, image string, keep int, apply bool) (nodePruneReport, error) {
	listed, err := kube.ListJSON(ctx)
	if err != nil {
		return nodePruneReport{}, fmt.Errorf("node-prune: list the gt namespace: %w", err)
	}
	spares, err := nodeImagePruneSpares(listed)
	if err != nil {
		return nodePruneReport{}, fmt.Errorf("node-prune: work out the spare digests: %w", err)
	}
	manifest, err := nodeImagePruneManifest(name, image, keep, apply, spares)
	if err != nil {
		return nodePruneReport{}, fmt.Errorf("node-prune: build the job manifest: %w", err)
	}
	if err := kube.Apply(ctx, manifest); err != nil {
		applyErr := fmt.Errorf("node-prune: apply job %s: %w", name, err)
		return nodePruneReport{}, deleteNodePruneJob(ctx, kube, name, applyErr)
	}
	out, err := readNodePruneJob(ctx, kube, name)
	err = deleteNodePruneJob(ctx, kube, name, err)
	if err != nil {
		return nodePruneReport{}, err
	}
	return parseNodePruneLog(out)
}

// readNodePruneJob waits for the applied Job and reads its log.
func readNodePruneJob(ctx context.Context, kube nodePruneKube, name string) ([]byte, error) {
	if err := kube.WaitComplete(ctx, name); err != nil {
		return nil, fmt.Errorf("node-prune: wait for job %s: %w", name, err)
	}
	// Untrimmed: the parser needs the log's final newline to tell a whole log from a cut one.
	out, err := kube.Logs(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("node-prune: read the log of job %s: %w", name, err)
	}
	return out, nil
}

// deleteNodePruneJob deletes the Job after the apply was attempted. The delete runs
// on a context the caller's cancellation cannot reach, so a canceled run still
// deletes it, and it has its own deadline. It returns cause, joined with the
// delete's own error when the delete fails too.
func deleteNodePruneJob(ctx context.Context, kube nodePruneKube, name string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nodePruneDeleteTimeout)
	defer cancel()
	if err := kube.Delete(ctx, name); err != nil {
		return errors.Join(cause, fmt.Errorf("node-prune: delete job %s: %w", name, err))
	}
	return cause
}
