package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/internal/testutil"
)

// The two objects the control values render, as the cluster serves them: a
// backup writes these bytes unchanged.
const (
	backupDeploymentJSON = `{"kind":"Deployment","metadata":{"name":"forgejo-runner","namespace":"forgejo"}}` + "\n"
	backupConfigMapJSON  = `{"kind":"ConfigMap","metadata":{"name":"forgejo-runner-config","namespace":"forgejo"}}` + "\n"
)

// backupAnswer is what one live read returns.
type backupAnswer struct {
	body string
	exit int
	err  error
}

// backupKubectl answers the backup's two reads, and fails any other call so a
// wrong argv cannot pass.
func backupKubectl(t *testing.T, deployment, configmap backupAnswer) *recordingKubectl {
	answers := map[string]backupAnswer{"deployment/forgejo-runner": deployment, "configmap/forgejo-runner-config": configmap}
	return &recordingKubectl{answer: func(call recordedKubectlCall) (string, int, error) {
		if len(call.args) < 2 || call.args[0] != "get" {
			t.Errorf("unexpected cluster call %v: a backup reads the two live objects", call.args)
			return "", 0, nil
		}
		answer, ok := answers[call.args[1]]
		if !ok {
			t.Errorf("unexpected cluster read %v", call.args)
			return "", 0, nil
		}
		return answer.body, answer.exit, answer.err
	}}
}

// crossNamespaceRender renders as build does with the ConfigMap moved to
// another namespace, so one restore command cannot name both objects.
func crossNamespaceRender(build func(forgejorunner.Values) (forgejorunner.Manifests, error)) func(forgejorunner.Values) (forgejorunner.Manifests, error) {
	return func(v forgejorunner.Values) (forgejorunner.Manifests, error) {
		m, err := build(v)
		if err != nil {
			return forgejorunner.Manifests{}, err
		}
		m.ConfigMap.Metadata.Namespace = "another"
		return m, nil
	}
}

// TestRunnerBackupWritesBothFilesAsRead: the two files hold the bytes the
// cluster read returned, at mode 0600, under the directory the roll named.
func TestRunnerBackupWritesBothFilesAsRead(t *testing.T) {
	t.Parallel()
	applier := testRunnerApplier(backupKubectl(t, backupAnswer{body: backupDeploymentJSON}, backupAnswer{body: backupConfigMapJSON}))
	dir := filepath.Join(testutil.PrivateTempDir(t), "backup")

	names, err := applier.Backup(context.Background(), dir)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if got := strings.Join(names, ","); got != "configmap.json,deployment.json" {
		t.Errorf("names = %q, want the ConfigMap file and then the Deployment file", got)
	}
	for _, f := range [][2]string{{"deployment.json", backupDeploymentJSON}, {"configmap.json", backupConfigMapJSON}} {
		file, want := f[0], f[1]
		path := filepath.Join(dir, file)
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", file, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", file, got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if string(data) != want {
			t.Errorf("%s = %q, want the bytes the cluster read returned (%q)", file, data, want)
		}
	}
}

// TestRunnerBackupPrintsTheRestoreCommand: a good backup prints the one command
// that applies both files back, the ConfigMap before the Deployment.
func TestRunnerBackupPrintsTheRestoreCommand(t *testing.T) {
	t.Parallel()
	applier := testRunnerApplier(backupKubectl(t, backupAnswer{body: backupDeploymentJSON}, backupAnswer{body: backupConfigMapJSON}))
	out := &bytes.Buffer{}
	applier.out = out

	if _, err := applier.Backup(context.Background(), filepath.Join(testutil.PrivateTempDir(t), "backup")); err != nil {
		t.Fatalf("backup: %v", err)
	}

	got := strings.TrimSpace(out.String())
	if prefix := "kubectl --context colima-gt-control -n forgejo apply -f "; !strings.HasPrefix(got, prefix) {
		t.Errorf("printed %q, want it to start with %q", got, prefix)
	}
	configmap, deployment := strings.Index(got, "configmap.json"), strings.Index(got, "deployment.json")
	if configmap < 0 || deployment < 0 {
		t.Fatalf("printed %q, want it to name both backup files", got)
	}
	if configmap > deployment {
		t.Errorf("printed %q, want the ConfigMap applied before the Deployment that mounts it", got)
	}
}

// TestRunnerBackupRefusesWhatItCannotWrite: every refusal a backup makes returns
// its error, writes no file, and prints no restore command.
func TestRunnerBackupRefusesWhatItCannotWrite(t *testing.T) {
	t.Parallel()
	failure := errors.New("the cluster is unreachable")
	for _, tc := range []struct {
		name   string
		read   func(*testing.T) *recordingKubectl
		mutate func(*runnerApplier)
		dir    func(*testing.T) string
		want   string
	}{
		{name: "a relative directory", dir: func(*testing.T) string { return "backup" }, want: "not absolute"},
		{name: "values that cannot be read", mutate: func(a *runnerApplier) {
			a.values = func() (forgejorunner.Values, error) { return forgejorunner.Values{}, failure }
		}, want: "reading the control cluster's runner values"},
		{name: "a render that fails", mutate: func(a *runnerApplier) {
			a.build = func(forgejorunner.Values) (forgejorunner.Manifests, error) { return forgejorunner.Manifests{}, failure }
		}, want: "rendering the runner"},
		{name: "objects in two namespaces", mutate: func(a *runnerApplier) { a.build = crossNamespaceRender(a.build) }, want: "cannot apply both"},
		{name: "a deployment read that fails", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, backupAnswer{err: failure}, backupAnswer{body: backupConfigMapJSON})
		}, want: "reading the live deployment/forgejo-runner"},
		{name: "a deployment read that exits nonzero", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, backupAnswer{exit: 1}, backupAnswer{body: backupConfigMapJSON})
		}, want: "kubectl exited 1"},
		{name: "a configmap read that is not JSON", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, backupAnswer{body: backupDeploymentJSON}, backupAnswer{body: "configmap/forgejo-runner-config updated"})
		}, want: "printed no JSON"},
		{name: "a directory that cannot be made", dir: func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatalf("make the blocking file: %v", err)
			}
			return filepath.Join(file, "backup")
		}, want: "making the backup directory"},
		{name: "a backup file that cannot be written", dir: func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), "backup")
			if err := os.MkdirAll(filepath.Join(dir, "configmap.json"), 0o700); err != nil {
				t.Fatalf("make the occupied path: %v", err)
			}
			return dir
		}, want: "writing the backup file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applier := testRunnerApplier(backupKubectl(t, backupAnswer{body: backupDeploymentJSON}, backupAnswer{body: backupConfigMapJSON}))
			if tc.read != nil {
				applier.kubectl = tc.read(t).run
			}
			out := &bytes.Buffer{}
			applier.out = out
			if tc.mutate != nil {
				tc.mutate(&applier)
			}
			dir := filepath.Join(testutil.PrivateTempDir(t), "backup")
			if tc.dir != nil {
				dir = tc.dir(t)
			}

			_, err := applier.Backup(context.Background(), dir)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("a failed backup printed %q: a refusal reports no restore command", out.String())
			}
			if _, statErr := os.Stat(dir); tc.dir == nil && !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("stat %s = %v, want nothing written after a refusal", dir, statErr)
			}
		})
	}
}
