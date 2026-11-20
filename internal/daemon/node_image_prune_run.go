package daemon

import (
	"context"
	"fmt"
)

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
		return nodePruneReport{}, fmt.Errorf("node-prune: apply job %s: %w", name, err)
	}
	if err := kube.WaitComplete(ctx, name); err != nil {
		return nodePruneReport{}, fmt.Errorf("node-prune: wait for job %s: %w", name, err)
	}
	// Untrimmed: the parser needs the log's final newline to tell a whole log from a cut one.
	out, err := kube.Logs(ctx, name)
	if err != nil {
		return nodePruneReport{}, fmt.Errorf("node-prune: read the log of job %s: %w", name, err)
	}
	if err := kube.Delete(ctx, name); err != nil {
		return nodePruneReport{}, fmt.Errorf("node-prune: delete job %s: %w", name, err)
	}
	return parseNodePruneLog(out)
}
