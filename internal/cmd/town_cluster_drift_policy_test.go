package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestClusterShapeDriftPolicyReportsADifference moves one field of a hand-applied
// admission or RBAC object away from its embedded document, so the run must exit
// 1 and name the object and the path (gt-aar839).
func TestClusterShapeDriftPolicyReportsADifference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resource string
		path     string
		mutate   func(t *testing.T, live map[string]any)
	}{
		{
			name:     "an admission policy whose validations differ",
			resource: "validatingadmissionpolicy/" + updatePolicyName,
			path:     "spec.validations",
			mutate: func(t *testing.T, live map[string]any) {
				t.Helper()
				if live["kind"] != validatingPolicyKind || policyLiveName(live) != updatePolicyName {
					return
				}
				validations, ok := driftObjectAt(live, "spec")["validations"].([]any)
				if !ok || len(validations) == 0 {
					t.Fatalf("the live policy carries no validations to add to")
				}
				driftObjectAt(live, "spec")["validations"] = append(validations,
					map[string]any{"expression": "true", "message": "a validation the file does not name"})
			},
		},
		{
			name:     "a binding that names another policy",
			resource: "validatingadmissionpolicybinding/" + updatePolicyName,
			path:     "spec.policyName",
			mutate: func(t *testing.T, live map[string]any) {
				t.Helper()
				if live["kind"] != validatingPolicyBindKind || policyLiveName(live) != updatePolicyName {
					return
				}
				driftObjectAt(live, "spec")["policyName"] = "gt-update-jobs-old"
			},
		},
		{
			name:     "a Role that loses a resourceName",
			resource: "role/" + updateRoleName,
			path:     "rules[0].resourceNames",
			mutate: func(t *testing.T, live map[string]any) {
				t.Helper()
				if live["kind"] != roleKind || policyLiveName(live) != updateRoleName {
					return
				}
				rule := policyLiveRule(t, live)
				names, ok := rule["resourceNames"].([]any)
				if !ok {
					t.Fatalf("the live Role's first rule names no resourceNames")
				}
				var kept []any
				for _, name := range names {
					if name != "bd-gateway" {
						kept = append(kept, name)
					}
				}
				if len(kept) == len(names) {
					t.Fatalf("the live Role's first rule does not name bd-gateway to remove")
				}
				rule["resourceNames"] = kept
			},
		},
		{
			name:     "a RoleBinding that grants another subject",
			resource: "rolebinding/" + updateRoleName,
			path:     "subjects[0].name",
			mutate: func(t *testing.T, live map[string]any) {
				t.Helper()
				if live["kind"] != roleBindingKind || policyLiveName(live) != updateRoleName {
					return
				}
				subjects, ok := live["subjects"].([]any)
				if !ok || len(subjects) == 0 {
					t.Fatalf("the live RoleBinding binds no subjects")
				}
				subject, ok := subjects[0].(map[string]any)
				if !ok {
					t.Fatalf("the live RoleBinding's first subject is not an object")
				}
				subject["name"] = "another-service-account"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftTestSeams(t, nil)
			d.kubectl = driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), tc.mutate)
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != 1 {
				t.Fatalf("exit = %d, want 1 (err: %v); printed:\n%s", got, err, out.String())
			}
			lines := driftLines(out)
			want := tc.resource + ": " + tc.path + ": rendered "
			if len(lines) != 1 || !strings.HasPrefix(lines[0], want) {
				t.Fatalf("printed\n%q\nwant one line starting %q", lines, want)
			}
			if body := strings.TrimPrefix(lines[0], tc.resource+": "); !driftLineShape.MatchString(body) {
				t.Errorf("line %q is not a path and two fingerprints", lines[0])
			}
		})
	}
}

// TestClusterShapeDriftPolicyHoldsAnObjectInStep drives the compare over the
// hand-applied objects with the live objects in step: it exits 0 and prints
// nothing, so the difference the case above reports is not the compare's default
// (gt-aar839).
func TestClusterShapeDriftPolicyHoldsAnObjectInStep(t *testing.T) {
	t.Parallel()
	d, out, town := driftTestSeams(t, nil)
	var reads []string
	d.kubectl = driftRecordReads(driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), nil), &reads)
	err := runClusterDrift(context.Background(), d)
	if got := driftExit(err); got != 0 {
		t.Fatalf("exit = %d, want 0 (err: %v); printed:\n%s", got, err, out.String())
	}
	if got := driftLines(out); got != nil {
		t.Errorf("printed %q for objects in step, want nothing", got)
	}
	if !slices.Contains(reads, "validatingadmissionpolicy/"+updatePolicyName) {
		t.Errorf("read %q, want the update admission policy among them", reads)
	}
}

