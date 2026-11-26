package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
)

// runnerRollKubeContext is the context the operator's own shell reaches the control cluster through (docs/reference.md).
const runnerRollKubeContext = "colima-gt-control"

// The files a backup writes, and the modes it gives them and their directory.
const (
	runnerBackupConfigMapFile  = "configmap.json"
	runnerBackupDeploymentFile = "deployment.json"
	runnerBackupFileMode       = 0o600
	runnerBackupDirMode        = 0o700
)

// runnerBackupObject is one live object a backup writes: its kubectl noun, the
// name and namespace the render gives it, and its file under the backup dir.
type runnerBackupObject struct {
	kind      string
	name      string
	namespace string
	file      string
}

// runnerBackupObjects is the pair a backup writes, in the order the restore
// command applies them, so the ConfigMap lands before the Deployment mounts it.
func runnerBackupObjects(m forgejorunner.Manifests) []runnerBackupObject {
	return []runnerBackupObject{
		{kind: "configmap", name: m.ConfigMap.Metadata.Name, namespace: m.ConfigMap.Metadata.Namespace, file: runnerBackupConfigMapFile},
		{kind: "deployment", name: m.Deployment.Metadata.Name, namespace: m.Deployment.Metadata.Namespace, file: runnerBackupDeploymentFile},
	}
}

// Backup writes the live Deployment and ConfigMap the roll is about to replace
// as JSON under dir, then prints the command that applies them back. It reads
// both before writing any, so a failed read leaves dir absent; a failed write names its file.
func (a runnerApplier) Backup(ctx context.Context, dir string) ([]string, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("the backup directory %q is not absolute, so its files would land where the process runs", dir)
	}
	m, err := a.manifests()
	if err != nil {
		return nil, err
	}
	objects := runnerBackupObjects(m)
	if objects[0].namespace != objects[1].namespace {
		return nil, fmt.Errorf("the live %s is in namespace %q and the live %s in %q, so one restore command cannot apply both",
			objects[0].kind, objects[0].namespace, objects[1].kind, objects[1].namespace)
	}
	bodies := make([][]byte, len(objects))
	for i, object := range objects {
		if bodies[i], err = a.readRunnerBackupObject(ctx, object); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, runnerBackupDirMode); err != nil {
		return nil, fmt.Errorf("making the backup directory %s: %w", dir, err)
	}
	for i, object := range objects {
		path := filepath.Join(dir, object.file)
		if err := os.WriteFile(path, bodies[i], runnerBackupFileMode); err != nil {
			return nil, fmt.Errorf("writing the backup file %s: %w", path, err)
		}
	}
	fmt.Fprintln(a.out, runnerRestoreCommand(dir, objects[0].namespace, objects))
	return []string{objects[0].file, objects[1].file}, nil
}

// readRunnerBackupObject reads one live object as the JSON kubectl prints. The
// error names the object, and a nonzero exit its status, never the output, since
// an object's fields can hold a value that is a secret.
func (a runnerApplier) readRunnerBackupObject(ctx context.Context, object runnerBackupObject) ([]byte, error) {
	resource := object.kind + "/" + object.name
	out, exit, err := a.kubectl(ctx, "", nil, "kubectl", "get", resource, "-n", object.namespace, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("reading the live %s: %w", resource, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("reading the live %s: kubectl exited %d", resource, exit)
	}
	if !json.Valid([]byte(out)) {
		return nil, fmt.Errorf("reading the live %s: kubectl printed no JSON", resource)
	}
	return []byte(out), nil
}

// runnerRestoreCommand is the command that applies a backup's files over the
// live objects, in the order the roll wrote them. A path is shell-quoted, so a
// town root holding a space still prints a command a shell runs.
func runnerRestoreCommand(dir, namespace string, objects []runnerBackupObject) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kubectl --context %s -n %s apply", runnerRollKubeContext, namespace)
	for _, object := range objects {
		fmt.Fprintf(&b, " -f %s", shellWord(filepath.Join(dir, object.file)))
	}
	return b.String()
}
