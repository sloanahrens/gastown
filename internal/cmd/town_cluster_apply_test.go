package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/update"
)

// clusterApplyFileSHA is the hex SHA-256 of data, as plan.json records a plan file's hash.
func clusterApplyFileSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// shapeWebFile is readWebFile with the hash of readWebDoc, the bytes the plan
// directory holds for it. readWebFile's placeholder hash would now be refused.
var shapeWebFile = planRecordFile{Name: readWebFile.Name, Phase: readWebFile.Phase, SHA256: clusterApplyFileSHA(readWebDoc), Revision: readWebFile.Revision, ResourceVersion: readWebFile.ResourceVersion}

// shapeAbsentDoc is a Deployment the cluster does not hold: its plan file names
// no resourceVersion, so only the hash check reads it.
var shapeAbsentDoc = []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n  namespace: gastown\n")

// shapeAbsentFile is the plan record's entry for shapeAbsentDoc.
var shapeAbsentFile = planRecordFile{Name: "deployment-api.yaml", Phase: planPhaseWorkloads, SHA256: clusterApplyFileSHA(shapeAbsentDoc), Absent: true}

// shapeKube is a RunFunc that counts every cluster call, so a run that applies
// nothing can be shown to have made none.
func shapeKube(calls *int) update.RunFunc {
	return func(context.Context, string, []string, string, ...string) (string, int, error) {
		*calls++
		return "", 0, nil
	}
}

// shapeVerifier is an approvalVerifier that counts its calls and returns want,
// so a run can be shown to have verified once, with a chosen result, or not at
// all.
func shapeVerifier(calls *int, want error) approvalVerifier {
	return func(clusterapproval.Record, planRecord) error {
		*calls++
		return want
	}
}

// shapeApprovalFor is an approval record approving the plan sha256 sha.
func shapeApprovalFor(sha string) []byte {
	return []byte(`{"act":"apply-plan","cluster":"control","plan_sha256":"` + sha + `","proposal_id":"0123456789ab","approved_at":"2026-11-26T08:00:00Z","expires_at":"2026-11-26T09:00:00Z"}`)
}

// shapeLiveKube is a RunFunc that records every kubectl call's arguments and
// stdin and answers each read with out at exit, failing with err, so a test can
// choose the cluster's answer and show which calls the run made.
type shapeLiveKube struct {
	calls  [][]string
	stdins []string
	out    string
	exit   int
	err    error
}

func (k *shapeLiveKube) run(ctx context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
	k.calls = append(k.calls, args)
	k.stdins = append(k.stdins, string(runStdinOf(ctx)))
	return k.out, k.exit, k.err
}

// shapeApplies counts recorded calls that carry an apply verb, so a run that
// applied nothing can be shown to have made no apply call.
func shapeApplies(calls [][]string) int {
	var n int
	for _, args := range calls {
		for _, arg := range args {
			if arg == "apply" {
				n++
				break
			}
		}
	}
	return n
}

// shapeLiveAt is the get answer for the plan's web Deployment at version.
func shapeLiveAt(version string) string {
	return `{"metadata":{"name":"web","namespace":"gastown","resourceVersion":"` + version + `"}}`
}

// TestClusterShapeApplyUnreadablePlan: a plan that cannot be read returns the
// reader's error and makes no cluster call.
func TestClusterShapeApplyUnreadablePlan(t *testing.T) {
	t.Parallel()
	var calls int
	err := runTownClusterApply(context.Background(), shapeKube(&calls),
		filepath.Join(t.TempDir(), "plan"), filepath.Join(t.TempDir(), "approval.json"), readKubeconfs, nil)
	if err == nil || !strings.Contains(err.Error(), "the plan record cannot be read") {
		t.Fatalf("runTownClusterApply returned %v, want the plan read error", err)
	}
	if calls != 0 {
		t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
	}
}

// shapeFlags gives both phases a kubeconfig, so a plan holding files in each can run.
var shapeFlags = planFlags{policyKubeconfig: "/keys/policy.kubeconfig", applyKubeconfig: "/keys/apply.kubeconfig"}

