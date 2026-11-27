package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The five documents of a gt-sbx set, one per kind the gt-sbx apply orders: a
// static policy for phase P, an openshell ConfigMap, the agent-sandbox controller
// Deployment, the openshell StatefulSet, and the gt-bootstrap ConfigMap.
var (
	sbxPolicyDoc      = []byte("apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingAdmissionPolicy\nmetadata:\n  name: gt-policy\n")
	sbxConfigDoc      = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: openshell-config\n  namespace: openshell\n")
	sbxControllerDoc  = []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: agent-sandbox-controller\n  namespace: agent-sandbox-system\n")
	sbxStatefulSetDoc = []byte("apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: openshell\n  namespace: openshell\n")
	sbxBootstrapDoc   = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: gt-bootstrap\n  namespace: openshell\n")
)

// sbxFlags gives both phases a kubeconfig, as shapeFlags does, so a gt-sbx run can
// read and apply in both.
var sbxFlags = shapeFlags

// sbxPlanFiles lists a gt-sbx set's plan records in an order other than the apply's,
// so a run that follows the list instead of the order shows it. The StatefulSet
// records revision, and the controller records the revision and resourceVersion the
// cluster holds it at.
func sbxPlanFiles(statefulSetDoc []byte, statefulSetRevision string) []planRecordFile {
	return []planRecordFile{
		{Name: "w-statefulset.yaml", Phase: planPhaseWorkloads, SHA256: clusterApplyFileSHA(statefulSetDoc), Revision: statefulSetRevision},
		{Name: "w-bootstrap.yaml", Phase: planPhaseWorkloads, SHA256: clusterApplyFileSHA(sbxBootstrapDoc)},
		{Name: "w-config.yaml", Phase: planPhaseWorkloads, SHA256: clusterApplyFileSHA(sbxConfigDoc)},
		{Name: "w-controller.yaml", Phase: planPhaseWorkloads, SHA256: clusterApplyFileSHA(sbxControllerDoc), Revision: "2", ResourceVersion: "7"},
		{Name: "p-policy.yaml", Phase: planPhaseStatic, SHA256: clusterApplyFileSHA(sbxPolicyDoc)},
	}
}

// sbxDocs maps each plan file name to the bytes the plan directory holds for it.
func sbxDocs(statefulSetDoc []byte) map[string][]byte {
	return map[string][]byte{
		"w-statefulset.yaml": statefulSetDoc,
		"w-bootstrap.yaml":   sbxBootstrapDoc,
		"w-config.yaml":      sbxConfigDoc,
		"w-controller.yaml":  sbxControllerDoc,
		"p-policy.yaml":      sbxPolicyDoc,
	}
}

// sbxCall is one cluster call a gt-sbx run made: the kubeconfig it was bound to, its
// verb arguments, and a label naming what it did, such as "apply w-config.yaml".
type sbxCall struct {
	kubeconfig string
	args       []string
	label      string
}

// sbxKube is the cluster a gt-sbx run calls. It records every call with its label
// and answers the calls a run makes: an apply succeeds, a rollout completes, the
// controller is held at resourceVersion 7 and Available, and the StatefulSet's live
// object is statefulSet. The call labelled fail exits 1, so a test can stop the run
// at one call.
type sbxKube struct {
	names       map[string]string // document bytes to the plan file name they are
	calls       []sbxCall
	statefulSet string
	fail        string
}

func newSbxKube() *sbxKube {
	return &sbxKube{names: map[string]string{}, statefulSet: `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`}
}

func (k *sbxKube) run(ctx context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
	call := sbxCall{args: shapeVerb(args)}
	if len(args) >= 2 && args[0] == "--kubeconfig" {
		call.kubeconfig = args[1]
	}
	call.label = k.label(call.args, string(runStdinOf(ctx)))
	k.calls = append(k.calls, call)
	if call.label == k.fail {
		return "", 1, nil
	}
	return k.answer(call.args), 0, nil
}

// label names a call: the file an apply sent, the resource a rollout or read named,
// or the controller whose Available condition was read.
func (k *sbxKube) label(v []string, stdin string) string {
	switch {
	case len(v) > 0 && v[0] == "apply":
		if name, ok := k.names[stdin]; ok {
			return "apply " + name
		}
		return "apply <unknown document>"
	case len(v) > 2 && v[0] == "rollout":
		return "rollout " + v[2]
	case len(v) > 2 && v[0] == "get" && strings.HasPrefix(v[len(v)-1], "jsonpath"):
		_, name := sbxResource(v)
		return "available " + name
	case len(v) > 2 && v[0] == "get":
		kind, name := sbxResource(v)
		return "read " + kind + "/" + name
	}
	return "other " + strings.Join(v, " ")
}

// sbxResource returns the kind and name a get call reads, for a call of at least
// three arguments. kubectl takes them as one argument, deployment/x, as
// readLiveObject sends them, or as two, deployment x, as the rollout's Available
// read does.
func sbxResource(v []string) (string, string) {
	if kind, name, ok := strings.Cut(v[1], "/"); ok {
		return kind, name
	}
	return v[1], v[2]
}

