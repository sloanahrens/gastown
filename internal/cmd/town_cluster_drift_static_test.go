package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/deploy/k8s"
	"github.com/steveyegge/gastown/internal/update"
)

// TestTownClusterDriftStaticBuildsTheRBACTargets holds a ClusterRole and a
// namespaced RoleBinding from the embedded set to the targets they build, named
// by kind and name, and a policy row the same set holds to no target at all
// (gt-cd1iso).
func TestTownClusterDriftStaticBuildsTheRBACTargets(t *testing.T) {
	t.Parallel()
	objects := driftStaticObjects(t,
		"ClusterRole/gt-drift-reader",
		"RoleBinding/gt-sweeps/sweep-runner",
		"ValidatingAdmissionPolicy/gt-update-jobs",
	)
	targets, err := staticObjectTargets(objects, driftStaticCovered)
	if err != nil {
		t.Fatalf("staticObjectTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("built %d targets, want the ClusterRole and the RoleBinding and none for the covered policy", len(targets))
	}
	want := []driftTarget{
		{kind: "ClusterRole", resource: "clusterrole/gt-drift-reader", cluster: true},
		{kind: "RoleBinding", resource: "rolebinding/sweep-runner", namespace: "gt-sweeps"},
	}
	for i, w := range want {
		got := targets[i]
		if got.kind != w.kind || got.resource != w.resource || got.namespace != w.namespace || got.cluster != w.cluster {
			t.Errorf("target %d = kind %q, resource %q, namespace %q, cluster %v; want %q, %q, %q, %v",
				i, got.kind, got.resource, got.namespace, got.cluster, w.kind, w.resource, w.namespace, w.cluster)
		}
	}
}

// TestTownClusterDriftStaticReportsAnRBACDifference drives each RBAC target's
// read against a live object one field from its document, so the read reports
// the path (gt-cd1iso).
func TestTownClusterDriftStaticReportsAnRBACDifference(t *testing.T) {
	t.Parallel()
	objects := driftStaticObjects(t, "ClusterRole/gt-drift-reader", "RoleBinding/gt-sweeps/sweep-runner")
	targets, err := staticObjectTargets(objects, driftStaticCovered)
	if err != nil {
		t.Fatalf("staticObjectTargets: %v", err)
	}
	d, _, _ := driftTestSeams(t, nil)
	reader, _ := driftStaticReader(t, targets, func(t *testing.T, target driftTarget, live map[string]any) {
		t.Helper()
		switch target.kind {
		case "ClusterRole":
			rules, ok := live["rules"].([]any)
			if !ok || len(rules) == 0 {
				t.Fatalf("the live ClusterRole carries no rules")
			}
			rule, ok := rules[0].(map[string]any)
			if !ok {
				t.Fatalf("the live ClusterRole's first rule is not an object")
			}
			rule["verbs"] = []any{"get"}
		case "RoleBinding":
			subjects, ok := live["subjects"].([]any)
			if !ok || len(subjects) == 0 {
				t.Fatalf("the live RoleBinding binds no subjects")
			}
			subject, ok := subjects[0].(map[string]any)
			if !ok {
				t.Fatalf("the live RoleBinding's first subject is not an object")
			}
			subject["name"] = "another-service-account"
		}
	})
	d.kubectl = reader
	cases := []struct {
		resource string
		path     string
	}{
		{resource: "clusterrole/gt-drift-reader", path: "rules[0].verbs"},
		{resource: "rolebinding/sweep-runner", path: "subjects[0].name"},
	}
	for _, tc := range cases {
		target := driftStaticTarget(t, targets, tc.resource)
		lines, err := target.differences(context.Background(), d)
		if err != nil {
			t.Fatalf("%s: %v", tc.resource, err)
		}
		if len(lines) != 1 || !strings.HasPrefix(lines[0], tc.path+": rendered ") {
			t.Errorf("%s printed %q, want one line at %s", tc.resource, lines, tc.path)
		}
	}
}

// TestTownClusterDriftStaticBuildsThePriorityClassAndNamespaceTargets holds a
// cluster-scoped PriorityClass and Namespace to the targets they build, named by
// kind and name (gt-cd1iso).
func TestTownClusterDriftStaticBuildsThePriorityClassAndNamespaceTargets(t *testing.T) {
	t.Parallel()
	objects := append(driftStaticObjects(t, "PriorityClass/gt-control-plane"), driftStaticNamespaceFixture())
	targets, err := staticObjectTargets(objects, driftStaticCovered)
	if err != nil {
		t.Fatalf("staticObjectTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("built %d targets, want the PriorityClass and the Namespace", len(targets))
	}
	want := []driftTarget{
		{kind: "PriorityClass", resource: "priorityclass/gt-control-plane", cluster: true},
		{kind: "Namespace", resource: "namespace/gt-fixture", cluster: true},
	}
	for i, w := range want {
		got := targets[i]
		if got.kind != w.kind || got.resource != w.resource || got.namespace != w.namespace || got.cluster != w.cluster {
			t.Errorf("target %d = kind %q, resource %q, namespace %q, cluster %v; want %q, %q, %q, %v",
				i, got.kind, got.resource, got.namespace, got.cluster, w.kind, w.resource, w.namespace, w.cluster)
		}
	}
}

// TestTownClusterDriftStaticReportsAPriorityClassAndNamespaceDifference drives a
// PriorityClass's value and a Namespace's named label one field from their
// documents, so each read reports the path (gt-cd1iso).
func TestTownClusterDriftStaticReportsAPriorityClassAndNamespaceDifference(t *testing.T) {
	t.Parallel()
	objects := append(driftStaticObjects(t, "PriorityClass/gt-control-plane"), driftStaticNamespaceFixture())
	targets, err := staticObjectTargets(objects, driftStaticCovered)
	if err != nil {
		t.Fatalf("staticObjectTargets: %v", err)
	}
	d, _, _ := driftTestSeams(t, nil)
	reader, _ := driftStaticReader(t, targets, func(t *testing.T, target driftTarget, live map[string]any) {
		t.Helper()
		switch target.kind {
		case "PriorityClass":
			live["value"] = float64(1)
		case "Namespace":
			driftObjectAt(live, "metadata", "labels")["reviewed"] = "false"
		}
	})
	d.kubectl = reader
	cases := []struct {
		resource string
		path     string
	}{
		{resource: "priorityclass/gt-control-plane", path: "value"},
		{resource: "namespace/gt-fixture", path: "metadata.labels.reviewed"},
	}
	for _, tc := range cases {
		target := driftStaticTarget(t, targets, tc.resource)
		lines, err := target.differences(context.Background(), d)
		if err != nil {
			t.Fatalf("%s: %v", tc.resource, err)
		}
		if len(lines) != 1 || !strings.HasPrefix(lines[0], tc.path+": rendered ") {
			t.Errorf("%s printed %q, want one line at %s", tc.resource, lines, tc.path)
		}
	}
}

// TestTownClusterDriftStaticRefusesASecret holds the set's token Secret to an
// error naming its kind and the object, since 5a reads no Secret (gt-cd1iso).
func TestTownClusterDriftStaticRefusesASecret(t *testing.T) {
	t.Parallel()
	objects := driftStaticObjects(t, "Secret/gt-sweeps/sweep-runner-token")
	targets, err := staticObjectTargets(objects, driftStaticCovered)
	if err == nil {
		t.Fatalf("built %d targets for a Secret, want the refusal", len(targets))
	}
	if targets != nil {
		t.Errorf("built %d targets beside the error, want none", len(targets))
	}
	for _, want := range []string{"Secret", "gt-sweeps/sweep-runner-token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
}

// TestTownClusterDriftStaticRefusesNoCoveredReport holds the nil cover report to
// an error rather than a call through it, since a run that cannot tell the
// policy rows' objects from the rest compares every one twice (gt-cd1iso).
func TestTownClusterDriftStaticRefusesNoCoveredReport(t *testing.T) {
	t.Parallel()
	targets, err := staticObjectTargets(driftStaticObjects(t, "PriorityClass/gt-control-plane"), nil)
	if err == nil {
		t.Fatalf("built %d targets with no cover report, want the refusal", len(targets))
	}
	if targets != nil {
		t.Errorf("built %d targets beside the error, want none", len(targets))
	}
	if want := "policy rows hold"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
}

// TestTownClusterDriftStaticRefusesAnObjectTwice holds an input that names the
// same object twice to an error: two targets would read and print it twice
// (gt-cd1iso).
func TestTownClusterDriftStaticRefusesAnObjectTwice(t *testing.T) {
	t.Parallel()
	one := driftStaticObject(roleKind, "gt-mac-watch", "gt", false, "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: gt-mac-watch\n  namespace: gt\nrules: []\n")
	targets, err := staticObjectTargets([]k8s.StaticObject{one, one}, driftStaticCovered)
	if err == nil {
		t.Fatalf("built %d targets for one object named twice, want the refusal", len(targets))
	}
	if targets != nil {
		t.Errorf("built %d targets beside the error, want none", len(targets))
	}
	if want := "compared twice"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
}

// TestTownClusterDriftStaticKeepsSameNamesInDifferentNamespaces holds a Role of
// one name in two namespaces to two targets: the namespace is part of the
// object's identity, not a duplicate (gt-cd1iso).
func TestTownClusterDriftStaticKeepsSameNamesInDifferentNamespaces(t *testing.T) {
	t.Parallel()
	objects := []k8s.StaticObject{
		driftStaticObject(roleKind, "gt-mac-watch", "gt", false, "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: gt-mac-watch\n  namespace: gt\nrules: []\n"),
		driftStaticObject(roleKind, "gt-mac-watch", "forgejo", false, "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: gt-mac-watch\n  namespace: forgejo\nrules: []\n"),
	}
	targets, err := staticObjectTargets(objects, driftStaticCovered)
	if err != nil {
		t.Fatalf("staticObjectTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("built %d targets for one Role name in two namespaces, want 2", len(targets))
	}
}

// TestTownClusterDriftStaticHasNoDuplicateTargets walks the embedded set and
// holds the result to one target per object and none for a policy row
// (gt-cd1iso).
func TestTownClusterDriftStaticHasNoDuplicateTargets(t *testing.T) {
	t.Parallel()
	objects, want := driftStaticObjectsWithoutSecrets(t)
	targets, err := staticObjectTargets(objects, driftStaticCovered)
	if err != nil {
		t.Fatalf("staticObjectTargets: %v", err)
	}
	if len(targets) != want {
		t.Errorf("built %d targets, want one for each of the %d static objects the policy rows do not hold", len(targets), want)
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		key := target.resource + " " + target.namespace
		if seen[key] {
			t.Errorf("%s in %q is built twice", target.resource, target.namespace)
		}
		seen[key] = true
		if _, known := staticTargetKinds[target.kind]; !known {
			t.Errorf("%s is a kind the compare does not read", target.resource)
		}
	}
}

// TestTownClusterDriftStaticReadsOnlyGets drives every target of the embedded
// set against its live object in step: no difference, and every cluster call a
// get (gt-cd1iso).
func TestTownClusterDriftStaticReadsOnlyGets(t *testing.T) {
	t.Parallel()
	objects, _ := driftStaticObjectsWithoutSecrets(t)
	targets, err := staticObjectTargets(objects, driftStaticCovered)
	if err != nil {
		t.Fatalf("staticObjectTargets: %v", err)
	}
	d, _, _ := driftTestSeams(t, nil)
	reader, reads := driftStaticReader(t, targets, nil)
	d.kubectl = reader
	for _, target := range targets {
		lines, err := target.differences(context.Background(), d)
		if err != nil {
			t.Fatalf("%s: %v", target.resource, err)
		}
		if lines != nil {
			t.Errorf("%s printed %q with the live object in step", target.resource, lines)
		}
	}
	if len(*reads) != len(targets) {
		t.Errorf("made %d cluster calls for %d targets, want one each", len(*reads), len(targets))
	}
	for _, read := range *reads {
		if !strings.HasPrefix(read, "get ") {
			t.Errorf("ran kubectl %q, want a get", read)
		}
	}
}

// TestTownClusterDriftStaticRefusesABrokenObject drives every way a static
// object's document can fail the build: a document that is not YAML, none, more
// than one, one naming another object, a namespace the object does not name, an
// object whose marked scope and kind disagree, a kind the compare does not read,
// and a compared field the document lacks (gt-cd1iso).
func TestTownClusterDriftStaticRefusesABrokenObject(t *testing.T) {
	t.Parallel()
	role := "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: gt-mac-watch\n  namespace: gt\nrules: []\n"
	cases := []struct {
		name   string
		object k8s.StaticObject
		want   string
	}{
		{
			name:   "a document that is not YAML",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", false, "kind: [\n"),
			want:   "decoding the static Role gt/gt-mac-watch",
		},
		{
			name:   "no document",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", false, ""),
			want:   "holds no document to compare",
		},
		{
			name:   "an empty document",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", false, "{}"),
			want:   "holds no document to compare",
		},
		{
			name:   "a broken second document",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", false, role+"---\nkind: [\n"),
			want:   "decoding the static Role gt/gt-mac-watch",
		},
		{
			name:   "more than one document",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", false, role+"---\n"+role),
			want:   "holds more than one document",
		},
		{
			name:   "a document naming another kind",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", false, strings.Replace(role, "kind: Role", "kind: RoleBinding", 1)),
			want:   `holds a "RoleBinding" document`,
		},
		{
			name:   "a document naming another object",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", false, strings.Replace(role, "name: gt-mac-watch", "name: another", 1)),
			want:   `holds a document named "another"`,
		},
		{
			name:   "a document naming another namespace",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", false, strings.Replace(role, "namespace: gt", "namespace: other", 1)),
			want:   `names namespace "other", want "gt"`,
		},
		{
			name:   "a cluster-scoped kind marked namespaced",
			object: driftStaticObject(namespaceKind, "gt-sweeps", "", false, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: gt-sweeps\n"),
			want:   "marked namespaced, and its kind is cluster-scoped",
		},
		{
			name:   "a namespaced kind marked cluster-scoped",
			object: driftStaticObject(roleKind, "gt-mac-watch", "gt", true, role),
			want:   "marked cluster-scoped, and its kind is namespaced",
		},
		{
			name:   "a cluster-scoped kind named in a namespace",
			object: driftStaticObject(namespaceKind, "gt-sweeps", "gt", true, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: gt-sweeps\n"),
			want:   "is named in namespace",
		},
		{
			name:   "a namespaced kind with no namespace",
			object: driftStaticObject(roleKind, "gt-mac-watch", "", false, role),
			want:   "names no namespace",
		},
		{
			name:   "a kind the compare does not read",
			object: driftStaticObject("ConfigMap", "gt-config", "gt", false, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: gt-config\n  namespace: gt\n"),
			want:   "is a kind the drift compare does not read",
		},
		{
			name:   "a compared field the document lacks",
			object: driftStaticObject(priorityClassKind, "gt-ci", "", true, "apiVersion: scheduling.k8s.io/v1\nkind: PriorityClass\nmetadata:\n  name: gt-ci\nglobalDefault: false\ndescription: ci\n"),
			want:   "has no value to compare",
		},
		{
			name:   "a document with no apiVersion",
			object: driftStaticObject(namespaceKind, "gt-sweeps", "", true, "kind: Namespace\nmetadata:\n  name: gt-sweeps\n"),
			want:   "has no apiVersion",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			targets, err := staticObjectTargets([]k8s.StaticObject{tc.object}, driftStaticCovered)
			if err == nil {
				t.Fatalf("built %d targets, want the refusal", len(targets))
			}
			if targets != nil {
				t.Errorf("built %d targets beside the error, want none", len(targets))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
		})
	}
}

// driftStaticCovered reports the static objects policyTargetSpecs holds, the
// objects the compare reads by their embedded files.
func driftStaticCovered(kind, name, namespace string) bool {
	for _, spec := range policyTargetSpecs {
		if spec.kind != kind || spec.name != name {
			continue
		}
		if spec.cluster {
			return namespace == ""
		}
		return namespace == updateRBACNamespace
	}
	return false
}

// driftStaticObjects is the embedded set's objects the keys name, each
// "Kind/name" or "Kind/namespace/name".
func driftStaticObjects(t *testing.T, keys ...string) []k8s.StaticObject {
	t.Helper()
	all, err := k8s.Static()
	if err != nil {
		t.Fatalf("reading the embedded static set: %v", err)
	}
	objects := make([]k8s.StaticObject, 0, len(keys))
	for _, key := range keys {
		parts := strings.Split(key, "/")
		if len(parts) < 2 || len(parts) > 3 {
			t.Fatalf("key %q is not Kind/name or Kind/namespace/name", key)
		}
		name := parts[len(parts)-1]
		namespace := ""
		if len(parts) == 3 {
			namespace = parts[1]
		}
		var found []k8s.StaticObject
		for _, object := range all {
			if object.Kind == parts[0] && object.Name == name && object.Namespace == namespace {
				found = append(found, object)
			}
		}
		if len(found) != 1 {
			t.Fatalf("the static set holds %d objects named %s, want one", len(found), key)
		}
		objects = append(objects, found[0])
	}
	return objects
}

// driftStaticObjectsWithoutSecrets is the embedded set's objects the build
// reads, and how many of them the policy rows do not hold. The Secrets are left
// out because 5a refuses one and the not-compared line is 5b's (gt-w6g6ok).
func driftStaticObjectsWithoutSecrets(t *testing.T) ([]k8s.StaticObject, int) {
	t.Helper()
	all, err := k8s.Static()
	if err != nil {
		t.Fatalf("reading the embedded static set: %v", err)
	}
	objects := make([]k8s.StaticObject, 0, len(all))
	want := 0
	for _, object := range all {
		if object.Kind == "Secret" {
			continue
		}
		objects = append(objects, object)
		if !driftStaticCovered(object.Kind, object.Name, object.Namespace) {
			want++
		}
	}
	return objects, want
}

// driftStaticObject is a static object a test builds, with the document it must
// agree with.
func driftStaticObject(kind, name, namespace string, cluster bool, doc string) k8s.StaticObject {
	return k8s.StaticObject{Kind: kind, Name: name, Namespace: namespace, Cluster: cluster, Doc: []byte(doc)}
}

// driftStaticNamespaceFixture is a Namespace whose document names a label, which
// the embedded gt-sweeps document does not, so a live label it loses is a
// compared field.
func driftStaticNamespaceFixture() k8s.StaticObject {
	return driftStaticObject(namespaceKind, "gt-fixture", "", true, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: gt-fixture\n  labels:\n    reviewed: \"true\"\n")
}

// driftStaticTarget is the target the resource names, and fails the test when
// there is none.
func driftStaticTarget(t *testing.T, targets []driftTarget, resource string) driftTarget {
	t.Helper()
	for _, target := range targets {
		if target.resource == resource {
			return target
		}
	}
	t.Fatalf("no target named %s among %d", resource, len(targets))
	return driftTarget{}
}

// driftStaticReader answers every read from the target's own object, mutated by
// mutate, and records each call as the verb and the resource it read.
func driftStaticReader(t *testing.T, targets []driftTarget, mutate func(t *testing.T, target driftTarget, live map[string]any)) (update.RunFunc, *[]string) {
	t.Helper()
	byResource := make(map[string]driftTarget, len(targets))
	for _, target := range targets {
		byResource[target.resource] = target
	}
	var reads []string
	answer := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
		if name != "kubectl" {
			t.Fatalf("ran %q, want kubectl", name)
		}
		if len(args) < 2 || args[len(args)-1] != "json" {
			t.Fatalf("ran kubectl %q, want a get with -o json", args)
		}
		reads = append(reads, args[0]+" "+args[1])
		target, ok := byResource[args[1]]
		if !ok {
			t.Fatalf("kubectl read %q, which no target names", args[1])
		}
		return driftLiveAnswer(t, target.object, func(t *testing.T, live map[string]any) {
			if mutate != nil {
				mutate(t, target, live)
			}
		}), 0, nil
	}
	return answer, &reads
}