// shapePolicyDoc is a static ValidatingAdmissionPolicy, a document phase P applies.
var shapePolicyDoc = []byte("apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingAdmissionPolicy\nmetadata:\n  name: gt-policy\n")

// shapePolicyFile is the plan record's phase P entry for shapePolicyDoc.
var shapePolicyFile = planRecordFile{Name: "policy.yaml", Phase: planPhaseStatic, SHA256: clusterApplyFileSHA(shapePolicyDoc)}

// shapeApplyCalls returns each call that carries an apply verb, in call order,
// as its arguments and the stdin it was sent.
func shapeApplyCalls(k *shapeLiveKube) (calls [][]string, stdins []string) {
	for i, call := range k.calls {
		if slices.Contains(call, "apply") {
			calls = append(calls, call)
			stdins = append(stdins, k.stdins[i])
		}
	}
	return calls, stdins
}

// TestClusterShapeApplyReadablePlan: a plan and approval that read, an approval
// the verifier accepts, and a live Deployment at the plan's resourceVersion are
// applied: the run re-reads the Deployment under the plan's kubeconfig, then
// applies its file once under the apply kubeconfig, sending the checked bytes on
// stdin, and returns nil.
func TestClusterShapeApplyReadablePlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapeWebFile))
	writeReadFile(t, filepath.Join(dir, "approval.json"), readApprovalOK)
	kube := &shapeLiveKube{out: shapeLiveAt("7")}
	var verified int
	if err := runTownClusterApply(context.Background(), kube.run, dir, filepath.Join(dir, "approval.json"), readKubeconfs, shapeVerifier(&verified, nil)); err != nil {
		t.Fatalf("runTownClusterApply returned %v, want the plan applied", err)
	}
	if verified != 1 {
		t.Errorf("runTownClusterApply called verify %d times, want once", verified)
	}
	applies, stdins := shapeApplyCalls(kube)
	if len(kube.calls) != 2 || len(applies) != 1 {
		t.Fatalf("runTownClusterApply made the calls %v, want one read and one apply", kube.calls)
	}
	want := []string{"--kubeconfig", readKubeconfs.applyKubeconfig, "apply", "-f", "-"}
	if !slices.Equal(applies[0], want) {
		t.Errorf("the apply's arguments %v, want %v", applies[0], want)
	}
	if stdins[0] != string(readWebDoc) {
		t.Errorf("the apply's stdin %q, want the checked bytes %q", stdins[0], string(readWebDoc))
	}
}

// TestClusterShapeApplyPhases: phase P's file is applied first, under the policy
// kubeconfig, then phase W's, under the apply kubeconfig, each from its own
// checked bytes, and no file is applied under the other phase's.
func TestClusterShapeApplyPhases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "policy.yaml"), shapePolicyDoc)
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapePolicyFile, shapeWebFile))
	approvalPath := filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, readApprovalOK)
	kube := &shapeLiveKube{out: shapeLiveAt("7")}
	var verified int
	if err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, shapeFlags, shapeVerifier(&verified, nil)); err != nil {
		t.Fatalf("runTownClusterApply returned %v, want the plan applied", err)
	}
	applies, stdins := shapeApplyCalls(kube)
	want := [][]string{
		{"--kubeconfig", shapeFlags.policyKubeconfig, "apply", "-f", "-"},
		{"--kubeconfig", shapeFlags.applyKubeconfig, "apply", "-f", "-"},
	}
	if !slices.EqualFunc(applies, want, func(a, b []string) bool { return slices.Equal(a, b) }) {
		t.Errorf("runTownClusterApply applied %v, want phase P's file under the policy kubeconfig, then phase W's under the apply kubeconfig: %v", applies, want)
	}
	wantStdin := []string{string(shapePolicyDoc), string(readWebDoc)}
	if !slices.Equal(stdins, wantStdin) {
		t.Errorf("runTownClusterApply applied the stdin %q, want each file's checked bytes %q", stdins, wantStdin)
	}
}

