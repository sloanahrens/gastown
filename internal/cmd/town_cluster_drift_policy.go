package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// The static manifests' files, so a refusal names the path a reader resolves from
// the repo root.
const (
	updateAdmissionFile      = "deploy/k8s/update-admission.yaml"
	gatewayAdmissionFile     = "deploy/k8s/bd-gateway-service-admission.yaml"
	updateRBACFile           = "deploy/k8s/update-rbac.yaml"
	updatePolicyName         = "gt-update-jobs"
	gatewayPolicyName        = "gt-bd-gateway-service"
	updateRoleName           = "gt-update"
	updateLaunchRoleName     = "gt-update-launch"
	validatingPolicyKind     = "ValidatingAdmissionPolicy"
	validatingPolicyBindKind = "ValidatingAdmissionPolicyBinding"
	roleKind                 = "Role"
	roleBindingKind          = "RoleBinding"
	// updateRBACNamespace is the namespace the update Job's Roles and
	// RoleBindings live in. The reads take it from here, never from the file, so
	// an edited namespace cannot reach kubectl as a flag.
	updateRBACNamespace = "gt"
)

// policyTargetSpec names one embedded document a drift run compares: the file it
// is embedded from, the object's kind and name, and whether the live object is
// cluster-scoped. The scope is written out rather than derived from the kind, so
// a read never takes its namespace from a value the file could get wrong.
type policyTargetSpec struct {
	file    string
	raw     []byte
	kind    string
	name    string
	cluster bool
}

// policyTargetSpecs is every static object the drift compare holds to the
// embedded files: the two admission policies and their bindings, and the update
// Job's Roles and RoleBindings in gt (gt-aar839). A ServiceAccount in the same
// file is not compared, since a live one carries fields the file does not name.
var policyTargetSpecs = []policyTargetSpec{
	{file: updateAdmissionFile, raw: k8s.UpdateAdmissionYAML, kind: validatingPolicyKind, name: updatePolicyName, cluster: true},
	{file: updateAdmissionFile, raw: k8s.UpdateAdmissionYAML, kind: validatingPolicyBindKind, name: updatePolicyName, cluster: true},
	{file: gatewayAdmissionFile, raw: k8s.BdGatewayServiceAdmissionYAML, kind: validatingPolicyKind, name: gatewayPolicyName, cluster: true},
	{file: gatewayAdmissionFile, raw: k8s.BdGatewayServiceAdmissionYAML, kind: validatingPolicyBindKind, name: gatewayPolicyName, cluster: true},
	{file: updateRBACFile, raw: k8s.UpdateRBACYAML, kind: roleKind, name: updateRoleName},
	{file: updateRBACFile, raw: k8s.UpdateRBACYAML, kind: roleBindingKind, name: updateRoleName},
	{file: updateRBACFile, raw: k8s.UpdateRBACYAML, kind: roleKind, name: updateLaunchRoleName},
	{file: updateRBACFile, raw: k8s.UpdateRBACYAML, kind: roleBindingKind, name: updateLaunchRoleName},
}

// policyDriftTargets is the drift targets of the objects deploy/k8s applies by
// hand.
func policyDriftTargets() ([]driftTarget, error) {
	return policyDriftTargetsFrom(policyTargetSpecs)
}

// policyDriftTargetsFrom reads each spec's embedded bytes, reduces the document
// it names to the fields the compare reads, and pairs it with the live object it
// must equal. A spec whose document the bytes do not hold, or hold twice, is an
// error rather than a target the compare skips.
func policyDriftTargetsFrom(specs []policyTargetSpec) ([]driftTarget, error) {
	targets := make([]driftTarget, 0, len(specs))
	for _, spec := range specs {
		docs, err := parsePolicyFile(spec.file, spec.raw)
		if err != nil {
			return nil, err
		}
		doc, err := findPolicyDoc(docs, spec.file, spec.kind, spec.name)
		if err != nil {
			return nil, err
		}
		object, err := policyComparedObject(doc)
		if err != nil {
			return nil, err
		}
		if spec.cluster {
			targets = append(targets, clusterScopedDriftTarget(spec.kind, spec.name, object))
			continue
		}
		// The read's namespace is the constant, and the file must agree: a
		// document that names another namespace, or none, is a broken input
		// rather than a cluster in step.
		if doc.namespace != updateRBACNamespace {
			return nil, fmt.Errorf("%s %s %s names namespace %q, want %q", doc.file, doc.kind, doc.name, doc.namespace, updateRBACNamespace)
		}
		targets = append(targets, namespacedDriftTarget(spec.kind, spec.name, updateRBACNamespace, object))
	}
	return targets, nil
}

// clusterScopedDriftTarget is the drift target of a cluster-scoped object: the
// resource kubectl reads it by, with no namespace.
func clusterScopedDriftTarget(kind, name string, object any) driftTarget {
	return driftTarget{kind: kind, resource: strings.ToLower(kind) + "/" + name, object: object, cluster: true}
}

// staticPolicyDoc is one document of an embedded manifest: the file it came
// from, its decoded body, and the identifying fields the target list names it by.
type staticPolicyDoc struct {
	file      string
	body      map[string]any
	kind      string
	name      string
	namespace string
}

