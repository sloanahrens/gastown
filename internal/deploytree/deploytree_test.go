package deploytree

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	digestA = "registry.local/gt/daemon@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "registry.local/gt/trackerd@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// testTrackerHost is the tracker address the fixtures state. It is in
	// TEST-NET-3, so it is obviously not a real host.
	testTrackerHost = "203.0.113.5"
	testTrackerCIDR = testTrackerHost + "/32"
)

// valuesFile is a valid values file for the workspace id.
func valuesFile(ws string) []byte {
	return []byte(fmt.Sprintf(`workspace: %s
namespace: gt-%s
trackerDSNSecret: %s-tracker-dsn
trackerHost: %s
images:
  daemon: %s
  trackerd: %s
`, ws, ws, ws, testTrackerHost, digestA, digestB))
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

// TestTrackerHostMustBeOneAddress pins the tracker address to one IP: a name
// cannot be matched by a NetworkPolicy, and a range could admit another
// workspace's pods.
func TestTrackerHostMustBeOneAddress(t *testing.T) {
	t.Parallel()
	line := "trackerHost: " + testTrackerHost + "\n"
	cases := []struct {
		name string
		file string
		ok   bool
	}{
		{"an IPv4 address", string(valuesFile("alpha")), true},
		{"an IPv6 address", strings.Replace(string(valuesFile("alpha")), line, "trackerHost: 2001:db8::1\n", 1), true},
		{"missing", strings.Replace(string(valuesFile("alpha")), line, "", 1), false},
		{"a hostname", strings.Replace(string(valuesFile("alpha")), line, "trackerHost: tracker.example\n", 1), false},
		{"a range", strings.Replace(string(valuesFile("alpha")), line, "trackerHost: 10.20.0.0/16\n", 1), false},
		{"the whole IPv4 space", strings.Replace(string(valuesFile("alpha")), line, "trackerHost: 0.0.0.0/0\n", 1), false},
		{"an address with a port", strings.Replace(string(valuesFile("alpha")), line, "trackerHost: 203.0.113.5:5432\n", 1), false},
	}
	for _, c := range cases {
		_, err := Load([]byte(c.file))
		if c.ok && err != nil {
			t.Errorf("%s: Load = %v, want accepted", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrBadValues) {
			t.Errorf("%s: Load = %v, want ErrBadValues", c.name, err)
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

type ipBlock struct {
	CIDR string `yaml:"cidr"`
}

type policyPeer struct {
	PodSelector       *selectorSpec `yaml:"podSelector"`
	NamespaceSelector *selectorSpec `yaml:"namespaceSelector"`
	IPBlock           *ipBlock      `yaml:"ipBlock"`
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

func hasType(p networkPolicy, policyType string) bool {
	for _, t := range p.Spec.PolicyTypes {
		if t == policyType {
			return true
		}
	}
	return false
}

// podLabels are the labels the renderer puts on a component's pods and selects
// them by.
func podLabels(ws, component string) map[string]string {
	return map[string]string{"gt.dev/workspace": ws, "gt.dev/component": component}
}

// selectorLabelsMatch reports whether sel names exactly want.
func selectorLabelsMatch(sel *selectorSpec, want map[string]string) bool {
	return sel != nil && maps.Equal(sel.MatchLabels, want)
}

func samePorts(got, want []policyPort) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func isDNSPeer(peer policyPeer) bool {
	return peer.IPBlock == nil &&
		selectorLabelsMatch(peer.NamespaceSelector, map[string]string{"kubernetes.io/metadata.name": "kube-system"}) &&
		selectorLabelsMatch(peer.PodSelector, map[string]string{"k8s-app": "kube-dns"})
}

// TestAllowRulesNameOnlyTheWorkspacesOwnLabels reads the rendered policies
// directly: every peer is this workspace's own pod, the cluster DNS, or the
// one tracker address, so no rule selects a namespace or label of another
// workspace.
func TestAllowRulesNameOnlyTheWorkspacesOwnLabels(t *testing.T) {
	t.Parallel()
	for _, ws := range []string{"alpha", "beta"} {
		ownLabels := false
		for name, p := range policies(t, ws) {
			if !strings.HasPrefix(name, ws+"-") {
				t.Errorf("policy %q does not carry the workspace id", name)
			}
			for _, sel := range selectors(p) {
				if got, ok := sel.MatchLabels["gt.dev/workspace"]; ok && got != ws {
					t.Errorf("policy %s selects label gt.dev/workspace=%s, want %s", name, got, ws)
				}
				if got, ok := sel.MatchLabels["kubernetes.io/metadata.name"]; ok && got != "kube-system" {
					t.Errorf("policy %s selects namespace %s, want only kube-system", name, got)
				}
				if sel.MatchLabels["gt.dev/workspace"] == ws {
					ownLabels = true
				}
			}
			for _, peer := range peers(p) {
				switch {
				case peer.IPBlock != nil:
					if peer.PodSelector != nil || peer.NamespaceSelector != nil {
						t.Errorf("policy %s: an ipBlock peer also names pods or a namespace", name)
					}
					if peer.IPBlock.CIDR != testTrackerCIDR {
						t.Errorf("policy %s: ipBlock admits %s, want only the tracker at %s", name, peer.IPBlock.CIDR, testTrackerCIDR)
					}
				case peer.NamespaceSelector != nil:
					if !isDNSPeer(peer) {
						t.Errorf("policy %s: peer %+v names another namespace, want only cluster DNS", name, peer)
					}
				case peer.PodSelector != nil:
					if peer.PodSelector.MatchLabels["gt.dev/workspace"] != ws {
						t.Errorf("policy %s: podSelector %v does not name workspace %s", name, peer.PodSelector.MatchLabels, ws)
					}
				default:
					t.Errorf("policy %s: a peer selects everything", name)
				}
			}
		}
		if !ownLabels {
			t.Errorf("%s's allow rules do not name its own labels", ws)
		}
	}
}

func TestDaemonToTrackerdAndDNSAreAllowed(t *testing.T) {
	t.Parallel()
	alpha := policies(t, "alpha")

	ing, ok := alpha["alpha-allow-ingress"]
	if !ok {
		t.Fatal("alpha-allow-ingress is not rendered")
	}
	if !hasType(ing, "Ingress") {
		t.Error("alpha-allow-ingress does not declare Ingress")
	}
	if !selectorLabelsMatch(&ing.Spec.PodSelector, podLabels("alpha", "trackerd")) {
		t.Errorf("alpha-allow-ingress selects pods %v, want trackerd", ing.Spec.PodSelector.MatchLabels)
	}
	if len(ing.Spec.Ingress) != 1 {
		t.Fatalf("alpha-allow-ingress has %d ingress rules, want one", len(ing.Spec.Ingress))
	}
	in := ing.Spec.Ingress[0]
	if len(in.From) != 1 || !selectorLabelsMatch(in.From[0].PodSelector, podLabels("alpha", "daemon")) {
		t.Errorf("alpha-allow-ingress admits %+v, want the daemon only", in.From)
	}
	if !samePorts(in.Ports, []policyPort{{"TCP", trackerdPort}}) {
		t.Errorf("alpha-allow-ingress opens ports %+v, want TCP/%d only", in.Ports, trackerdPort)
	}

	eg, ok := alpha["alpha-allow-egress"]
	if !ok {
		t.Fatal("alpha-allow-egress is not rendered")
	}
	if !hasType(eg, "Egress") {
		t.Error("alpha-allow-egress does not declare Egress")
	}
	if !selectorLabelsMatch(&eg.Spec.PodSelector, map[string]string{"gt.dev/workspace": "alpha"}) {
		t.Errorf("alpha-allow-egress selects pods %v, want the workspace's pods", eg.Spec.PodSelector.MatchLabels)
	}
	if len(eg.Spec.Egress) != 2 {
		t.Fatalf("alpha-allow-egress has %d egress rules, want two", len(eg.Spec.Egress))
	}
	var toTrackerd, toDNS bool
	for _, rule := range eg.Spec.Egress {
		if len(rule.To) == 1 &&
			selectorLabelsMatch(rule.To[0].PodSelector, podLabels("alpha", "trackerd")) &&
			rule.To[0].NamespaceSelector == nil && rule.To[0].IPBlock == nil &&
			samePorts(rule.Ports, []policyPort{{"TCP", trackerdPort}}) {
			toTrackerd = true
		}
		if len(rule.To) == 1 && isDNSPeer(rule.To[0]) &&
			samePorts(rule.Ports, []policyPort{{"UDP", dnsPort}, {"TCP", dnsPort}}) {
			toDNS = true
		}
	}
	if !toTrackerd {
		t.Error("no rule admits the workspace's pods to trackerd on its port")
	}
	if !toDNS {
		t.Error("no rule admits the workspace's pods to cluster DNS")
	}
}

// TestTrackerHostEgressReachesTheTracker pins the rule that lets trackerd reach
// the tracker database: without it the tracker is unreachable from the
// rendered town.
func TestTrackerHostEgressReachesTheTracker(t *testing.T) {
	t.Parallel()
	p, ok := policies(t, "alpha")["alpha-allow-tracker-egress"]
	if !ok {
		t.Fatal("alpha-allow-tracker-egress is not rendered")
	}
	if !hasType(p, "Egress") {
		t.Error("alpha-allow-tracker-egress does not declare Egress")
	}
	if !selectorLabelsMatch(&p.Spec.PodSelector, podLabels("alpha", "trackerd")) {
		t.Errorf("alpha-allow-tracker-egress selects pods %v, want trackerd, the component holding the DSN", p.Spec.PodSelector.MatchLabels)
	}
	if len(p.Spec.Egress) != 1 {
		t.Fatalf("alpha-allow-tracker-egress has %d egress rules, want one", len(p.Spec.Egress))
	}
	rule := p.Spec.Egress[0]
	if len(rule.Ports) != 0 {
		t.Errorf("the tracker rule pins ports %+v, but the DSN's port lives in the Secret", rule.Ports)
	}
	if len(rule.To) != 1 || rule.To[0].IPBlock == nil {
		t.Fatalf("the tracker rule admits %+v, want one ipBlock", rule.To)
	}
	if got := rule.To[0].IPBlock.CIDR; got != testTrackerCIDR {
		t.Errorf("the tracker rule admits %s, want the tracker's address %s", got, testTrackerCIDR)
	}
	if rule.To[0].PodSelector != nil || rule.To[0].NamespaceSelector != nil {
		t.Error("the tracker peer also names pods or a namespace, want the address only")
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
		"a malformed second document": string(one) + "---\n[[[[\n",
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