// TestClusterShapeApplyDocumentKinds: a plan file holding a Secret in either
// phase, or a static policy document or Role in phase W, is refused before any
// cluster call, so verify never runs and nothing is applied. A Secret after a
// Deployment document, or inside a List or a typed SecretList, is refused too, so
// every document and every list item is read, and a document that does not decode
// is refused.
func TestClusterShapeApplyDocumentKinds(t *testing.T) {
	t.Parallel()
	secretDoc := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: api-key\n  namespace: gastown\n")
	roleDoc := []byte("apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: r\n  namespace: gastown\n")
	listSecretDoc := []byte("apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Secret\n  metadata:\n    name: api-key\n    namespace: gastown\n")
	typedListSecretDoc := []byte("apiVersion: v1\nkind: SecretList\nitems:\n- apiVersion: v1\n  metadata:\n    name: api-key\n    namespace: gastown\n")
	tests := []struct {
		name    string
		phase   string
		doc     []byte
		wantErr string
	}{
		{name: "a Secret in phase P", phase: planPhaseStatic, doc: secretDoc, wantErr: "holds a Secret"},
		{name: "a Secret in phase W", phase: planPhaseWorkloads, doc: secretDoc, wantErr: "holds a Secret"},
		{name: "a Secret after a Deployment document", phase: planPhaseWorkloads, doc: append(append([]byte{}, readWebDoc...), append([]byte("---\n"), secretDoc...)...), wantErr: "holds a Secret"},
		{name: "a Secret inside a List", phase: planPhaseWorkloads, doc: listSecretDoc, wantErr: "holds a Secret"},
		{name: "a Secret inside a SecretList", phase: planPhaseStatic, doc: typedListSecretDoc, wantErr: "holds a Secret"},
		{name: "a ValidatingAdmissionPolicy in phase W", phase: planPhaseWorkloads, doc: shapePolicyDoc, wantErr: "in phase W"},
		{name: "a Role in phase W", phase: planPhaseWorkloads, doc: roleDoc, wantErr: "in phase W"},
		{name: "a document that does not decode", phase: planPhaseWorkloads, doc: []byte("kind: [unclosed\n"), wantErr: "does not decode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeReadFile(t, filepath.Join(dir, "doc.yaml"), tc.doc)
			writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(planRecordFile{Name: "doc.yaml", Phase: tc.phase, SHA256: clusterApplyFileSHA(tc.doc), Absent: true}))
			approvalPath := filepath.Join(dir, "approval.json")
			writeReadFile(t, approvalPath, readApprovalOK)
			kube := &shapeLiveKube{out: shapeLiveAt("7")}
			var verified int
			err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, shapeFlags, shapeVerifier(&verified, nil))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "nothing was applied") {
				t.Fatalf("runTownClusterApply returned %v, want a refusal naming %q and nothing applied", err, tc.wantErr)
			}
			if verified != 0 || len(kube.calls) != 0 {
				t.Errorf("runTownClusterApply called verify %d times and made the calls %v, want none", verified, kube.calls)
			}
		})
	}
}

