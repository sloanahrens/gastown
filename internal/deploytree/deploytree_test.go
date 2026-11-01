package deploytree

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const digestA = "registry.local/gt/daemon@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "registry.local/gt/trackerd@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// valuesFile is a valid values file for the workspace id.
func valuesFile(ws string) []byte {
	return []byte(fmt.Sprintf(`workspace: %s
namespace: gt-%s
trackerDSNSecret: %s-tracker-dsn
images:
  daemon: %s
  trackerd: %s
`, ws, ws, ws, digestA, digestB))
}

func mustRender(t *testing.T, ws string) []Resource {
	t.Helper()
	v, err := Load(valuesFile(ws))
	if err != nil {
		t.Fatalf("Load(%s): %v", ws, err)
	}
	res, err := Render(v)
	if err != nil {
		t.Fatalf("Render(%s): %v", ws, err)
	}
	return res
}

func identities(res []Resource) map[string]bool {
	m := map[string]bool{}
	for _, r := range res {
		m[r.Kind+"/"+r.Namespace+"/"+r.Name] = true
	}
	return m
}

func TestTwoWorkspacesShareNoResourceName(t *testing.T) {
	t.Parallel()
	a, b := identities(mustRender(t, "alpha")), identities(mustRender(t, "beta"))
	if len(a) < 6 {
		t.Fatalf("alpha rendered %d resources, want a whole town", len(a))
	}
	for id := range a {
		if b[id] {
			t.Errorf("both workspaces render %s", id)
		}
	}
	for _, r := range mustRender(t, "alpha") {
		if !strings.Contains(r.Name, "alpha") {
			t.Errorf("%s %s does not carry the workspace id", r.Kind, r.Name)
		}
		if r.Kind != "Namespace" && !strings.Contains(string(r.YAML), "gt.dev/workspace: alpha") {
			t.Errorf("%s %s has no workspace label", r.Kind, r.Name)
		}
	}
}

func TestNoPostgresServiceRendered(t *testing.T) {
	t.Parallel()
	for _, r := range mustRender(t, "alpha") {
		low := strings.ToLower(string(r.YAML))
		if strings.Contains(low, "postgres") || r.Kind == "StatefulSet" {
			t.Errorf("%s %s is or mentions Postgres:\n%s", r.Kind, r.Name, r.YAML)
		}
	}
}

func TestDSNIsASecretReferenceOnly(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"postgres://user:pw@db.example:5432/gt",
		"user:pw@db",
		"Not A Name",
		"",
	} {
		file := strings.Replace(string(valuesFile("alpha")), "alpha-tracker-dsn", bad, 1)
		if bad == "" {
			file = strings.Replace(string(valuesFile("alpha")), "trackerDSNSecret: alpha-tracker-dsn\n", "", 1)
		}
		_, err := Load([]byte(file))
		if !errors.Is(err, ErrBadValues) {
			t.Errorf("Load with trackerDSNSecret %q returned %v, want ErrBadValues", bad, err)
		}
	}
	// The rendered town refers to the Secret by name and holds no DSN.
	for _, r := range mustRender(t, "alpha") {
		if strings.Contains(string(r.YAML), "://") {
			t.Errorf("%s %s carries a URL:\n%s", r.Kind, r.Name, r.YAML)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	first, second := YAML(mustRender(t, "alpha")), YAML(mustRender(t, "alpha"))
	if !bytes.Equal(first, second) {
		t.Fatal("two renders of one values file differ")
	}
}

func TestValuesAreValidated(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"namespace of another workspace": strings.Replace(string(valuesFile("alpha")), "namespace: gt-alpha", "namespace: gt-beta", 1),
		"image by tag":                   strings.Replace(string(valuesFile("alpha")), digestA, "registry.local/gt/daemon:latest", 1),
		"unknown key":                    string(valuesFile("alpha")) + "extra: 1\n",
		"unknown component":              string(valuesFile("alpha")) + "  postgres: registry.local/pg@sha256:" + strings.Repeat("c", 64) + "\n",
		"bad workspace id":               strings.ReplaceAll(string(valuesFile("alpha")), "alpha", "Al-pha"),
	}
	for name, file := range cases {
		if _, err := Load([]byte(file)); !errors.Is(err, ErrBadValues) {
			t.Errorf("%s: Load returned %v, want ErrBadValues", name, err)
		}
	}
}