// TestClusterShapeDriftPolicyRefusesAnUnreadableObject holds a live admission
// policy and a live Role the cluster does not hold to the unreadable exit status,
// never 0 or 1 (gt-aar839).
func TestClusterShapeDriftPolicyRefusesAnUnreadableObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resource string
	}{
		{name: "an admission policy the cluster does not hold", resource: "validatingadmissionpolicy/" + updatePolicyName},
		{name: "a Role the cluster does not hold", resource: "role/" + updateRoleName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, town := driftTestSeams(t, nil)
			answer := driftAnswering(t, driftTestRendered(t, d, driftTestFlags(t, town)), nil)
			d.kubectl = func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
				if len(args) > 1 && args[1] == tc.resource {
					// kubectl's status for an object the cluster does not hold.
					return "", 1, nil
				}
				return answer(ctx, dir, env, name, args...)
			}
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != driftExitUnreadable {
				t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, driftExitUnreadable, err, out.String())
			}
			if want := "the live " + tc.resource + " could not be read"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if got := driftLines(out); got != nil {
				t.Errorf("printed %q beside the refusal, want nothing", got)
			}
		})
	}
}

// TestClusterShapeDriftPolicyRefusesABrokenEmbed drives every way an embedded
// document can fail the compare: a document with no kind or name, a document
// the spec names and the bytes do not hold, a document held twice, a document
// that is not YAML, a compared field that is missing, a kind the compare does
// not read, and a namespaced object whose namespace is not gt (gt-aar839).
func TestClusterShapeDriftPolicyRefusesABrokenEmbed(t *testing.T) {
	t.Parallel()
	const file = "deploy/k8s/test.yaml"
	spec := func(kind, name string, cluster bool, raw string) []policyTargetSpec {
		return []policyTargetSpec{{file: file, raw: []byte(raw), kind: kind, name: name, cluster: cluster}}
	}
	policy := func(raw string) []policyTargetSpec {
		return spec(validatingPolicyKind, updatePolicyName, true, raw)
	}
	cases := []struct {
		name string
		raw  string
		spec []policyTargetSpec
		want string
	}{
		{
			name: "a document that is not YAML",
			spec: policy("kind: ["),
			want: "reading " + file + " document 1",
		},
		{
			name: "a document with no kind",
			spec: policy("apiVersion: v1\nmetadata:\n  name: gt-update-jobs\nspec: {}\n"),
			want: "document 1 has no kind",
		},
		{
			name: "a document with no metadata.name",
			spec: policy("apiVersion: v1\nkind: ValidatingAdmissionPolicy\nmetadata: {}\nspec: {}\n"),
			want: "has no metadata.name",
		},
		{
			name: "a document the spec names and the bytes do not hold",
			spec: policy("apiVersion: v1\nkind: ValidatingAdmissionPolicy\nmetadata:\n  name: another-policy\nspec: {}\n"),
			want: "holds no ValidatingAdmissionPolicy " + updatePolicyName + " to compare",
		},
		{
			name: "a document held twice",
			spec: policy("apiVersion: v1\nkind: ValidatingAdmissionPolicy\nmetadata:\n  name: gt-update-jobs\nspec: {}\n---\napiVersion: v1\nkind: ValidatingAdmissionPolicy\nmetadata:\n  name: gt-update-jobs\nspec: {}\n"),
			want: "holds 2 ValidatingAdmissionPolicy gt-update-jobs documents, want one",
		},
		{
			name: "a document with no apiVersion",
			spec: policy("kind: ValidatingAdmissionPolicy\nmetadata:\n  name: gt-update-jobs\nspec: {}\n"),
			want: "has no apiVersion",
		},
		{
			name: "a policy with no spec",
			spec: policy("apiVersion: v1\nkind: ValidatingAdmissionPolicy\nmetadata:\n  name: gt-update-jobs\n"),
			want: "has no spec mapping to compare",
		},
		{
			name: "a binding with no spec",
			spec: spec(validatingPolicyBindKind, updatePolicyName, true, "apiVersion: v1\nkind: ValidatingAdmissionPolicyBinding\nmetadata:\n  name: gt-update-jobs\n"),
			want: "has no spec mapping to compare",
		},
		{
			name: "a binding with no policyName",
			spec: spec(validatingPolicyBindKind, updatePolicyName, true, "apiVersion: v1\nkind: ValidatingAdmissionPolicyBinding\nmetadata:\n  name: gt-update-jobs\nspec:\n  validationActions: [Deny]\n"),
			want: "has no spec.policyName to compare",
		},
		{
			name: "a Role with no rules",
			spec: spec(roleKind, updateRoleName, false, "apiVersion: v1\nkind: Role\nmetadata:\n  name: gt-update\n  namespace: gt\n"),
			want: "has no rules to compare",
		},
		{
			name: "a RoleBinding with no roleRef",
			spec: spec(roleBindingKind, updateRoleName, false, "apiVersion: v1\nkind: RoleBinding\nmetadata:\n  name: gt-update\n  namespace: gt\nsubjects: []\n"),
			want: "has no roleRef to compare",
		},
		{
			name: "a Role in another namespace",
			spec: spec(roleKind, updateRoleName, false, "apiVersion: v1\nkind: Role\nmetadata:\n  name: gt-update\n  namespace: other\nrules: []\n"),
			want: `names namespace "other", want "gt"`,
		},
		{
			name: "a Role with no namespace",
			spec: spec(roleKind, updateRoleName, false, "apiVersion: v1\nkind: Role\nmetadata:\n  name: gt-update\nrules: []\n"),
			want: `names namespace "", want "gt"`,
		},
		{
			name: "a kind the compare does not read",
			spec: spec("ServiceAccount", updateRoleName, true, "apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: gt-update\n"),
			want: "is a kind the policy compare does not read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			targets, err := policyDriftTargetsFrom(tc.spec)
			if err == nil {
				t.Fatalf("targets = %d with no error, want the refusal", len(targets))
			}
			if targets != nil {
				t.Errorf("targets = %v beside the error, want none", targets)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
		})
	}
}