// TestClusterShapeApplyMissingFlag: applyPlanFiles reads every phase's kubeconfig
// before its first apply, so a phase without its flag refuses with nothing
// applied — phase P's file is not applied before phase W's missing flag is found
// — a file whose phase is not one of the plan's two is refused rather than
// dropped, and the refusal names the apply command, not the plan.
func TestClusterShapeApplyMissingFlag(t *testing.T) {
	t.Parallel()
	policy := applyPlanFile{file: shapePolicyFile, data: shapePolicyDoc}
	web := applyPlanFile{file: shapeWebFile, data: readWebDoc}
	tests := []struct {
		name    string
		files   []applyPlanFile
		flags   planFlags
		wantErr string
	}{
		{name: "phase P's kubeconfig is missing", files: []applyPlanFile{policy}, flags: planFlags{applyKubeconfig: "/keys/apply.kubeconfig"}, wantErr: "--policy-kubeconfig"},
		{name: "phase W's kubeconfig is missing after phase P's file", files: []applyPlanFile{policy, web}, flags: planFlags{policyKubeconfig: "/keys/policy.kubeconfig"}, wantErr: "--apply-kubeconfig"},
		{name: "a file's phase is not one of the plan's two", files: []applyPlanFile{{file: planRecordFile{Name: "x.yaml", Phase: "X"}, data: shapePolicyDoc}}, flags: shapeFlags, wantErr: "is not a plan phase"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls int
			kube := func(context.Context, string, []string, string, ...string) (string, int, error) {
				calls++
				return "", 0, nil
			}
			_, err := applyPlanFiles(context.Background(), kube, t.TempDir(), tc.files, tc.flags)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "nothing was applied") {
				t.Fatalf("applyPlanFiles returned %v, want a refusal naming %q and nothing applied", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "gt town cluster plan") {
				t.Errorf("applyPlanFiles returned the plan command's error %v, want the apply's", err)
			}
			if calls != 0 {
				t.Errorf("applyPlanFiles made %d calls, want none", calls)
			}
		})
	}
}

// TestClusterShapeApplyPartial: an apply that fails stops the run, with no
// further apply call, and the error names the files applied and not applied and
// says nothing was rolled back.
func TestClusterShapeApplyPartial(t *testing.T) {
	t.Parallel()
	orderedNames := []string{"policy.yaml", "deployment-web.yaml"}
	tests := []struct {
		name    string
		failing string
		wantErr string
	}{
		{name: "the phase P apply fails", failing: "policy.yaml", wantErr: "applied nothing and not applied policy.yaml, deployment-web.yaml"},
		{name: "the phase W apply fails", failing: "deployment-web.yaml", wantErr: "applied policy.yaml and not applied deployment-web.yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeReadFile(t, filepath.Join(dir, "policy.yaml"), shapePolicyDoc)
			writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
			writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapePolicyFile, shapeWebFile))
			approvalPath := filepath.Join(dir, "approval.json")
			writeReadFile(t, approvalPath, readApprovalOK)
			var applied []string
			kube := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
				if !slices.Contains(args, "apply") {
					return shapeLiveAt("7"), 0, nil
				}
				file := orderedNames[len(applied)]
				applied = append(applied, file)
				if file == tc.failing {
					return "", 1, nil
				}
				return "", 0, nil
			}
			var verified int
			err := runTownClusterApply(context.Background(), kube, dir, approvalPath, shapeFlags, shapeVerifier(&verified, nil))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "nothing was rolled back") {
				t.Fatalf("runTownClusterApply returned %v, want a partial-apply error naming %q", err, tc.wantErr)
			}
			if applied[len(applied)-1] != tc.failing {
				t.Errorf("runTownClusterApply applied %v after the failing %s, want none", applied, tc.failing)
			}
		})
	}
}

// TestClusterShapeApplyClusterMoved: a held Deployment the cluster no longer
// holds at the plan's resourceVersion ends the run before any apply, saying
// nothing was applied: another version, no version, an object that is gone, and
// a read that failed are each refused.
func TestClusterShapeApplyClusterMoved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		exit    int
		err     error
		wantErr string
	}{
		{name: "the live Deployment is at another resourceVersion", out: shapeLiveAt("8"), wantErr: "not the version the plan names"},
		{name: "the live Deployment has no resourceVersion", out: `{"metadata":{}}`, wantErr: "not the version the plan names"},
		{name: "the cluster no longer holds the Deployment", wantErr: "no longer holds"},
		{name: "the read exits nonzero", out: `{}`, exit: 1, wantErr: "cannot re-read"},
		{name: "the read fails", err: errors.New("kubectl not found"), wantErr: "cannot re-read"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
			writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapeWebFile))
			approvalPath := filepath.Join(dir, "approval.json")
			writeReadFile(t, approvalPath, readApprovalOK)
			kube := &shapeLiveKube{out: tc.out, exit: tc.exit, err: tc.err}
			var verified int
			err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, readKubeconfs, shapeVerifier(&verified, nil))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "nothing was applied") {
				t.Fatalf("runTownClusterApply returned %v, want the moved-cluster refusal naming %q and nothing applied", err, tc.wantErr)
			}
			if verified != 1 {
				t.Errorf("runTownClusterApply called verify %d times, want once", verified)
			}
			if len(kube.calls) != 1 || shapeApplies(kube.calls) != 0 {
				t.Errorf("runTownClusterApply made the calls %v, want one read and no apply", kube.calls)
			}
		})
	}
}