// answer is the cluster's reply to a call that did not fail.
func (k *sbxKube) answer(v []string) string {
	if len(v) < 3 || v[0] != "get" {
		return ""
	}
	kind, _ := sbxResource(v)
	switch {
	case kind == "statefulset":
		return k.statefulSet
	case kind == "deployment" && strings.HasPrefix(v[len(v)-1], "jsonpath"):
		return "True"
	case kind == "deployment":
		return `{"metadata":{"name":"agent-sandbox-controller","namespace":"agent-sandbox-system","resourceVersion":"7"}}`
	}
	return ""
}

// labels returns the call labels in order.
func (k *sbxKube) labels() []string {
	out := make([]string, 0, len(k.calls))
	for _, call := range k.calls {
		out = append(out, call.label)
	}
	return out
}

// sbxRun writes a gt-sbx plan directory whose plan.json lists planFiles, with the
// documents docs in it and an approval for it, and runs it through
// runTownClusterApply against k. It returns what the run printed and its error.
func sbxRun(t *testing.T, k *sbxKube, planFiles []planRecordFile, docs map[string][]byte) (string, error) {
	t.Helper()
	dir := t.TempDir()
	for name, doc := range docs {
		writeReadFile(t, filepath.Join(dir, name), doc)
		k.names[string(doc)] = name
	}
	data, err := json.Marshal(planRecord{Cluster: clusterSbx, ID: readSHA, Files: planFiles})
	if err != nil {
		t.Fatal(err)
	}
	writeReadFile(t, filepath.Join(dir, planFileName), data)
	approvalPath := filepath.Join(dir, "approval.json")
	writeReadFile(t, approvalPath, readApprovalOK)
	var out bytes.Buffer
	var verified int
	err = runTownClusterApply(context.Background(), k.run, &out, dir, approvalPath, sbxFlags, shapeVerifier(&verified, nil))
	return out.String(), err
}

// TestClusterShapeSbxOrder: a gt-sbx plan listed in a scrambled order is applied in
// the gt-sbx order: phase P's policy first, then the openshell ConfigMap, then the
// controller Deployment with its rollout and Available condition read, then the
// StatefulSet with its rollout and ready replica count read, then gt-bootstrap last.
// The controller's recheck read runs before any apply.
func TestClusterShapeSbxOrder(t *testing.T) {
	t.Parallel()
	k := newSbxKube()
	out, err := sbxRun(t, k, sbxPlanFiles(sbxStatefulSetDoc, "3"), sbxDocs(sbxStatefulSetDoc))
	if err != nil {
		t.Fatalf("runTownClusterApply returned %v, want the gt-sbx plan applied", err)
	}
	want := []string{
		"read deployment/agent-sandbox-controller",
		"apply p-policy.yaml",
		"apply w-config.yaml",
		"apply w-controller.yaml",
		"rollout deployment/agent-sandbox-controller",
		"available agent-sandbox-controller",
		"apply w-statefulset.yaml",
		"rollout statefulset/openshell",
		"read statefulset/openshell",
		"apply w-bootstrap.yaml",
	}
	if got := k.labels(); !slices.Equal(got, want) {
		t.Errorf("runTownClusterApply made the calls %q, want %q", got, want)
	}
	for _, line := range []string{
		"kubectl rollout undo deployment/agent-sandbox-controller -n agent-sandbox-system --to-revision=2",
		"kubectl rollout undo statefulset/openshell -n openshell --to-revision=3",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("the run printed %q, want the undo line %q", out, line)
		}
	}
}

// TestClusterShapeSbxKubeconfigs: phase P's policy apply is bound to the policy
// kubeconfig and every other call of a gt-sbx run, the phase W applies, the rollouts
// and the reads, to the apply kubeconfig, so no workload is read or changed through
// the policy identity.
func TestClusterShapeSbxKubeconfigs(t *testing.T) {
	t.Parallel()
	k := newSbxKube()
	if _, err := sbxRun(t, k, sbxPlanFiles(sbxStatefulSetDoc, "3"), sbxDocs(sbxStatefulSetDoc)); err != nil {
		t.Fatalf("runTownClusterApply returned %v, want the gt-sbx plan applied", err)
	}
	var policy int
	for _, call := range k.calls {
		if call.kubeconfig == sbxFlags.policyKubeconfig {
			policy++
			if call.label != "apply p-policy.yaml" {
				t.Errorf("the policy kubeconfig was used for %q, want only the phase P apply", call.label)
			}
			continue
		}
		if call.kubeconfig != sbxFlags.applyKubeconfig {
			t.Errorf("%q was bound to kubeconfig %q, want the apply kubeconfig", call.label, call.kubeconfig)
		}
	}
	if policy != 1 {
		t.Errorf("the policy kubeconfig was used %d times, want once", policy)
	}
}