// TestClusterShapeDriftPolicyRefusesAnInconsistentScope holds a target whose
// scope and namespace disagree to an error before any cluster read: a
// cluster-scoped object named in a namespace, and a namespaced object with none
// (gt-aar839).
func TestClusterShapeDriftPolicyRefusesAnInconsistentScope(t *testing.T) {
	t.Parallel()
	object := map[string]any{
		"apiVersion": "v1",
		"kind":       roleKind,
		"metadata":   map[string]any{"name": updateRoleName},
	}
	cases := []struct {
		name   string
		target driftTarget
		want   string
	}{
		{
			name:   "a cluster-scoped object named in a namespace",
			target: driftTarget{kind: validatingPolicyKind, resource: "validatingadmissionpolicy/" + updatePolicyName, namespace: updateRBACNamespace, cluster: true, object: object},
			want:   "named in namespace",
		},
		{
			name:   "a namespaced object with no namespace",
			target: driftTarget{kind: roleKind, resource: "role/" + updateRoleName, object: object},
			want:   "names no namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _ := driftTestSeams(t, nil)
			d.kubectl = driftNoRead(t)
			if _, err := tc.target.differences(context.Background(), d); err == nil {
				t.Fatalf("the target was read, want the refusal")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
		})
	}
}

// TestClusterShapeDriftPolicyRefusesAnUnreadableEmbed holds a builder that cannot
// read the embedded documents, and a deps value that never got the builder, to
// the unreadable exit status before any cluster read: a run that cannot read the
// documents must not report a cluster in step (gt-aar839).
func TestClusterShapeDriftPolicyRefusesAnUnreadableEmbed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		adjust func(*clusterDriftDeps)
		want   string
	}{
		{
			name: "the builder cannot read the documents",
			adjust: func(d *clusterDriftDeps) {
				d.policyTargets = func() ([]driftTarget, error) {
					return nil, errors.New("the embed is gone")
				}
			},
			want: "cannot read the embedded admission and RBAC documents to compare",
		},
		{
			name: "no builder is wired",
			adjust: func(d *clusterDriftDeps) {
				d.policyTargets = nil
			},
			want: "no embedded admission and RBAC targets are wired",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, out, _ := driftTestSeams(t, nil)
			d.kubectl = driftNoRead(t)
			tc.adjust(&d)
			err := runClusterDrift(context.Background(), d)
			if got := driftExit(err); got != driftExitUnreadable {
				t.Fatalf("exit = %d, want %d (err: %v); printed:\n%s", got, driftExitUnreadable, err, out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
			if got := driftLines(out); got != nil {
				t.Errorf("printed %q beside the refusal, want nothing", got)
			}
		})
	}
}

// policyLiveName is the live object's metadata.name.
func policyLiveName(live map[string]any) string {
	metadata, ok := live["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := metadata["name"].(string)
	return name
}

// policyLiveRule is the live Role's first rule.
func policyLiveRule(t *testing.T, live map[string]any) map[string]any {
	t.Helper()
	rules, ok := live["rules"].([]any)
	if !ok || len(rules) == 0 {
		t.Fatalf("the live Role carries no rules")
	}
	rule, ok := rules[0].(map[string]any)
	if !ok {
		t.Fatalf("the live Role's first rule is not an object")
	}
	return rule
}