// TestClusterShapeApplyOtherPlan: an approval that authorizes another plan's
// sha256 is refused before verify, so the verifier is never called and no
// cluster call is made.
func TestClusterShapeApplyOtherPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapeWebFile))
	approvalPath := filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, shapeApprovalFor(strings.Repeat("cd", 32)))
	var calls, verified int
	err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, approvalPath, readKubeconfs, shapeVerifier(&verified, nil))
	if err == nil || !strings.Contains(err.Error(), "another plan") || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("runTownClusterApply returned %v, want an error saying the approval is for another plan and nothing was applied", err)
	}
	if verified != 0 {
		t.Errorf("runTownClusterApply called verify %d times, want none", verified)
	}
	if calls != 0 {
		t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
	}
}

// TestClusterShapeApplyVerifierError: a verifier that refuses the approval ends
// the run with its error, saying nothing was applied, and no cluster call.
func TestClusterShapeApplyVerifierError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapeWebFile))
	approvalPath := filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, readApprovalOK)
	var calls, verified int
	err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, approvalPath, readKubeconfs, shapeVerifier(&verified, errors.New("the approval expired")))
	if err == nil || !strings.Contains(err.Error(), "the approval expired") || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("runTownClusterApply returned %v, want the verifier's error saying nothing was applied", err)
	}
	if verified != 1 {
		t.Errorf("runTownClusterApply called verify %d times, want once", verified)
	}
	if calls != 0 {
		t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
	}
}

// TestClusterShapeApplyUnreadableApproval: a plan that reads and an approval
// that does not returns the approval reader's error and makes no cluster call.
func TestClusterShapeApplyUnreadableApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		missing bool   // no approval file at the path
		data    []byte // the approval file's bytes when it is written
		wantErr string
	}{
		{name: "a missing approval record", missing: true, wantErr: "there is no approval record"},
		{name: "a malformed approval record", data: []byte("{}"), wantErr: "is not a valid approval record"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
			writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapeWebFile))
			approvalPath := filepath.Join(dir, "approval.json")
			if !tc.missing {
				writeReadFile(t, approvalPath, tc.data)
			}
			var calls int
			err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, approvalPath, readKubeconfs, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runTownClusterApply returned %v, want an error naming %q", err, tc.wantErr)
			}
			if calls != 0 {
				t.Errorf("runTownClusterApply made %d cluster calls, want none", calls)
			}
		})
	}
}

