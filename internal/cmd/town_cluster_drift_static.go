package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// The static set's kinds beside the admission and RBAC kinds the policy rows
// hold (town_cluster_drift_policy.go).
const (
	clusterRoleKind        = "ClusterRole"
	clusterRoleBindingKind = "ClusterRoleBinding"
	priorityClassKind      = "PriorityClass"
	namespaceKind          = "Namespace"
	serviceAccountKind     = "ServiceAccount"
)

// staticSource names the embedded set in a refusal, where a policy row names the
// file its document came from.
const staticSource = "the embedded static set"

// staticTargetKinds is every kind a static object is compared by, and whether
// its live object is cluster-scoped. The scope comes from the kind rather than
// from the object, since only the kind decides where kubectl reads: a namespaced
// read lands in the namespace it is given, and a cluster-scoped one in whatever
// namespace kubectl's context holds.
var staticTargetKinds = map[string]bool{
	validatingPolicyKind:     true,
	validatingPolicyBindKind: true,
	clusterRoleKind:          true,
	clusterRoleBindingKind:   true,
	priorityClassKind:        true,
	namespaceKind:            true,
	serviceAccountKind:       false,
	roleKind:                 false,
	roleBindingKind:          false,
}

// staticObjectTargets is the drift targets of the static objects the policy rows
// do not hold: each object covered does not claim, reduced as the policy rows'
// documents are and read by its kind's scope. covered reports the objects
// policyTargetSpecs holds, since the compare reads those by their files
// (gt-cd1iso).
func staticObjectTargets(objects []k8s.StaticObject, covered func(kind, name, namespace string) bool) ([]driftTarget, error) {
	if covered == nil {
		return nil, errors.New("the static objects need a report of the objects the policy rows hold, so none were compared")
	}
	targets := make([]driftTarget, 0, len(objects))
	seen := make(map[string]bool, len(objects))
	for _, object := range objects {
		if covered(object.Kind, object.Name, object.Namespace) {
			continue
		}
		target, err := staticObjectTarget(object)
		if err != nil {
			return nil, err
		}
		key := target.resource + " " + target.namespace
		if seen[key] {
			return nil, fmt.Errorf("the static %s is compared twice, so the set holds it twice", staticObjectLabel(object))
		}
		seen[key] = true
		targets = append(targets, target)
	}
	return targets, nil
}

// staticObjectTarget is the drift target of one static object: its document
// reduced to the fields the compare reads, read by the scope its kind has and
// the namespace the object names.
func staticObjectTarget(object k8s.StaticObject) (driftTarget, error) {
	cluster, known := staticTargetKinds[object.Kind]
	if !known {
		return driftTarget{}, fmt.Errorf("the static %s is a kind the drift compare does not read", staticObjectLabel(object))
	}
	if cluster != object.Cluster {
		return driftTarget{}, fmt.Errorf("the static %s is marked %s, and its kind is %s, so it was not read", staticObjectLabel(object), scopeWord(object.Cluster), scopeWord(cluster))
	}
	switch {
	case cluster && object.Namespace != "":
		return driftTarget{}, fmt.Errorf("the cluster-scoped static %s is named in namespace %q, so it was not read", staticObjectLabel(object), object.Namespace)
	case !cluster && object.Namespace == "":
		return driftTarget{}, fmt.Errorf("the namespaced static %s names no namespace, so it was not read", staticObjectLabel(object))
	}
	doc, err := staticObjectDoc(object)
	if err != nil {
		return driftTarget{}, err
	}
	compared, err := policyComparedObject(doc)
	if err != nil {
		return driftTarget{}, err
	}
	if cluster {
		return clusterScopedDriftTarget(object.Kind, object.Name, compared), nil
	}
	return namespacedDriftTarget(object.Kind, object.Name, object.Namespace, compared), nil
}

// staticObjectDoc decodes an object's document into the document the policy
// rows' reduction reads, and refuses one that names another object than the one
// it came with: the compare would otherwise report one object's fields under
// another object's name.
func staticObjectDoc(object k8s.StaticObject) (staticPolicyDoc, error) {
	label := staticObjectLabel(object)
	dec := yaml.NewDecoder(bytes.NewReader(object.Doc))
	var body map[string]any
	switch err := dec.Decode(&body); {
	case errors.Is(err, io.EOF):
		return staticPolicyDoc{}, fmt.Errorf("the static %s holds no document to compare", label)
	case err != nil:
		return staticPolicyDoc{}, fmt.Errorf("decoding the static %s: %w", label, err)
	}
	if len(body) == 0 {
		return staticPolicyDoc{}, fmt.Errorf("the static %s holds no document to compare", label)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return staticPolicyDoc{}, fmt.Errorf("decoding the static %s: %w", label, err)
	default:
		return staticPolicyDoc{}, fmt.Errorf("the static %s holds more than one document, so none was compared", label)
	}
	metadata, _ := body["metadata"].(map[string]any)
	kind, _ := body["kind"].(string)
	name, _ := metadata["name"].(string)
	namespace, _ := metadata["namespace"].(string)
	switch {
	case kind != object.Kind:
		return staticPolicyDoc{}, fmt.Errorf("the static %s holds a %q document", label, kind)
	case name != object.Name:
		return staticPolicyDoc{}, fmt.Errorf("the static %s holds a document named %q", label, name)
	case namespace != object.Namespace:
		return staticPolicyDoc{}, fmt.Errorf("the static %s names namespace %q, want %q", label, namespace, object.Namespace)
	}
	return staticPolicyDoc{file: staticSource, body: body, kind: object.Kind, name: object.Name, namespace: object.Namespace}, nil
}

// staticObjectLabel names a static object in a refusal: its kind, and the
// namespace and name a reader resolves it by.
func staticObjectLabel(object k8s.StaticObject) string {
	if object.Namespace == "" {
		return object.Kind + " " + object.Name
	}
	return object.Kind + " " + object.Namespace + "/" + object.Name
}

// scopeWord names a scope the way a refusal does.
func scopeWord(cluster bool) string {
	if cluster {
		return "cluster-scoped"
	}
	return "namespaced"
}