// parsePolicyFile decodes every document of one embedded file. A document that
// is not an object, or carries no kind or metadata.name, is an error: the embed
// is part of the build, so a document the compare cannot name is a broken build
// input, not a cluster in step.
func parsePolicyFile(file string, raw []byte) ([]staticPolicyDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []staticPolicyDoc
	for index := 1; ; index++ {
		var body map[string]any
		err := dec.Decode(&body)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s document %d: %w", file, index, err)
		}
		if len(body) == 0 {
			continue
		}
		kind, _ := body["kind"].(string)
		metadata, _ := body["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)
		switch {
		case kind == "":
			return nil, fmt.Errorf("%s document %d has no kind", file, index)
		case name == "":
			return nil, fmt.Errorf("%s document %d (%s) has no metadata.name", file, index, kind)
		}
		docs = append(docs, staticPolicyDoc{file: file, body: body, kind: kind, name: name, namespace: namespace})
	}
}

// findPolicyDoc returns the one document of the kind and name. None is an error,
// because a document the target list names and the embed lacks would otherwise
// be skipped as a cluster in step; more than one is refused, since the compare
// would read whichever it found last.
func findPolicyDoc(docs []staticPolicyDoc, file, kind, name string) (staticPolicyDoc, error) {
	var found []staticPolicyDoc
	for _, doc := range docs {
		if doc.kind == kind && doc.name == name {
			found = append(found, doc)
		}
	}
	switch len(found) {
	case 0:
		return staticPolicyDoc{}, fmt.Errorf("%s holds no %s %s to compare", file, kind, name)
	case 1:
		return found[0], nil
	default:
		return staticPolicyDoc{}, fmt.Errorf("%s holds %d %s %s documents, want one", file, len(found), kind, name)
	}
}

// policyComparedObject is the document reduced to what the compare reads, with
// the object's identity kept so the live answer is matched to it:
//   - a ValidatingAdmissionPolicy's spec,
//   - a ValidatingAdmissionPolicyBinding's policyName and validationActions,
//   - a Role's and a ClusterRole's rules,
//   - a RoleBinding's and a ClusterRoleBinding's roleRef and subjects,
//   - a PriorityClass's value, globalDefault and description,
//   - a Namespace's and a ServiceAccount's identity, since their documents name
//     nothing else the compare reads,
//   - the metadata labels and annotations the document names, for every kind,
//     since a label can steer admission or selection and one only the live
//     object carries is not drift (gt-cd1iso).
//
// A field the compare names and the document lacks is an error: a missing field
// would otherwise read as nothing to compare.
func policyComparedObject(doc staticPolicyDoc) (map[string]any, error) {
	apiVersion, _ := doc.body["apiVersion"].(string)
	if apiVersion == "" {
		return nil, fmt.Errorf("%s %s %s has no apiVersion", doc.file, doc.kind, doc.name)
	}
	metadata := map[string]any{"name": doc.name}
	if doc.namespace != "" {
		metadata["namespace"] = doc.namespace
	}
	if named, ok := doc.body["metadata"].(map[string]any); ok {
		for _, key := range []string{"labels", "annotations"} {
			if value, present := named[key]; present {
				metadata[key] = value
			}
		}
	}
	object := map[string]any{"apiVersion": apiVersion, "kind": doc.kind, "metadata": metadata}
	switch doc.kind {
	case validatingPolicyKind:
		spec, err := policyMapField(doc, "spec")
		if err != nil {
			return nil, err
		}
		object["spec"] = spec
	case validatingPolicyBindKind:
		spec, err := policyMapField(doc, "spec")
		if err != nil {
			return nil, err
		}
		selected := map[string]any{}
		for _, key := range []string{"policyName", "validationActions"} {
			value, ok := spec[key]
			if !ok {
				return nil, fmt.Errorf("%s %s %s has no spec.%s to compare", doc.file, doc.kind, doc.name, key)
			}
			selected[key] = value
		}
		object["spec"] = selected
	case roleKind, clusterRoleKind:
		rules, ok := doc.body["rules"]
		if !ok {
			return nil, fmt.Errorf("%s %s %s has no rules to compare", doc.file, doc.kind, doc.name)
		}
		object["rules"] = rules
	case roleBindingKind, clusterRoleBindingKind:
		for _, key := range []string{"roleRef", "subjects"} {
			value, ok := doc.body[key]
			if !ok {
				return nil, fmt.Errorf("%s %s %s has no %s to compare", doc.file, doc.kind, doc.name, key)
			}
			object[key] = value
		}
	case priorityClassKind:
		for _, key := range []string{"value", "globalDefault", "description"} {
			value, ok := doc.body[key]
			if !ok {
				return nil, fmt.Errorf("%s %s %s has no %s to compare", doc.file, doc.kind, doc.name, key)
			}
			object[key] = value
		}
	case namespaceKind, serviceAccountKind:
		// The identity and named metadata above are all these documents state
		// the compare reads; the server fills in the rest, such as a Namespace's
		// spec.finalizers and a ServiceAccount's token fields.
	default:
		return nil, fmt.Errorf("%s %s %s is a kind the policy compare does not read", doc.file, doc.kind, doc.name)
	}
	return object, nil
}

// policyMapField returns the document's field as an object, refusing a missing
// one or one that is not a mapping.
func policyMapField(doc staticPolicyDoc, key string) (map[string]any, error) {
	field, ok := doc.body[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s %s %s has no %s mapping to compare", doc.file, doc.kind, doc.name, key)
	}
	return field, nil
}
