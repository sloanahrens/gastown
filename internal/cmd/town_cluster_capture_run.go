package cmd

import (
	"context"
	"fmt"
)

// runClusterCapture is one capture run: it splits ref and resolves out under
// d.workDir before anything is read or written, so a bad ref or a path outside
// the work dir fails without reaching the cluster or creating a file. The
// capture itself comes next (gt-yxy3ll reads the object, gt-7xo0k2 writes it);
// until then a checked run says the capture is not written.
func runClusterCapture(ctx context.Context, d clusterCaptureDeps, ref, namespace, out string) error { //nolint:unparam // the read gt-yxy3ll adds to this run takes ctx; the signature is fixed by gt-iniggu
	kind, name, err := parseClusterCaptureRef(ref)
	if err != nil {
		return err
	}
	path, err := clusterCaptureOutPath(d.workDir, out)
	if err != nil {
		return err
	}
	return fmt.Errorf("the capture of %s/%s to %s is not written yet", kind, name, path)
}