// shapeLedgerLines reads the ledger the runs write beside the approval record at
// approvalPath, one decoded entry per line.
func shapeLedgerLines(t *testing.T, approvalPath string) []applyLedgerEntry {
	t.Helper()
	path := applyLedgerPath(approvalPath)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the ledger %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	entries := make([]applyLedgerEntry, 0, len(lines))
	for _, line := range lines {
		var entry applyLedgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("the ledger line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// TestClusterShapeApplyLedger: every run appends one line to the ledger beside
// the approval record, a refused run included. The applied run's line names its
// plan, the file it applied and the result applied; the refused run's line says
// refused and names no file. Each line carries a time, and the ledger is created
// 0644, the mode the operator reads it in.
func TestClusterShapeApplyLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapeWebFile))
	approvalPath := filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, readApprovalOK)
	kube := &shapeLiveKube{out: shapeLiveAt("7")}
	var verified int
	if err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, readKubeconfs, shapeVerifier(&verified, nil)); err != nil {
		t.Fatalf("the applied run returned %v, want the plan applied", err)
	}
	// A refused run: the plan file no longer hashes to what plan.json records.
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), append(append([]byte{}, readWebDoc...), "# edited\n"...))
	if err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, readKubeconfs, shapeVerifier(&verified, nil)); err == nil {
		t.Fatal("the refused run was applied, want a refusal")
	}
	lines := shapeLedgerLines(t, approvalPath)
	if len(lines) != 2 {
		t.Fatalf("the ledger holds %d lines, want one per run", len(lines))
	}
	applied, refused := lines[0], lines[1]
	if applied.Result != applyResultApplied || applied.Plan != readSHA || !slices.Equal(applied.Objects, []string{"deployment-web.yaml"}) {
		t.Errorf("the applied run's line = %+v, want the plan %s, the file it applied and the result %q", applied, readSHA, applyResultApplied)
	}
	if refused.Result != applyResultRefused || len(refused.Objects) != 0 || refused.Plan != readSHA {
		t.Errorf("the refused run's line = %+v, want the plan %s, no file and the result %q", refused, readSHA, applyResultRefused)
	}
	if applied.TS.IsZero() || refused.TS.IsZero() {
		t.Errorf("the ledger lines hold the times %v and %v, want both recorded", applied.TS, refused.TS)
	}
	info, err := os.Stat(applyLedgerPath(approvalPath))
	if err != nil {
		t.Fatalf("stating the ledger: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("the ledger's mode is %04o, want 0644 so the operator can read it", perm)
	}
}

// TestClusterShapeApplyLedgerPartial: a run that applies phase P's file and then
// fails phase W's records the result partial and the one file it applied.
func TestClusterShapeApplyLedgerPartial(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeReadFile(t, filepath.Join(dir, "policy.yaml"), shapePolicyDoc)
	writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
	writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapePolicyFile, shapeWebFile))
	approvalPath := filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, readApprovalOK)
	kube := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
		switch {
		case !slices.Contains(args, "apply"):
			return shapeLiveAt("7"), 0, nil
		case slices.Contains(args, shapeFlags.applyKubeconfig):
			return "", 1, nil
		}
		return "", 0, nil
	}
	var verified int
	if err := runTownClusterApply(context.Background(), kube, dir, approvalPath, shapeFlags, shapeVerifier(&verified, nil)); err == nil {
		t.Fatal("a failed phase W apply was reported as applied, want a partial apply")
	}
	lines := shapeLedgerLines(t, approvalPath)
	if len(lines) != 1 {
		t.Fatalf("the ledger holds %d lines, want one for the run", len(lines))
	}
	if lines[0].Result != applyResultPartial || !slices.Equal(lines[0].Objects, []string{"policy.yaml"}) || lines[0].Plan != readSHA {
		t.Errorf("the partial run's line = %+v, want the plan %s, the file it applied and the result %q", lines[0], readSHA, applyResultPartial)
	}
}

