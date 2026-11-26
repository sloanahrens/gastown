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

// runnerRollKubeContext is the context the operator's shell reaches the control
// cluster through. Before changing it, read the Tier sweep runner section of
// docs/reference.md.
const runnerRollKubeContext = "colima-gt-control"

// The files a backup writes, and the modes it gives them and their directory.
const (
	runnerBackupConfigMapFile  = "configmap.json"
	runnerBackupDeploymentFile = "deployment.json"
	runnerBackupFileMode       = 0o600
	runnerBackupDirMode        = 0o700
)

// runnerBackupObject is one live object: the kubectl noun and kind, its identity
// in the render, and its file under the backup dir.
type runnerBackupObject struct {
	noun, kind, name, namespace, file string
}

// runnerBackupObjects is the pair a backup writes, the ConfigMap first so the
// restore applies it before the Deployment that mounts it.
func runnerBackupObjects(m forgejorunner.Manifests) []runnerBackupObject {
	return []runnerBackupObject{
		{noun: "configmap", kind: "ConfigMap", name: m.ConfigMap.Metadata.Name, namespace: m.ConfigMap.Metadata.Namespace, file: runnerBackupConfigMapFile},
		{noun: "deployment", kind: "Deployment", name: m.Deployment.Metadata.Name, namespace: m.Deployment.Metadata.Namespace, file: runnerBackupDeploymentFile},
	}
}

// Backup writes the live Deployment and ConfigMap the roll is about to replace
// as JSON that applies back cleanly under dir, then prints the command that
// applies them. It reads both before writing any, so a failed read writes nothing.
// The roll's calls run on the current kubectl context, so Backup checks that
// context first and refuses to read unless it is runnerRollKubeContext: the files
// must come from the cluster the printed command names.
func (a runnerApplier) Backup(ctx context.Context, dir string) ([]string, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("the backup directory %q is not absolute, so its files would land where the process runs", dir)
	}
	if err := a.checkRunnerBackupContext(ctx); err != nil {
		return nil, err
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
		if err := writeRunnerBackupFile(path, bodies[i]); err != nil {
			return nil, fmt.Errorf("writing the backup file %s: %w", path, err)
		}
	}
	fmt.Fprintln(a.out, runnerRestoreCommand(dir, objects[0].namespace, objects))
	return []string{objects[0].file, objects[1].file}, nil
}

// checkRunnerBackupContext refuses unless the current kubectl context is
// runnerRollKubeContext. An unreadable context is a refusal too, never a pass.
// The error names the context found and the one required; kubectl's output is
// not echoed on a nonzero exit.
func (a runnerApplier) checkRunnerBackupContext(ctx context.Context) error {
	out, exit, err := a.kubectl(ctx, "", nil, "kubectl", "config", "current-context")
	if err != nil {
		return fmt.Errorf("reading the current kubectl context: %w; the backup needs %s", err, runnerRollKubeContext)
	}
	if exit != 0 {
		return fmt.Errorf("reading the current kubectl context: kubectl exited %d; the backup needs %s", exit, runnerRollKubeContext)
	}
	if found := strings.TrimSpace(out); found != runnerRollKubeContext {
		return fmt.Errorf("the current kubectl context is %q, not %s, so the backup would read a cluster the restore command does not name", found, runnerRollKubeContext)
	}
	return nil
}

// readRunnerBackupObject reads one live object as the JSON a backup writes. The
// error names the object, and a nonzero exit its status, never the output.
func (a runnerApplier) readRunnerBackupObject(ctx context.Context, object runnerBackupObject) ([]byte, error) {
	resource := object.noun + "/" + object.name
	out, exit, err := a.kubectl(ctx, "", nil, "kubectl", "get", resource, "-n", object.namespace, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("reading the live %s: %w", resource, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("reading the live %s: kubectl exited %d", resource, exit)
	}
	body, err := runnerBackupApplyJSON([]byte(out), object.kind, object.name)
	if err != nil {
		return nil, fmt.Errorf("reading the live %s: %w", resource, err)
	}
	return body, nil
}

// runnerBackupApplyJSON is one live object as a backup writes it: kubectl's JSON
// without the fields the server and apply own. It refuses any kind or name but
// the one asked for, so a Secret or another object never reaches a file.
func runnerBackupApplyJSON(raw []byte, kind, name string) ([]byte, error) {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("kubectl printed JSON that is not one object")
	}
	if got, _ := object["kind"].(string); got != kind {
		return nil, fmt.Errorf("kubectl printed kind %q, not the %s that was read", got, kind)
	}
	metadata, _ := object["metadata"].(map[string]any)
	if got, _ := metadata["name"].(string); got != name {
		return nil, fmt.Errorf("kubectl printed the %s named %q, not %q", kind, got, name)
	}
	if annotations, ok := metadata["annotations"].(map[string]any); ok {
		for _, field := range []string{"deployment.kubernetes.io/revision", "kubectl.kubernetes.io/last-applied-configuration"} {
			delete(annotations, field)
		}
		if len(annotations) == 0 {
			delete(metadata, "annotations")
		}
	}
	// The fields the API server assigns: dropped so the restore's apply sets them.
	for _, field := range []string{"creationTimestamp", "generation", "managedFields", "resourceVersion", "selfLink", "uid"} {
		delete(metadata, field)
	}
	delete(object, "status")
	out, err := json.Marshal(object)
	return append(out, '\n'), err
}

// writeRunnerBackupFile writes one backup file at 0600. The mode a new file gets
// never reaches one that already exists, so the file is chmod'ed after writing.
func writeRunnerBackupFile(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, runnerBackupFileMode)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if chmodErr := f.Chmod(runnerBackupFileMode); err == nil {
		err = chmodErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
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
