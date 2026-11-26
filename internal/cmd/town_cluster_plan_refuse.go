package cmd

import (
	"fmt"
	"reflect"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// planUnreviewedStaticSecret names the first Secret document the render's static
// set holds that deploy/k8s does not review, or "" when every one of them is a
// reviewed document. A document is reviewed when a reviewed Secret of the same
// kind, name and namespace declares the same fields: the name alone is not
// enough, since a Secret may reuse the reviewed one's name with fields the
// operator never reviewed. data and stringData are left out of the comparison, as
// the plan leaves them out: the cluster fills a Secret's data, and the reviewed
// static Secret carries none.
//
// Secrets stay operator-created: the reviewed static Secret carries no data and
// the cluster fills it in, so it is the only Secret document a plan may hold. Any
// other one refuses the plan. A document whose fields cannot be read has none, so
// it matches no reviewed document and refuses too. reviewedStatic reads the
// reviewed set; nil reads k8s.Static.
func planUnreviewedStaticSecret(static []k8s.StaticObject, reviewedStatic func() ([]k8s.StaticObject, error)) (string, error) {
	if reviewedStatic == nil {
		reviewedStatic = k8s.Static
	}
	reviewed, err := reviewedStatic()
	if err != nil {
		return "", fmt.Errorf("cannot read the static documents deploy/k8s reviews: %w", err)
	}
	reviewedSecrets := make(map[applyKey]map[string]any, len(reviewed))
	for _, object := range reviewed {
		if object.Kind != secretKind {
			continue
		}
		if declared := planSecretDeclaredFields(object); declared != nil {
			reviewedSecrets[applyKey{kind: object.Kind, name: object.Name, namespace: object.Namespace}] = declared
		}
	}
	for _, object := range static {
		if object.Kind != secretKind {
			continue
		}
		reviewedDeclared, known := reviewedSecrets[applyKey{kind: object.Kind, name: object.Name, namespace: object.Namespace}]
		if !known || !reflect.DeepEqual(planSecretDeclaredFields(object), reviewedDeclared) {
			return staticObjectLabel(object), nil
		}
	}
	return "", nil
}

// planSecretDeclaredFields is a Secret document's declared fields: its document
// without data and stringData, which the cluster owns and fills. A document that
// cannot be read has none, so nil, and matches no reviewed document.
func planSecretDeclaredFields(object k8s.StaticObject) map[string]any {
	doc, err := staticObjectDoc(object)
	if err != nil {
		return nil
	}
	declared := make(map[string]any, len(doc.body))
	for key, value := range doc.body {
		switch key {
		case "data", "stringData":
			continue
		}
		declared[key] = value
	}
	return declared
}