// TestClusterShapeApplyLedgerNotRecorded: a run whose ledger line cannot be
// written is an error saying so — a good apply included, so an apply that
// changed the cluster is never reported as recorded — and a refused run keeps
// its own error beside it.
func TestClusterShapeApplyLedgerNotRecorded(t *testing.T) {
	t.Parallel()
	t.Run("a good apply", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
		writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapeWebFile))
		approvalPath := filepath.Join(dir, "approval.json")
		writeReadFile(t, approvalPath, readApprovalOK)
		// A directory at the ledger path makes the append impossible.
		if err := os.Mkdir(applyLedgerPath(approvalPath), 0o700); err != nil {
			t.Fatal(err)
		}
		kube := &shapeLiveKube{out: shapeLiveAt("7")}
		var verified int
		err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, readKubeconfs, shapeVerifier(&verified, nil))
		if err == nil || !strings.Contains(err.Error(), "ledger line could not be written") {
			t.Fatalf("runTownClusterApply = %v, want an error saying the ledger line could not be written", err)
		}
		if shapeApplies(kube.calls) != 1 {
			t.Errorf("the run made %d apply calls, want the one apply its error still reports", shapeApplies(kube.calls))
		}
	})
	t.Run("a refused run", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeReadFile(t, filepath.Join(dir, "deployment-web.yaml"), readWebDoc)
		writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(shapeWebFile))
		approvalPath := filepath.Join(dir, "approval.json") // no approval record: the run refuses
		if err := os.Mkdir(applyLedgerPath(approvalPath), 0o700); err != nil {
			t.Fatal(err)
		}
		var calls int
		err := runTownClusterApply(context.Background(), shapeKube(&calls), dir, approvalPath, readKubeconfs, nil)
		if err == nil || !strings.Contains(err.Error(), "there is no approval record") || !strings.Contains(err.Error(), "ledger line could not be written") {
			t.Fatalf("runTownClusterApply = %v, want the refusal and the ledger error", err)
		}
	})
}

// shapeLedgerEntry is a good ledger line for the refusal rows to append.
var shapeLedgerEntry = applyLedgerEntry{TS: time.Now().UTC(), Plan: readSHA, Objects: []string{"deployment-web.yaml"}, Result: applyResultApplied}

// shapeLedgerFake is an applyLedgerWriter whose write or close fails, the two
// failures a full disk would cause.
type shapeLedgerFake struct {
	writeErr error
	closeErr error
	written  []byte
}

func (f *shapeLedgerFake) Write(b []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.written = append(f.written, b...)
	return len(b), nil
}

func (f *shapeLedgerFake) Close() error { return f.closeErr }

// TestClusterShapeApplyLedgerRefuses: an append that cannot build its line, or
// whose ledger path is a symlink, cannot be checked, cannot be opened, cannot be
// written or cannot be closed, is an error and records nothing.
func TestClusterShapeApplyLedgerRefuses(t *testing.T) {
	t.Parallel()
	t.Run("a line that cannot be encoded", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), applyLedgerName)
		entry := shapeLedgerEntry
		entry.TS = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		err := appendApplyLedger(path, entry)
		if err == nil || !strings.Contains(err.Error(), "cannot be built") {
			t.Fatalf("appendApplyLedger = %v, want the line cannot be built", err)
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("appendApplyLedger wrote %s, want nothing", path)
		}
	})
	t.Run("a symlink at the ledger path", func(t *testing.T) {
		t.Parallel()
		target := filepath.Join(t.TempDir(), "target.jsonl")
		path := filepath.Join(t.TempDir(), applyLedgerName)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		err := appendApplyLedger(path, shapeLedgerEntry)
		if err == nil || !strings.Contains(err.Error(), "is a symlink") {
			t.Fatalf("appendApplyLedger = %v, want the symlink refused", err)
		}
		if _, statErr := os.Stat(target); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("appendApplyLedger wrote through the symlink to %s, want nothing", target)
		}
	})
	t.Run("a ledger path that cannot be checked", func(t *testing.T) {
		t.Parallel()
		parent := filepath.Join(t.TempDir(), "file")
		writeReadFile(t, parent, []byte("x"))
		err := appendApplyLedger(filepath.Join(parent, applyLedgerName), shapeLedgerEntry)
		if err == nil || !strings.Contains(err.Error(), "cannot be checked") {
			t.Fatalf("appendApplyLedger = %v, want the path cannot be checked", err)
		}
	})
	t.Run("a ledger that cannot be opened", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), applyLedgerName)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		err := appendApplyLedger(path, shapeLedgerEntry)
		if err == nil || !strings.Contains(err.Error(), "cannot be opened") {
			t.Fatalf("appendApplyLedger = %v, want the ledger cannot be opened", err)
		}
	})
	t.Run("a ledger write that fails", func(t *testing.T) {
		t.Parallel()
		fake := &shapeLedgerFake{writeErr: errors.New("no space left")}
		ledger := applyLedger{path: filepath.Join(t.TempDir(), applyLedgerName), open: func(string) (applyLedgerWriter, error) { return fake, nil }}
		err := ledger.append(shapeLedgerEntry)
		if err == nil || !strings.Contains(err.Error(), "cannot be written") {
			t.Fatalf("append = %v, want the ledger cannot be written", err)
		}
		if len(fake.written) != 0 {
			t.Errorf("append wrote %q past a failing write, want nothing", fake.written)
		}
	})
	t.Run("a ledger close that fails", func(t *testing.T) {
		t.Parallel()
		fake := &shapeLedgerFake{closeErr: errors.New("close failed")}
		ledger := applyLedger{path: filepath.Join(t.TempDir(), applyLedgerName), open: func(string) (applyLedgerWriter, error) { return fake, nil }}
		err := ledger.append(shapeLedgerEntry)
		if err == nil || !strings.Contains(err.Error(), "cannot be written") {
			t.Fatalf("append = %v, want the ledger cannot be written", err)
		}
		if !strings.HasSuffix(string(fake.written), "\n") {
			t.Errorf("append wrote %q, want one complete line", fake.written)
		}
	})
}

