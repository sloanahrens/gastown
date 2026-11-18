package k8s

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"
)

// staticDir is where the static manifests live, so a refusal names a path that
// resolves from the repo root.
const staticDir = "deploy/k8s"

// staticFileNames is the embed directive's list in the order Static returns the
// documents; a name the directive does not embed fails Static's read.
var staticFileNames = []string{
	"bd-gateway-service-admission.yaml",
	"update-admission.yaml",
	"update-rbac.yaml",
	"sweep-rbac.yaml",
	"mac-watch-rbac.yaml",
	"priority-classes.yaml",
}

//go:embed bd-gateway-service-admission.yaml update-admission.yaml update-rbac.yaml sweep-rbac.yaml mac-watch-rbac.yaml priority-classes.yaml
var staticManifests embed.FS

// StaticObject is one Kubernetes object from the static manifests, with its
// document kept so a renderer applies the text that was reviewed.
type StaticObject struct {
	APIVersion string
	Kind       string
	Name       string
	// Namespace is empty for a cluster-scoped object (Cluster is true).
	Namespace string
	Cluster   bool
	// SecretNoData is true for a Secret the cluster fills in; Static refuses a
	// Secret that carries data.
	SecretNoData bool
	Doc          []byte
}

// Static returns every object the six embedded manifests hold, in file order
// and then document order.
func Static() ([]StaticObject, error) {
	return staticObjects(staticManifests, staticFileNames)
}

// StaticExcluded lists every YAML file under deploy/k8s that Static does not
// embed, with the owner who decides it and why it is not an applied object. A
// new hand-applied file joins this list or the embedded set, or the accounting
// test fails.
var StaticExcluded = []struct{ File, Owner, Reason string }{
	{"deploy/k8s/control-priorities.yaml", "overseer", "a record table of the priority class each live Forgejo Deployment runs at, not an applied object"},
	{"deploy/k8s/policy/kind.yaml", "seagull", "the sandbox policy the gateway passes per sandbox create, not an object in the cluster"},
	{"deploy/k8s/forgejo-tolerations.yaml", "overseer", "strategic-merge patch text for Deployments that live outside every repo"},
	{"deploy/k8s/gt-cache/claim.yaml", "claude-60ny", "a PVC in namespace openshell, a gt-sbx object (the gt-sbx desired set)"},
}

// staticKinds maps every kind the static set may hold to whether it is
// cluster-scoped. A kind missing here is refused rather than read as namespaced:
// a cluster-scoped kind with no namespace would otherwise render into the
// caller's current namespace.
var staticKinds = map[string]bool{
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

// staticObjects reads files from fsys and returns every document they hold, in
// file order and document order.
func staticObjects(fsys fs.FS, files []string) ([]StaticObject, error) {
	var objects []StaticObject
	for _, file := range files {
		raw, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", staticPath(file), err)
		}
		for i, doc := range splitDocuments(raw) {
			obj, err := parseStaticObject(doc)
			if err != nil {
				return nil, fmt.Errorf("%s: document %d: %w", staticPath(file), i+1, err)
			}
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

// staticPath is the file as a refusal names it.
func staticPath(file string) string {
	return staticDir + "/" + file
}

// staticExcludedFiles returns rows' files relative to the package directory,
// refusing a row that names a path outside deploy/k8s: a row the walk can never
// find would hide its file as unaccounted rather than as a broken row.
func staticExcludedFiles(rows []struct{ File, Owner, Reason string }) ([]string, error) {
	files := make([]string, 0, len(rows))
	for _, row := range rows {
		rel, ok := strings.CutPrefix(row.File, staticDir+"/")
		if !ok {
			return nil, fmt.Errorf("StaticExcluded lists %q, which is not under %s/", row.File, staticDir)
		}
		files = append(files, rel)
	}
	return files, nil
}

// splitDocuments splits one manifest file into its documents on separator lines
// and drops the documents that hold only blank space.
func splitDocuments(raw []byte) [][]byte {
	var docs [][]byte
	var cur []byte
	flush := func() {
		if doc := bytes.TrimSpace(cur); len(doc) > 0 {
			docs = append(docs, doc)
		}
		cur = nil
	}
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		if strings.TrimRight(line, " \t\r\n") == "---" {
			flush()
			continue
		}
		cur = append(cur, line...)
	}
	flush()
	return docs
}

// staticDoc is the part of a document Static reads.
type staticDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Data       *yaml.Node `yaml:"data"`
	StringData *yaml.Node `yaml:"stringData"`
}

// parseStaticObject validates one document and reads the object it holds.
func parseStaticObject(doc []byte) (StaticObject, error) {
	var parsed staticDoc
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&parsed); err != nil {
		return StaticObject{}, fmt.Errorf("not a YAML object: %w", err)
	}
	// The splitter cuts on separator lines alone; a second document here means a
	// separator it did not recognize, and Decode would keep only the first.
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return StaticObject{}, errors.New("more than one YAML document between separators")
	}
	switch {
	case parsed.APIVersion == "":
		return StaticObject{}, errors.New("no apiVersion")
	case parsed.Kind == "":
		return StaticObject{}, errors.New("no kind")
	case parsed.Metadata.Name == "":
		return StaticObject{}, errors.New("no metadata.name")
	}
	cluster, known := staticKinds[parsed.Kind]
	if !known {
		return StaticObject{}, fmt.Errorf("kind %s is not in the static set's kinds", parsed.Kind)
	}
	obj := StaticObject{
		APIVersion: parsed.APIVersion,
		Kind:       parsed.Kind,
		Name:       parsed.Metadata.Name,
		Namespace:  parsed.Metadata.Namespace,
		Cluster:    cluster,
		Doc:        doc,
	}
	if cluster {
		obj.Namespace = ""
	}
	if parsed.Kind == "Secret" {
		if parsed.Data != nil || parsed.StringData != nil {
			return StaticObject{}, fmt.Errorf("Secret %s carries data, which the cluster fills", parsed.Metadata.Name)
		}
		obj.SecretNoData = true
	}
	return obj, nil
}