// TestClusterShapeSbxStatefulSetStops: a StatefulSet that does not come back ready
// stops the gt-sbx apply: its rollout or its replica read fails, the run ends on that
// call, gt-bootstrap is never applied, and the run's rollback line for the StatefulSet
// was printed before the first apply with the revision plan.json records.
func TestClusterShapeSbxStatefulSetStops(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		statefulSet string
		fail        string
		wantErr     string
		wantLast    string
	}{
		{
			name:        "ready replicas below the desired count",
			statefulSet: `{"spec":{"replicas":1},"status":{"readyReplicas":0}}`,
			wantErr:     "statefulset/openshell has 0 ready replicas of 1",
			wantLast:    "read statefulset/openshell",
		},
		{
			name:        "ready replicas omitted at zero",
			statefulSet: `{"spec":{"replicas":1},"status":{}}`,
			wantErr:     "statefulset/openshell has 0 ready replicas of 1",
			wantLast:    "read statefulset/openshell",
		},
		{
			name:     "rollout status does not complete",
			fail:     "rollout statefulset/openshell",
			wantErr:  "statefulset/openshell did not complete its rollout",
			wantLast: "rollout statefulset/openshell",
		},
		{
			name:     "replica read fails",
			fail:     "read statefulset/openshell",
			wantErr:  "reading the readiness of statefulset/openshell",
			wantLast: "read statefulset/openshell",
		},
		{
			name:        "spec.replicas is unread",
			statefulSet: `{"spec":{},"status":{"readyReplicas":1}}`,
			wantErr:     "its spec.replicas is not a number",
			wantLast:    "read statefulset/openshell",
		},
		{
			name:        "readyReplicas is not a number",
			statefulSet: `{"spec":{"replicas":1},"status":{"readyReplicas":"1"}}`,
			wantErr:     "its status.readyReplicas is not a number",
			wantLast:    "read statefulset/openshell",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newSbxKube()
			if tc.statefulSet != "" {
				k.statefulSet = tc.statefulSet
			}
			k.fail = tc.fail
			out, err := sbxRun(t, k, sbxPlanFiles(sbxStatefulSetDoc, "3"), sbxDocs(sbxStatefulSetDoc))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runTownClusterApply returned %v, want an error containing %q", err, tc.wantErr)
			}
			labels := k.labels()
			if labels[len(labels)-1] != tc.wantLast {
				t.Errorf("the run's last call was %q, want %q: nothing further may be called", labels[len(labels)-1], tc.wantLast)
			}
			if slices.Contains(labels, "apply w-bootstrap.yaml") {
				t.Errorf("the gt-bootstrap ConfigMap was applied after a StatefulSet that is not ready: %q", labels)
			}
			const undo = "kubectl rollout undo statefulset/openshell -n openshell --to-revision=3"
			if !strings.Contains(out, undo+"\n") {
				t.Errorf("the run printed %q, want the StatefulSet's undo line %q", out, undo)
			}
		})
	}
}

// TestClusterShapeSbxStatefulSetRefused: a gt-sbx plan whose StatefulSet has no
// positive revision, or a name kubectl could not wait for, is refused before the
// first apply, so no object is changed and the StatefulSet's undo line is never
// printed without the revision it names.
func TestClusterShapeSbxStatefulSetRefused(t *testing.T) {
	t.Parallel()
	badNamespace := []byte("apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: openshell\n  namespace: OpenShell\n")
	tests := []struct {
		name     string
		doc      []byte
		revision string
		wantErr  string
	}{
		{name: "no revision", doc: sbxStatefulSetDoc, revision: "", wantErr: "undo command for statefulset/openshell would name none"},
		{name: "revision zero", doc: sbxStatefulSetDoc, revision: "0", wantErr: "undo command for statefulset/openshell would name none"},
		{name: "revision not a number", doc: sbxStatefulSetDoc, revision: "3x", wantErr: "undo command for statefulset/openshell would name none"},
		{name: "namespace kubectl cannot take", doc: badNamespace, revision: "3", wantErr: "is not one kubectl could wait for"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newSbxKube()
			_, err := sbxRun(t, k, sbxPlanFiles(tc.doc, tc.revision), sbxDocs(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runTownClusterApply returned %v, want an error containing %q", err, tc.wantErr)
			}
			for _, label := range k.labels() {
				if strings.HasPrefix(label, "apply ") || strings.HasPrefix(label, "rollout ") {
					t.Errorf("the run called %q, want nothing applied or rolled out", label)
				}
			}
		})
	}
}

// TestClusterShapeSbxUnknownPhase: a gt-sbx file whose phase is neither P nor W is
// refused by the gt-sbx order before any cluster call, rather than dropped.
func TestClusterShapeSbxUnknownPhase(t *testing.T) {
	t.Parallel()
	k := newSbxKube()
	files := []applyPlanFile{{file: planRecordFile{Name: "x.yaml", Phase: "X"}, data: sbxConfigDoc, doc: applyDocument{Kind: "ConfigMap"}}}
	flags := sbxFlags
	flags.cluster = clusterSbx
	_, err := applyPlanFiles(context.Background(), k.run, io.Discard, t.TempDir(), files, flags)
	if err == nil || !strings.Contains(err.Error(), "has no place in the apply") {
		t.Fatalf("applyPlanFiles returned %v, want the unknown phase refused", err)
	}
	if len(k.calls) != 0 {
		t.Errorf("applyPlanFiles made %d cluster calls, want none", len(k.calls))
	}
}
