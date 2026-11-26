package cmd

import (
	"context"
	"fmt"
)

// runClusterCapture is one capture run: it splits ref and resolves out under
// d.workDir before anything is read or written, so a bad ref or a path outside
// the work dir fails without reaching the cluster or creating a file. It then
// reads and captures the object. Writing the capture comes next (gt-7xo0k2);
// until then a run that read the object says the capture is not written.
func runClusterCapture(ctx context.Context, d clusterCaptureDeps, ref, namespace, out string) error {
	kind, name, err := parseClusterCaptureRef(ref)
	if err != nil {
		return err
	}
	path, err := clusterCaptureOutPath(d.workDir, out)
	if err != nil {
		return err
	}
	if _, err := captureFromCluster(ctx, d, kind, name, namespace); err != nil {
		return err
	}
	return fmt.Errorf("the capture of %s/%s to %s is not written yet", kind, name, path)
}
