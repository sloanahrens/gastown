package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	readSHA        = strings.Repeat("ab", 32)
	readKubeconfs  = planFlags{applyKubeconfig: "/keys/apply.kubeconfig"}
	readWebDoc     = []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: gastown\n")
	readWebFile    = planRecordFile{Name: "deployment-web.yaml", Phase: planPhaseWorkloads, SHA256: "00", Revision: "3", ResourceVersion: "7"}
	readApprovalOK = []byte(`{"act":"apply-plan","cluster":"control","plan_sha256":"` + readSHA + `","proposal_id":"0123456789ab","approved_at":"2026-11-26T08:00:00Z","expires_at":"2026-11-26T09:00:00Z"}`)
)

// readPlanJSON is a plan.json of the given files. Marshalling these plain
// structs cannot fail, so a failure is a bug in the test itself.
func readPlanJSON(files ...planRecordFile) []byte {
	data, err := json.Marshal(planRecord{ID: readSHA, Files: files})
	if err != nil {
		panic(err)
	}
	return data
}

func writeReadFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClusterApplyReadLoadsPlan: a good plan directory returns its plan record and
// the one Deployment the plan holds, at its resourceVersion and phase kubeconfig.
func TestClusterApplyReadLoadsPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(readWebFile))

	record, held, err := loadApplyPlan(dir, readKubeconfs)
	if err != nil {
		t.Fatalf("loadApplyPlan returned %v, want the plan", err)
	}
	if record.ID != readSHA || len(record.Files) != 1 {
		t.Errorf("plan record = %+v, want the plan's id and its one file", record)
	}
	want := []applyDeployment{{key: "deployment/gastown/web", namespace: "gastown", name: "web", version: "7", kubeconfig: "/keys/apply.kubeconfig"}}
	if len(held) != len(want) || held[0] != want[0] {
		t.Errorf("held Deployments = %+v, want %+v", held, want)
	}
}

// TestClusterApplyReadRefusesBadPlan: each input that cannot be read or does not
// name a Deployment ends in an error and no plan, so no read reads as ok.
func TestClusterApplyReadRefusesBadPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		noDir    bool   // the plan directory does not exist
		plan     []byte // plan.json; nil writes the plan of readWebFile
		doc      []byte // deployment-web.yaml; nil writes readWebDoc
		docIsDir bool   // deployment-web.yaml is a directory, so it cannot be read
		flags    planFlags
		wantErr  string
	}{
		{name: "the plan directory is missing", noDir: true, flags: readKubeconfs, wantErr: "the plan record cannot be read"},
		{name: "the plan record is not JSON", plan: []byte("{"), flags: readKubeconfs, wantErr: "is not a plan record"},
		{name: "a file has a phase that is not one", plan: readPlanJSON(planRecordFile{Name: "deployment-web.yaml", Phase: "X", ResourceVersion: "7"}), flags: readKubeconfs, wantErr: "is not a plan phase"},
		{name: "a deployment file is unreadable", docIsDir: true, flags: readKubeconfs, wantErr: "cannot be read"},
		{name: "a deployment file has no kubeconfig", flags: planFlags{}, wantErr: "has files but no --apply-kubeconfig"},
		{name: "the apply kubeconfig reads as a flag", flags: planFlags{applyKubeconfig: "-x"}, wantErr: "starts with '-'"},
		{name: "a file name leaves the plan directory", plan: readPlanJSON(planRecordFile{Name: "../deployment-web.yaml", Phase: planPhaseWorkloads, ResourceVersion: "7"}), flags: readKubeconfs, wantErr: "not a file in the plan directory"},
		{name: "a file is not a Deployment", doc: []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  namespace: gastown\n"), flags: readKubeconfs, wantErr: "is not a Deployment"},
		{name: "a Deployment name is not a resource name", doc: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web/x\n  namespace: gastown\n"), flags: readKubeconfs, wantErr: "is not a resource name"},
		{name: "a Deployment namespace is not a namespace", doc: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: Gastown\n"), flags: readKubeconfs, wantErr: "is not a Kubernetes namespace name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "plan")
			if !tc.noDir {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				plan := tc.plan
				if plan == nil {
					plan = readPlanJSON(readWebFile)
				}
				writeReadFile(t, filepath.Join(dir, planFileName), plan)
				switch {
				case tc.docIsDir:
					if err := os.Mkdir(filepath.Join(dir, "deployment-web.yaml"), 0o700); err != nil {
						t.Fatal(err)
					}
				case tc.doc != nil:
					writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), tc.doc)
				default:
					writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
				}
			}
			record, held, err := loadApplyPlan(dir, tc.flags)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("loadApplyPlan returned %v, want an error naming %q", err, tc.wantErr)
			}
			if record.ID != "" || held != nil {
				t.Errorf("loadApplyPlan returned %+v and %+v with its error, want zero values", record, held)
			}
		})
	}
}

// TestClusterApplyReadApproval: a good approval file returns its record; a missing,
// unreadable or malformed file ends in an error and no record.
func TestClusterApplyReadApproval(t *testing.T) {
	t.Parallel()
	t.Run("a good approval file returns its record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "approval.json")
		writeReadFile(t, path, readApprovalOK)
		rec, err := readApplyApproval(path)
		if err != nil {
			t.Fatalf("readApplyApproval returned %v, want the record", err)
		}
		if rec.Act != "apply-plan" || rec.Cluster != "control" || rec.PlanSHA256 != readSHA || rec.ProposalID != "0123456789ab" {
			t.Errorf("record = %+v, want the apply-plan approval of the plan", rec)
		}
	})
	tests := []struct {
		name    string
		missing bool   // no file at the path
		isDir   bool   // the path is a directory, so it cannot be read
		data    []byte // the file's bytes when it is written
		wantErr string
	}{
		{name: "a missing approval file", missing: true, wantErr: "there is no approval record"},
		{name: "an unreadable approval file", isDir: true, wantErr: "cannot be read"},
		{name: "a malformed approval file", data: []byte("{}"), wantErr: "is not a valid approval record"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "approval.json")
			switch {
			case tc.isDir:
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case !tc.missing:
				writeReadFile(t, path, tc.data)
			}
			rec, err := readApplyApproval(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("readApplyApproval returned %v, want an error naming %q", err, tc.wantErr)
			}
			if rec.PlanSHA256 != "" || rec.Cluster != "" {
				t.Errorf("readApplyApproval returned %+v with its error, want a zero record", rec)
			}
		})
	}
}
