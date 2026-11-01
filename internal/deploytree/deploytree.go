// Package deploytree renders one town per workspace from a values file (plan
// row S1 of the hosted-platform plan). Two workspaces rendered from the same
// code share no resource name: every name, label and the namespace carry the
// workspace id. The tracker is a value, a reference to a Secret that holds the
// workspace's DSN, so no Postgres workload appears in the rendered town.
//
// The package renders manifests as data and makes no cluster call. The kind
// stack that runs the local town and a later cloud town are meant to render
// through Render; nothing calls it yet, so wiring them in is a later bead.
//
// The rendered NetworkPolicy denies all ingress and egress. The allow rules a
// running town needs (daemon to trackerd, DNS, egress to the tracker's host)
// are not rendered here; they are a later bead, and until it lands the town
// is isolated and cannot run. It has not been run under kind.
package deploytree

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Values is one workspace's values file.
type Values struct {
	// Workspace is the workspace id; every rendered name carries it.
	Workspace string `yaml:"workspace"`
	// Namespace must be NamespaceFor(Workspace); it is stated so a values file
	// reads on its own, and checked so two workspaces cannot share one.
	Namespace string `yaml:"namespace"`
	// TrackerDSNSecret names the Secret holding the workspace's tracker DSN.
	// It is a name only; a DSN in this field is refused.
	TrackerDSNSecret string `yaml:"trackerDSNSecret"`
	// Images maps a component (see Components) to an image digest reference
	// "repo@sha256:<64 hex>". A tag is refused.
	Images map[string]string `yaml:"images"`
}

// Components are the workloads a town renders, in render order.
var Components = []string{"daemon", "trackerd"}

// Resource is one rendered object: its identity and its YAML document.
type Resource struct {
	Kind      string
	Namespace string // empty for cluster-scoped kinds
	Name      string
	YAML      []byte
}

var (
	// ErrBadValues is returned for a values file Load or Render refuses.
	ErrBadValues = errors.New("deploytree: invalid values")

	idRe     = regexp.MustCompile(`^[a-z][a-z0-9]{1,23}$`)
	nameRe   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	digestRe = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]*@sha256:[0-9a-f]{64}$`)
)

// NamespaceFor is the namespace of a workspace's town.
func NamespaceFor(workspace string) string { return "gt-" + workspace }

// Load parses a values file strictly: an unknown key is an error, and the
// result is validated.
func Load(data []byte) (Values, error) {
	var v Values
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&v); err != nil {
		return Values{}, fmt.Errorf("%w: %v", ErrBadValues, err)
	}
	if err := v.Validate(); err != nil {
		return Values{}, err
	}
	return v, nil
}

// Validate checks the values. The DSN field must be a Secret name (lower-case
// letters, digits and dashes), so a URL or a credential pair, which carry
// characters a name cannot, fail here before they reach a manifest. The check
// is the shape of a name and cannot tell a name from a secret that happens to
// have that shape.
func (v Values) Validate() error {
	if !idRe.MatchString(v.Workspace) {
		return fmt.Errorf("%w: workspace %q is not a short lower-case id", ErrBadValues, v.Workspace)
	}
	if v.Namespace != NamespaceFor(v.Workspace) {
		return fmt.Errorf("%w: namespace %q, want %q", ErrBadValues, v.Namespace, NamespaceFor(v.Workspace))
	}
	if !nameRe.MatchString(v.TrackerDSNSecret) {
		return fmt.Errorf("%w: trackerDSNSecret must be a Secret name, not a DSN", ErrBadValues)
	}
	for _, c := range Components {
		if !digestRe.MatchString(v.Images[c]) {
			return fmt.Errorf("%w: images.%s must be repo@sha256:<digest>", ErrBadValues, c)
		}
	}
	keys := make([]string, 0, len(v.Images))
	for c := range v.Images {
		keys = append(keys, c)
	}
	sort.Strings(keys) // the error names the first unknown key, the same on every run
	for _, c := range keys {
		if !contains(Components, c) {
			return fmt.Errorf("%w: images.%s is not a component", ErrBadValues, c)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Render returns the workspace's town as resources sorted by kind, namespace
// and name. Same values, same bytes.
func Render(v Values) ([]Resource, error) { return render(v, yaml.Marshal) }

// render is Render over a marshal function, so a test can make marshaling
// fail.
func render(v Values, marshal func(any) ([]byte, error)) ([]Resource, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	ws, ns := v.Workspace, v.Namespace
	labels := map[string]any{"gt.dev/workspace": ws, "app.kubernetes.io/part-of": "gt-" + ws}
	meta := func(name string, namespaced bool) map[string]any {
		m := map[string]any{"name": name, "labels": labels}
		if namespaced {
			m["namespace"] = ns
		}
		return m
	}
	var res []Resource
	var failed error // the first marshal failure; later objects are skipped
	add := func(kind, apiVersion string, namespaced bool, name string, body map[string]any) {
		if failed != nil {
			return
		}
		doc := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta(name, namespaced)}
		for k, x := range body {
			doc[k] = x
		}
		out, err := marshal(doc)
		if err != nil {
			failed = fmt.Errorf("deploytree: marshal %s %s: %w", kind, name, err)
			return
		}
		r := Resource{Kind: kind, Name: name, YAML: out}
		if namespaced {
			r.Namespace = ns
		}
		res = append(res, r)
	}
	add("Namespace", "v1", false, ns, nil)
	add("NetworkPolicy", "networking.k8s.io/v1", true, ws+"-default-deny", map[string]any{
		"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"}},
	})
	for _, c := range Components {
		name := ws + "-" + c
		sel := map[string]any{"gt.dev/workspace": ws, "gt.dev/component": c}
		add("ServiceAccount", "v1", true, name, nil)
		container := map[string]any{"name": c, "image": v.Images[c]}
		if c == "trackerd" {
			container["env"] = []any{map[string]any{"name": "TRACKER_DSN", "valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": v.TrackerDSNSecret, "key": "dsn"}}}}
		}
		add("Deployment", "apps/v1", true, name, map[string]any{"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": sel},
			"template": map[string]any{
				"metadata": map[string]any{"labels": merge(labels, sel)},
				"spec":     map[string]any{"serviceAccountName": name, "containers": []any{container}},
			},
		}})
		if c == "trackerd" {
			add("Service", "v1", true, name, map[string]any{"spec": map[string]any{
				"selector": sel, "ports": []any{map[string]any{"port": 7433}},
			}})
		}
	}
	if failed != nil {
		return nil, failed
	}
	sort.Slice(res, func(i, j int) bool {
		a, b := res[i], res[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return res, nil
}

func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// YAML joins resources into one multi-document stream.
func YAML(res []Resource) []byte {
	var parts []string
	for _, r := range res {
		parts = append(parts, string(r.YAML))
	}
	return []byte(strings.Join(parts, "---\n"))
}