// TestClusterShapeApplyFileHash: a plan file whose bytes differ from the sha256
// plan.json records for it, or that cannot be read, or whose name leaves the plan
// directory, is refused before the approval is read and before any cluster call,
// so no apply is made and the verifier never runs.
func TestClusterShapeApplyFileHash(t *testing.T) {
	t.Parallel()
	tamperedWeb := append(append([]byte{}, readWebDoc...), "# edited\n"...)
	tamperedAPI := append(append([]byte{}, shapeAbsentDoc...), "# edited\n"...)
	tests := []struct {
		name    string
		files   []planRecordFile
		write   map[string][]byte // the plan directory's files; a file not named here is missing
		wantErr string
	}{
		{
			name:    "a held Deployment's bytes differ from its sha256",
			files:   []planRecordFile{shapeWebFile},
			write:   map[string][]byte{"deployment-web.yaml": tamperedWeb},
			wantErr: "does not match the sha256",
		},
		{
			name:    "an absent Deployment's bytes differ from its sha256",
			files:   []planRecordFile{shapeWebFile, shapeAbsentFile},
			write:   map[string][]byte{"deployment-web.yaml": readWebDoc, "deployment-api.yaml": tamperedAPI},
			wantErr: "does not match the sha256",
		},
		{
			name:    "an absent Deployment's file is missing",
			files:   []planRecordFile{shapeWebFile, shapeAbsentFile},
			write:   map[string][]byte{"deployment-web.yaml": readWebDoc},
			wantErr: "cannot be read",
		},
		{
			name:    "a file name leaves the plan directory",
			files:   []planRecordFile{shapeWebFile, {Name: "../secret.yaml", Phase: planPhaseWorkloads, Absent: true}},
			write:   map[string][]byte{"deployment-web.yaml": readWebDoc},
			wantErr: "not a file in the plan directory",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, data := range tc.write {
				writeReadFile(t, filepath.Join(dir, name), data)
			}
			writeReadFile(t, filepath.Join(dir, planFileName), readPlanJSON(tc.files...))
			approvalPath := filepath.Join(dir, "approval.json")
			writeReadFile(t, approvalPath, readApprovalOK)
			kube := &shapeLiveKube{out: shapeLiveAt("7")}
			var verified int
			err := runTownClusterApply(context.Background(), kube.run, dir, approvalPath, readKubeconfs, shapeVerifier(&verified, nil))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "nothing was applied") {
				t.Fatalf("runTownClusterApply returned %v, want a refusal naming %q and nothing applied", err, tc.wantErr)
			}
			if verified != 0 {
				t.Errorf("runTownClusterApply called verify %d times, want none", verified)
			}
			if len(kube.calls) != 0 || shapeApplies(kube.calls) != 0 {
				t.Errorf("runTownClusterApply made the calls %v, want none", kube.calls)
			}
		})
	}
}
