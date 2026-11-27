package cmd

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// runClusterCapture is one capture run: it splits ref and resolves out under
// d.workDir before anything is read or written, so a bad ref or a path outside
// the work dir fails without reaching the cluster or creating a file. It then
// reads the object and writes the capture to path with mode 0600, replacing an
// existing file; a write that fails is returned.
func runClusterCapture(ctx context.Context, d clusterCaptureDeps, ref, namespace, out string) error {
	kind, name, err := parseClusterCaptureRef(ref)
	if err != nil {
		return err
	}
	path, err := clusterCaptureOutPath(d.workDir, out)
	if err != nil {
		return err
	}
	data, err := captureFromCluster(ctx, d, kind, name, namespace)
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing the capture of %s/%s to %s: %w", kind, name, path, err)
	}
	return nil
}