func TestMarshalFailureFailsTheRender(t *testing.T) {
	t.Parallel()
	v, err := Load(valuesFile("alpha"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	boom := errors.New("marshal refused")
	res, err := render(v, func(any) ([]byte, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("render returned %v, want the marshal error", err)
	}
	if res != nil {
		t.Errorf("failed render returned %d resources, want none", len(res))
	}
}

func TestRenderRefusesInvalidValues(t *testing.T) {
	t.Parallel()
	if _, err := Render(Values{Workspace: "alpha"}); !errors.Is(err, ErrBadValues) {
		t.Fatalf("Render of incomplete values returned %v, want ErrBadValues", err)
	}
}

func TestUnknownImageErrorNamesTheFirstKeyEveryTime(t *testing.T) {
	t.Parallel()
	file := string(valuesFile("alpha")) + "  zeta: registry.local/z@sha256:" + strings.Repeat("c", 64) +
		"\n  beta: registry.local/b@sha256:" + strings.Repeat("c", 64) + "\n"
	for i := 0; i < 20; i++ {
		_, err := Load([]byte(file))
		if err == nil || !strings.Contains(err.Error(), "images.beta") {
			t.Fatalf("Load = %v, want the error to name images.beta every time", err)
		}
	}
}

func TestTwoWorkspacesCannotNameOneDSNSecret(t *testing.T) {
	t.Parallel()
	for _, ws := range []string{"alpha", "beta"} {
		file := strings.ReplaceAll(string(valuesFile(ws)), ws+"-tracker-dsn", "shared-tracker-dsn")
		if _, err := Load([]byte(file)); !errors.Is(err, ErrBadValues) {
			t.Errorf("workspace %s naming shared-tracker-dsn: Load = %v, want ErrBadValues", ws, err)
		}
	}
}

// networkPolicy is the part of a rendered NetworkPolicy these tests read.
type networkPolicy struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		PodSelector selectorSpec `yaml:"podSelector"`
		PolicyTypes []string     `yaml:"policyTypes"`
		Ingress     []policyRule `yaml:"ingress"`
		Egress      []policyRule `yaml:"egress"`
	} `yaml:"spec"`
}

type selectorSpec struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

type policyPeer struct {
	PodSelector       *selectorSpec `yaml:"podSelector"`
	NamespaceSelector *selectorSpec `yaml:"namespaceSelector"`
}

type policyRule struct {
	From  []policyPeer `yaml:"from"`
	To    []policyPeer `yaml:"to"`
	Ports []policyPort `yaml:"ports"`
}

type policyPort struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
}

// nsRef is a namespace by name and labels.
type nsRef struct {
	name   string
	labels map[string]string
}

// policies returns the rendered NetworkPolicies of a workspace by name.
func policies(t *testing.T, ws string) map[string]networkPolicy {
	t.Helper()
	out := map[string]networkPolicy{}
	for _, r := range mustRender(t, ws) {
		if r.Kind != "NetworkPolicy" {
			continue
		}
		var p networkPolicy
		if err := yaml.Unmarshal(r.YAML, &p); err != nil {
			t.Fatalf("unmarshal %s: %v", r.Name, err)
		}
		if p.Metadata.Name != r.Name {
			t.Fatalf("%s renders metadata.name %q", r.Name, p.Metadata.Name)
		}
		out[r.Name] = p
	}
	return out
}

// selectors lists every label selector in a policy: its pod selector, and each
// rule peer's pod and namespace selectors.
func selectors(p networkPolicy) []selectorSpec {
	out := []selectorSpec{p.Spec.PodSelector}
	for _, rules := range [][]policyRule{p.Spec.Ingress, p.Spec.Egress} {
		for _, rule := range rules {
			for _, peers := range [][]policyPeer{rule.From, rule.To} {
				for _, peer := range peers {
					if peer.PodSelector != nil {
						out = append(out, *peer.PodSelector)
					}
					if peer.NamespaceSelector != nil {
						out = append(out, *peer.NamespaceSelector)
					}
				}
			}
		}
	}
	return out
}

// peers lists every rule peer in a policy.
func peers(p networkPolicy) []policyPeer {
	var out []policyPeer
	for _, rules := range [][]policyRule{p.Spec.Ingress, p.Spec.Egress} {
		for _, rule := range rules {
			out = append(out, rule.From...)
			out = append(out, rule.To...)
		}
	}
	return out
}

