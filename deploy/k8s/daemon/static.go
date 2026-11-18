package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// staticScopes names every kind the static set may hold and whether it is
// cluster-scoped. A kind missing here is refused rather than read as
// namespaced: a namespaced document with no metadata.namespace would otherwise
// render into whatever namespace the applier's context holds, which the
// allowlist never saw. TestStaticScopesMatchTheEmbeddedSet holds the table
// equal to the kinds k8s.Static returns and their scope.
var staticScopes = map[string]bool{
	"ValidatingAdmissionPolicy":        true,
	"ValidatingAdmissionPolicyBinding": true,
	"ClusterRole":                      true,
	"ClusterRoleBinding":               true,
	"PriorityClass":                    true,
	"Namespace":                        true,
	"ServiceAccount":                   false,
	"Role":                             false,
	"RoleBinding":                      false,
	"Secret":                           false,
}

// staticObjects returns the static control-cluster set in the order Render
// writes it and refuses a document this render may not write. Build passes
// k8s.Static; a source that fails is returned, never read as an empty set.
func staticObjects(source func() ([]k8s.StaticObject, error), renderNamespace string) ([]k8s.StaticObject, error) {
	objects, err := source()
	if err != nil {
		return nil, fmt.Errorf("reading the static control-cluster manifests: %w", err)
	}
	return orderedStatic(objects, renderNamespace)
}

// staticDocument is the part of a static object's document the render judges:
// the fields in the bytes Render writes. The StaticObject metadata beside a
// document is not an authority, because it never reaches the cluster.
type staticDocument struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
}

// readStaticDocument parses exactly one YAML document and refuses one the
// allowlist cannot judge: not a single object, an unknown kind, or no name. Each
// refusal names the object, from the source metadata before the document parses
// and from the document's own kind and name after.
func readStaticDocument(object k8s.StaticObject) (staticDocument, error) {
	var parsed staticDocument
	dec := yaml.NewDecoder(bytes.NewReader(object.Doc))
	if err := dec.Decode(&parsed); err != nil {
		return staticDocument{}, fmt.Errorf("%snot one YAML object: %w", staticSubject(object), err)
	}
	// A separator the source's splitter missed would leave a second object
	// here; Decode reads only the first, so the rest must be refused rather
	// than silently dropped from what the allowlist judged.
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return staticDocument{}, fmt.Errorf("%ssecond document: %w", staticSubject(object), err)
	default:
		return staticDocument{}, fmt.Errorf("%sholds more than one YAML document", staticSubject(object))
	}
	switch {
	case parsed.Kind == "":
		return staticDocument{}, fmt.Errorf("%snames no kind", staticSubject(object))
	case parsed.Metadata.Name == "":
		return staticDocument{}, fmt.Errorf("static %s: names no metadata.name", parsed.Kind)
	}
	if _, known := staticScopes[parsed.Kind]; !known {
		return staticDocument{}, fmt.Errorf("static %s %s: holds a kind the static set does not carry",
			parsed.Kind, parsed.Metadata.Name)
	}
	return parsed, nil
}

// staticSubject opens a refusal with the object's name when the source metadata
// names both kind and name, which is all a refusal can read before the document
// parses.
func staticSubject(object k8s.StaticObject) string {
	if object.Kind != "" && object.Name != "" {
		return "static " + object.Kind + " " + object.Name + ": "
	}
	return "static document: "
}

// orderedStatic orders objects as Render writes them — the cluster-scoped ones
// first, then the namespaced ones, each in k8s.Static's order — so the admission
// policies exist before the Services and workloads they judge. Every decision
// reads the document Render writes, not the StaticObject fields beside it: a
// namespaced document whose namespace is not renderNamespace, the sweep's or
// Forgejo's is refused, naming the object.
func orderedStatic(objects []k8s.StaticObject, renderNamespace string) ([]k8s.StaticObject, error) {
	type judged struct {
		object  k8s.StaticObject
		cluster bool
	}
	documents := make([]judged, 0, len(objects))
	for _, object := range objects {
		doc, err := readStaticDocument(object)
		if err != nil {
			return nil, err
		}
		cluster := staticScopes[doc.Kind]
		switch {
		case cluster && doc.Metadata.Namespace != "":
			return nil, fmt.Errorf("static %s %s is cluster-scoped but its document names namespace %q",
				doc.Kind, doc.Metadata.Name, doc.Metadata.Namespace)
		case !cluster && doc.Metadata.Namespace == "":
			return nil, fmt.Errorf("static %s %s is namespaced but its document names no namespace",
				doc.Kind, doc.Metadata.Name)
		case !cluster && !staticNamespaceAllowed(doc.Metadata.Namespace, renderNamespace):
			return nil, fmt.Errorf("static %s %s is in namespace %q, outside the render's %q, %s or %s",
				doc.Kind, doc.Metadata.Name, doc.Metadata.Namespace, renderNamespace, SweepNamespace, CacheTrimNamespace)
		}
		documents = append(documents, judged{object: object, cluster: cluster})
	}
	ordered := make([]k8s.StaticObject, 0, len(documents))
	for _, doc := range documents {
		if doc.cluster {
			ordered = append(ordered, doc.object)
		}
	}
	for _, doc := range documents {
		if !doc.cluster {
			ordered = append(ordered, doc.object)
		}
	}
	return ordered, nil
}

// staticNamespaceAllowed reports whether a static document may land in
// namespace: the render's own, the sweep's, or Forgejo's, where the cache-trim
// Role lives (the operator's allowlist).
func staticNamespaceAllowed(namespace, renderNamespace string) bool {
	switch namespace {
	case renderNamespace, SweepNamespace, CacheTrimNamespace:
		return true
	}
	return false
}
