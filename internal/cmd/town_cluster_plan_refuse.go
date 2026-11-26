package cmd

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// planUnreviewedStaticSecret names the first Secret document the render's static
// set holds that deploy/k8s does not review, or "" when every one of them is a
// reviewed document. A document is reviewed when a reviewed Secret of the same
// kind, name and namespace declares the same fields and it carries no data or
// stringData: the name or the declared fields alone are not enough, since a
// Secret may reuse the reviewed one's identity with a field, or a value, the
// operator never reviewed.
//
// Secrets stay operator-created: the reviewed static Secret carries no data, the
// cluster fills it in, and the plan creates no Secret's values. So a Secret
// carrying data or stringData is not the reviewed document, whatever its identity
// and declared fields, and it refuses the plan, its name and the key it carries
// named, and never a value. Any other unreviewed one refuses too. A document
// whose fields cannot be read is refused the same way, since it cannot be shown
// to be the reviewed one. reviewedStatic reads the reviewed set; nil reads
// k8s.Static.
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
		// A reviewed Secret that carries data or stringData, or whose document
		// cannot be read, is not recorded: nothing can be shown to match it, so a
		// plan copy of it is refused below.
		if declared, carried := planSecretFields(object); declared != nil && len(carried) == 0 {
			reviewedSecrets[applyKey{kind: object.Kind, name: object.Name, namespace: object.Namespace}] = declared
		}
	}
	for _, object := range static {
		if object.Kind != secretKind {
			continue
		}
		reviewedDeclared, known := reviewedSecrets[applyKey{kind: object.Kind, name: object.Name, namespace: object.Namespace}]
		declared, carried := planSecretFields(object)
		switch {
		case !known:
			return staticObjectLabel(object), nil
		case len(carried) > 0:
			return fmt.Sprintf("%s carrying %s", staticObjectLabel(object), strings.Join(carried, " and ")), nil
		case !reflect.DeepEqual(declared, reviewedDeclared):
			return staticObjectLabel(object), nil
		}
	}
	return "", nil
}

// planSecretFields is a Secret document's declared fields, its document without
// data and stringData, and the keys it carries: the cluster fills a Secret's data
// and the reviewed static Secret carries none, so a plan Secret carrying data or
// stringData is not the reviewed document. A document that cannot be read has no
// declared fields, so nil, and matches no reviewed document. A data or stringData
// of any shape is reported, so a value that is not a name-to-string map is
// refused the same way as one that is.
func planSecretFields(object k8s.StaticObject) (map[string]any, []string) {
	doc, err := staticObjectDoc(object)
	if err != nil {
		return nil, nil
	}
	declared := make(map[string]any, len(doc.body))
	for key, value := range doc.body {
		switch key {
		case "data", "stringData":
			continue
		}
		declared[key] = value
	}
	var carried []string
	if _, ok := doc.body["data"]; ok {
		carried = append(carried, "data")
	}
	if _, ok := doc.body["stringData"]; ok {
		carried = append(carried, "stringData")
	}
	return declared, carried
}