// matches reports whether a selector selects an object with these labels. An
// empty selector selects everything.
func matches(sel selectorSpec, labels map[string]string) bool {
	for k, v := range sel.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// peerAllows reports whether a rule peer admits a pod labelled pod in a
// namespace labelled ns. A peer with no namespaceSelector admits only pods in
// the policy's own namespace, by name.
func peerAllows(peer policyPeer, policyNS, ns nsRef, pod map[string]string) bool {
	if peer.NamespaceSelector != nil {
		if !matches(*peer.NamespaceSelector, ns.labels) {
			return false
		}
	} else if ns.name != policyNS.name {
		return false
	}
	if peer.PodSelector != nil && !matches(*peer.PodSelector, pod) {
		return false
	}
	return true
}

// portsAllow reports whether a rule's port list admits port. An empty list
// admits every port.
func portsAllow(ports []policyPort, port policyPort) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

func hasType(p networkPolicy, policyType string) bool {
	for _, t := range p.Spec.PolicyTypes {
		if t == policyType {
			return true
		}
	}
	return false
}

// allowsEgress reports whether any of ps admits a pod labelled src in
// policyNS, selected by the policy, egressing to a pod labelled dst in dstNS on
// port.
func allowsEgress(ps map[string]networkPolicy, policyNS nsRef, src map[string]string, dstNS nsRef, dst map[string]string, port policyPort) bool {
	for _, p := range ps {
		if !hasType(p, "Egress") || !matches(p.Spec.PodSelector, src) {
			continue
		}
		for _, rule := range p.Spec.Egress {
			if !portsAllow(rule.Ports, port) {
				continue
			}
			for _, peer := range rule.To {
				if peerAllows(peer, policyNS, dstNS, dst) {
					return true
				}
			}
		}
	}
	return false
}

// allowsIngress reports whether any of ps admits a pod labelled src in srcNS
// reaching a pod labelled dst on port, where the policy selects dst in
// policyNS.
func allowsIngress(ps map[string]networkPolicy, policyNS, srcNS nsRef, src map[string]string, dst map[string]string, port policyPort) bool {
	for _, p := range ps {
		if !hasType(p, "Ingress") || !matches(p.Spec.PodSelector, dst) {
			continue
		}
		for _, rule := range p.Spec.Ingress {
			if !portsAllow(rule.Ports, port) {
				continue
			}
			for _, peer := range rule.From {
				if peerAllows(peer, policyNS, srcNS, src) {
					return true
				}
			}
		}
	}
	return false
}

// workspaceNS is a workspace's namespace as a cluster labels it: the rendered
// labels plus the name label Kubernetes adds.
func workspaceNS(ws string) nsRef {
	return nsRef{name: "gt-" + ws, labels: map[string]string{
		"gt.dev/workspace": ws, "app.kubernetes.io/part-of": "gt-" + ws, "kubernetes.io/metadata.name": "gt-" + ws,
	}}
}

func componentPod(ws, component string) map[string]string {
	return map[string]string{"gt.dev/workspace": ws, "gt.dev/component": component, "app.kubernetes.io/part-of": "gt-" + ws}
}

func TestAllowRulesNameOnlyTheWorkspacesOwnLabels(t *testing.T) {
	t.Parallel()
	alpha, beta := policies(t, "alpha"), policies(t, "beta")
	alphaNS, betaNS := workspaceNS("alpha"), workspaceNS("beta")
	betaPods := []map[string]string{componentPod("beta", "daemon"), componentPod("beta", "trackerd")}

	namesOwnLabels := false
	for name, p := range alpha {
		if !strings.HasPrefix(name, "alpha-") {
			t.Errorf("policy %q does not carry the workspace id", name)
		}
		for _, sel := range selectors(p) {
			if ws, ok := sel.MatchLabels["gt.dev/workspace"]; ok && ws != "alpha" {
				t.Errorf("policy %s selects label gt.dev/workspace=%s", name, ws)
			}
			if ns, ok := sel.MatchLabels["kubernetes.io/metadata.name"]; ok && ns != "kube-system" {
				t.Errorf("policy %s selects namespace %s", name, ns)
			}
			if ws, ok := sel.MatchLabels["gt.dev/workspace"]; ok && ws == "alpha" {
				namesOwnLabels = true
			}
		}
		for _, peer := range peers(p) {
			for _, pod := range betaPods {
				if peerAllows(peer, alphaNS, betaNS, pod) {
					t.Errorf("policy %s admits a beta pod with peer %+v", name, peer)
				}
			}
		}
	}
	if !namesOwnLabels {
		t.Fatal("no allow rule names the workspace's own labels")
	}

	// The same holds for the other workspace: its rules name beta, never alpha.
	betaNamesOwnLabels := false
	for name, p := range beta {
		for _, sel := range selectors(p) {
			if ws, ok := sel.MatchLabels["gt.dev/workspace"]; ok {
				if ws != "beta" {
					t.Errorf("policy %s selects label gt.dev/workspace=%s", name, ws)
				}
				betaNamesOwnLabels = true
			}
		}
		for _, peer := range peers(p) {
			for _, pod := range []map[string]string{componentPod("alpha", "daemon"), componentPod("alpha", "trackerd")} {
				if peerAllows(peer, betaNS, alphaNS, pod) {
					t.Errorf("policy %s admits an alpha pod with peer %+v", name, peer)
				}
			}
		}
	}
	if !betaNamesOwnLabels {
		t.Fatal("beta's allow rules do not name its own labels")
	}
}

func TestDaemonToTrackerdAndDNSAreAllowed(t *testing.T) {
	t.Parallel()
	alpha := policies(t, "alpha")
	alphaNS := workspaceNS("alpha")
	daemon, trackerd := componentPod("alpha", "daemon"), componentPod("alpha", "trackerd")
	dnsNS := nsRef{name: "kube-system", labels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}
	dnsPod := map[string]string{"k8s-app": "kube-dns"}

	if !allowsIngress(alpha, alphaNS, alphaNS, daemon, trackerd, policyPort{"TCP", trackerPort}) {
		t.Error("the daemon is not allowed to reach trackerd on its port")
	}
	if !allowsEgress(alpha, alphaNS, daemon, alphaNS, trackerd, policyPort{"TCP", trackerPort}) {
		t.Error("the daemon is not allowed to egress to trackerd on its port")
	}
	for _, port := range []policyPort{{"UDP", dnsPort}, {"TCP", dnsPort}} {
		if !allowsEgress(alpha, alphaNS, daemon, dnsNS, dnsPod, port) {
			t.Errorf("the daemon is not allowed to egress to cluster DNS over %s/%d", port.Protocol, port.Port)
		}
	}
}

func TestEgressWiderThanTheAllowRulesIsNotAllowed(t *testing.T) {
	t.Parallel()
	alpha := policies(t, "alpha")
	alphaNS := workspaceNS("alpha")
	daemon, trackerd := componentPod("alpha", "daemon"), componentPod("alpha", "trackerd")
	dnsNS := nsRef{name: "kube-system", labels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}
	dnsPod := map[string]string{"k8s-app": "kube-dns"}

	cases := []struct {
		name  string
		src   map[string]string
		dstNS nsRef
		dst   map[string]string
		port  policyPort
	}{
		{"trackerd on another port", daemon, alphaNS, trackerd, policyPort{"TCP", trackerPort + 1}},
		{"another component on trackerd's port", daemon, alphaNS, daemon, policyPort{"TCP", trackerPort}},
		{"trackerd on another protocol", daemon, alphaNS, trackerd, policyPort{"UDP", trackerPort}},
		{"DNS on another port", daemon, dnsNS, dnsPod, policyPort{"UDP", dnsPort + 1}},
	}
	for _, c := range cases {
		if allowsEgress(alpha, alphaNS, c.src, c.dstNS, c.dst, c.port) {
			t.Errorf("%s: egress is allowed, want it refused", c.name)
		}
	}
}

func TestDefaultDenyStaysDenyAll(t *testing.T) {
	t.Parallel()
	p, ok := policies(t, "alpha")["alpha-default-deny"]
	if !ok {
		t.Fatal("alpha-default-deny is not rendered")
	}
	if len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 0 {
		t.Errorf("default-deny has rules: %d ingress, %d egress", len(p.Spec.Ingress), len(p.Spec.Egress))
	}
	for _, want := range []string{"Ingress", "Egress"} {
		if !hasType(p, want) {
			t.Errorf("default-deny does not deny %s", want)
		}
	}
	if len(p.Spec.PodSelector.MatchLabels) != 0 {
		t.Errorf("default-deny selects pods by label, want every pod")
	}
}

func TestMultiDocumentValuesAreRefused(t *testing.T) {
	t.Parallel()
	one := valuesFile("alpha")
	cases := map[string]string{
		"a second values document":    string(one) + "---\n" + string(valuesFile("beta")),
		"a scalar second document":    string(one) + "---\n42\n",
		"an empty second document":    string(one) + "---\n",
		"a comment-only document":     string(one) + "---\n# nothing\n",
		"a malformed second document": string(one) + "---\n\tworkspace: beta\n",
	}
	for name, input := range cases {
		if _, err := Load([]byte(input)); !errors.Is(err, ErrBadValues) {
			t.Errorf("%s: Load = %v, want ErrBadValues", name, err)
		}
	}
	if _, err := Load(one); err != nil {
		t.Fatalf("one document: Load = %v, want it accepted", err)
	}
}
