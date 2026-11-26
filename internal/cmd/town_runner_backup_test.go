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

// The two live reads carry the fields the API server assigns, and the two
// files a backup writes hold the same objects without them.
const (
	backupDeploymentLiveJSON = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"forgejo-runner","namespace":"forgejo","uid":"0d1f2b3c-4a5b-6c7d-8e9f-0a1b2c3d4e5f","resourceVersion":"4711","generation":12,"creationTimestamp":"2026-11-01T00:00:00Z","managedFields":[{"manager":"kubectl"}],"annotations":{"deployment.kubernetes.io/revision":"12","kubectl.kubernetes.io/last-applied-configuration":"{}","team":"town"}},"spec":{"replicas":1},"status":{"readyReplicas":1}}` + "\n"
	backupConfigMapLiveJSON  = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"forgejo-runner-config","namespace":"forgejo","uid":"1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d","resourceVersion":"4712","creationTimestamp":"2026-11-01T00:00:00Z","managedFields":[{"manager":"kubectl"}]},"data":{"config.yml":"runner:\n  capacity: 1\n"}}` + "\n"
	backupConfigMapSecret    = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"forgejo-runner-config","namespace":"forgejo"},"data":{}}`
	backupDeploymentWritten  = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"annotations":{"team":"town"},"name":"forgejo-runner","namespace":"forgejo"},"spec":{"replicas":1}}` + "\n"
	backupConfigMapWritten   = `{"apiVersion":"v1","data":{"config.yml":"runner:\n  capacity: 1\n"},"kind":"ConfigMap","metadata":{"name":"forgejo-runner-config","namespace":"forgejo"}}` + "\n"
)

var backupWrittenFiles = [][2]string{{"deployment.json", backupDeploymentWritten}, {"configmap.json", backupConfigMapWritten}}

// backupAnswer is what one live read returns.
type backupAnswer struct {
	body string
	exit int
	err  error
}

// backupKubectl answers the reads by resource and fails any other call.
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

// TestRunnerBackup: a backup writes the live objects without the server's
// fields at 0600, even over files an earlier roll left, prints the restore
// command, and refuses what it cannot write without writing anything.
func TestRunnerBackup(t *testing.T) {
	t.Parallel()
	failure := errors.New("the cluster is unreachable")
	liveDeployment, liveConfigMap := backupAnswer{body: backupDeploymentLiveJSON}, backupAnswer{body: backupConfigMapLiveJSON}
	for _, tc := range []struct {
		name    string
		read    func(*testing.T) *recordingKubectl
		mutate  func(*runnerApplier)
		dir     func(*testing.T) string
		seed    bool
		wantErr string
	}{
		{name: "writes both files and prints the restore"},
		{name: "replaces the files an earlier roll left", seed: true},
		{name: "refuses a relative directory", dir: func(*testing.T) string { return "backup" }, wantErr: "not absolute"},
		{name: "refuses unreadable values", mutate: func(a *runnerApplier) {
			a.values = func() (forgejorunner.Values, error) { return forgejorunner.Values{}, failure }
		}, wantErr: "reading the control cluster's runner values"},
		{name: "refuses a render that fails", mutate: func(a *runnerApplier) {
			a.build = func(forgejorunner.Values) (forgejorunner.Manifests, error) { return forgejorunner.Manifests{}, failure }
		}, wantErr: "rendering the runner"},
		{name: "refuses two namespaces", mutate: func(a *runnerApplier) {
			a.build = func(v forgejorunner.Values) (forgejorunner.Manifests, error) {
				m, err := forgejorunner.Build(v)
				m.ConfigMap.Metadata.Namespace = "another"
				return m, err
			}
		}, wantErr: "cannot apply both"},
		{name: "refuses a deployment read that fails", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, backupAnswer{err: failure}, liveConfigMap)
		}, wantErr: "reading the live deployment/forgejo-runner"},
		{name: "refuses a deployment read that exits nonzero", read: func(t *testing.T) *recordingKubectl { return backupKubectl(t, backupAnswer{exit: 1}, liveConfigMap) }, wantErr: "kubectl exited 1"},
		{name: "refuses a configmap read that is not JSON", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, liveDeployment, backupAnswer{body: "forgejo-runner-config updated"})
		}, wantErr: "not one object"},
		{name: "refuses a configmap read that is null", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, liveDeployment, backupAnswer{body: "null"})
		}, wantErr: "not one object"},
		{name: "refuses a configmap read of another kind", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, liveDeployment, backupAnswer{body: backupConfigMapSecret})
		}, wantErr: `kind "Secret", not the ConfigMap`},
		{name: "refuses a configmap read of another object", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, liveDeployment, backupAnswer{body: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"another-configmap","namespace":"forgejo"}}`})
		}, wantErr: `named "another-configmap", not "forgejo-runner-config"`},
		{name: "refuses a configmap read with no metadata", read: func(t *testing.T) *recordingKubectl {
			return backupKubectl(t, liveDeployment, backupAnswer{body: `{"apiVersion":"v1","kind":"ConfigMap","metadata":"a string"}`})
		}, wantErr: `named "", not "forgejo-runner-config"`},
		{name: "refuses a directory it cannot make", dir: func(t *testing.T) string {
			file := filepath.Join(testutil.PrivateTempDir(t), "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatalf("make the blocking file: %v", err)
			}
			return filepath.Join(file, "backup")
		}, wantErr: "making the backup directory"},
		{name: "refuses a file it cannot write", dir: func(t *testing.T) string {
			dir := filepath.Join(testutil.PrivateTempDir(t), "backup")
			if err := os.MkdirAll(filepath.Join(dir, "configmap.json"), 0o700); err != nil {
				t.Fatalf("make the occupied path: %v", err)
			}
			return dir
		}, wantErr: "writing the backup file"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			applier := testRunnerApplier(backupKubectl(t, liveDeployment, liveConfigMap))
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
			if tc.seed {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("make the backup directory: %v", err)
				}
				for _, f := range backupWrittenFiles {
					path := filepath.Join(dir, f[0])
					if err := os.WriteFile(path, []byte("an earlier roll's backup"), 0o644); err != nil {
						t.Fatalf("write the earlier %s: %v", f[0], err)
					}
					if err := os.Chmod(path, 0o644); err != nil {
						t.Fatalf("widen the earlier %s: %v", f[0], err)
					}
				}
			}

			names, err := applier.Backup(context.Background(), dir)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
				}
				if out.Len() != 0 {
					t.Errorf("a failed backup printed %q: a refusal reports no restore command", out.String())
				}
				if _, statErr := os.Stat(dir); tc.dir == nil && !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("stat %s = %v, want nothing written after a refusal", dir, statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("backup: %v", err)
			}
			if got := strings.Join(names, ","); got != "configmap.json,deployment.json" {
				t.Errorf("names = %q, want the ConfigMap file and then the Deployment file", got)
			}
			for _, f := range backupWrittenFiles {
				path := filepath.Join(dir, f[0])
				fi, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat %s: %v", f[0], err)
				}
				if got := fi.Mode().Perm(); got != 0o600 {
					t.Errorf("%s mode = %04o, want 0600", f[0], got)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != f[1] {
					t.Errorf("%s = %q (%v), want %q", f[0], data, err, f[1])
				}
			}
			want := "kubectl --context colima-gt-control -n forgejo apply -f " + dir + "/configmap.json -f " + dir + "/deployment.json"
			if got := strings.TrimSpace(out.String()); got != want {
				t.Errorf("printed %q, want %q", got, want)
			}
		})
	}
}
