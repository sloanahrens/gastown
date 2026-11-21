package daemon

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestBdGatewayServicePolicyPinsRenderedService holds the Service guard's literals to
// the gateway Service the renderer makes: the type, the selector the guard admits for
// the bd-gateway name, and every label the guard refuses another Service for. The
// guard is read as text because the tests carry no CEL evaluator, as the update
// admission policy's tests do.
func TestBdGatewayServicePolicyPinsRenderedService(t *testing.T) {
	t.Parallel()
	svc := mustBuild(t, bdGatewayWorkloadValues(t)).BdGatewayService
	exprs := bdGatewayServicePolicyExpressions(t, filepath.Join(packageDir(t), "..", "bd-gateway-service-admission.yaml"))
	if len(exprs) != 3 {
		t.Fatalf("the Service guard holds %d validations, want 3: the bd-gateway shape, the bd-gateway-push shape and the other-Service refusal", len(exprs))
	}
	shape, others := exprs[0], exprs[2]

	if want := "object.spec.type == '" + svc.Spec.Type + "'"; !strings.Contains(shape, want) {
		t.Errorf("the bd-gateway validation does not require %q, the type the renderer sets: %q", want, shape)
	}
	if want := "size(object.spec.selector) == " + strconv.Itoa(len(svc.Spec.Selector)); !strings.Contains(shape, want) {
		t.Errorf("the bd-gateway validation does not require %q, the size of the renderer's selector: %q", want, shape)
	}
	for _, k := range sortedKeys(svc.Spec.Selector) {
		want := "k == '" + k + "' && object.spec.selector[k] == '" + svc.Spec.Selector[k] + "'"
		if !strings.Contains(shape, want) {
			t.Errorf("the bd-gateway validation does not admit the renderer's selector pair %q", want)
		}
	}
	for _, clause := range []string{
		"(!has(object.spec.externalIPs) || size(object.spec.externalIPs) == 0)",
		"!has(object.spec.externalName)",
	} {
		if !strings.Contains(shape, clause) {
			t.Errorf("the bd-gateway validation does not refuse an external address: %q is missing", clause)
		}
	}
	if !strings.Contains(others, "!object.spec.selector.all(k,") {
		t.Errorf("the other-Service validation does not refuse a selector made of the gateway's labels")
	}
	for _, k := range sortedKeys(bdGatewayLabels()) {
		want := "k == '" + k + "' && object.spec.selector[k] == '" + bdGatewayLabels()[k] + "'"
		if !strings.Contains(others, want) {
			t.Errorf("the other-Service validation does not treat the gateway's label pair %q as selecting the gateway's pods", want)
		}
	}
}

// TestBdGatewayServiceAdmissionPushLiterals holds the push Service's validation to the
// renderer's push Service: its name, type, the one selector pair, and the one port the
// guard admits, all of them the renderer's constants and the rendered object. The other-
// Service validation must exempt the push name, or the push Service is refused at apply.
func TestBdGatewayServiceAdmissionPushLiterals(t *testing.T) {
	t.Parallel()
	svc := mustBuild(t, bdGatewayWorkloadValues(t)).BdGatewayPushService
	if svc.Metadata.Name != BdGatewayPushName || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != BdGatewayPushPort {
		t.Fatalf("the renderer's push Service is %q with ports %+v, want %q on %d", svc.Metadata.Name, svc.Spec.Ports, BdGatewayPushName, BdGatewayPushPort)
	}
	exprs := bdGatewayServicePolicyExpressions(t, filepath.Join(packageDir(t), "..", "bd-gateway-service-admission.yaml"))
	if len(exprs) != 3 {
		t.Fatalf("the Service guard holds %d validations, want 3", len(exprs))
	}
	push, others := exprs[1], exprs[2]

	for _, want := range []string{
		"object.metadata.name != '" + BdGatewayPushName + "' ||",
		"object.spec.type == '" + svc.Spec.Type + "'",
		"size(object.spec.selector) == " + strconv.Itoa(len(svc.Spec.Selector)),
		"size(object.spec.ports) == " + strconv.Itoa(len(svc.Spec.Ports)),
		"object.spec.ports.all(p, p.port == " + strconv.Itoa(BdGatewayPushPort) + ")",
		"(!has(object.spec.externalIPs) || size(object.spec.externalIPs) == 0)",
		"!has(object.spec.externalName)",
	} {
		if !strings.Contains(push, want) {
			t.Errorf("the bd-gateway-push validation does not carry %q: %q", want, push)
		}
	}
	for _, k := range sortedKeys(svc.Spec.Selector) {
		want := "k == '" + k + "' && object.spec.selector[k] == '" + svc.Spec.Selector[k] + "'"
		if !strings.Contains(push, want) {
			t.Errorf("the bd-gateway-push validation does not admit the renderer's selector pair %q", want)
		}
	}
	if want := "object.metadata.name == '" + BdGatewayPushName + "' ||"; !strings.Contains(others, want) {
		t.Errorf("the other-Service validation does not exempt %q, so the push Service is refused: %q", BdGatewayPushName, others)
	}
}

// serviceGuardDoc is one document of the Service guard file: the policy or its binding,
// with the fields the structure test reads.
type serviceGuardDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		FailurePolicy     string   `yaml:"failurePolicy"`
		PolicyName        string   `yaml:"policyName"`
		ValidationActions []string `yaml:"validationActions"`
		MatchConstraints  struct {
			ResourceRules []struct {
				APIGroups  []string `yaml:"apiGroups"`
				Resources  []string `yaml:"resources"`
				Operations []string `yaml:"operations"`
			} `yaml:"resourceRules"`
		} `yaml:"matchConstraints"`
		MatchResources struct {
			NamespaceSelector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"namespaceSelector"`
		} `yaml:"matchResources"`
		Validations []struct {
			Expression string `yaml:"expression"`
			Message    string `yaml:"message"`
		} `yaml:"validations"`
	} `yaml:"spec"`
}

// bdGatewayServiceGuardDocs returns every document in the file at path, in order.
func bdGatewayServiceGuardDocs(t *testing.T, path string) []serviceGuardDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the Service guard: %v", err)
	}
	var docs []serviceGuardDoc
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc serviceGuardDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("the Service guard is not valid YAML: %v", err)
		}
		docs = append(docs, doc)
	}
}

// bdGatewayServicePolicyExpressions returns the validation expressions of the
// ValidatingAdmissionPolicy in the file at path, in document order.
func bdGatewayServicePolicyExpressions(t *testing.T, path string) []string {
	t.Helper()
	var exprs []string
	for _, doc := range bdGatewayServiceGuardDocs(t, path) {
		if doc.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		for _, v := range doc.Spec.Validations {
			exprs = append(exprs, v.Expression)
		}
	}
	return exprs
}

// TestBdGatewayServicePolicyStructure reads the Service guard's policy and binding as
// typed objects, since the tests carry no CEL evaluator. It holds the matched
// operations, the failure and action modes, the namespace the binding selects, each
// validation's message, and which rendered Services the other-Service validation admits.
// The refusals themselves are proven on the cluster at cutover (gt-wwo7ew).
func TestBdGatewayServicePolicyStructure(t *testing.T) {
	t.Parallel()
	docs := bdGatewayServiceGuardDocs(t, filepath.Join(packageDir(t), "..", "bd-gateway-service-admission.yaml"))
	var policy, binding *serviceGuardDoc
	for i := range docs {
		switch docs[i].Kind {
		case "ValidatingAdmissionPolicy":
			policy = &docs[i]
		case "ValidatingAdmissionPolicyBinding":
			binding = &docs[i]
		}
	}
	if policy == nil || binding == nil {
		t.Fatalf("the Service guard holds no policy or no binding (policy %v, binding %v)", policy != nil, binding != nil)
	}

	// The count is checked before any subtest is registered: a parallel subtest
	// that indexes the validations runs after this function returns, so a guard
	// placed later would not stop it from panicking on a short policy.
	if n := len(policy.Spec.Validations); n != 3 {
		t.Fatalf("the policy holds %d validations, want 3", n)
	}
	t.Run("the policy matches Service creates and updates in the core group", func(t *testing.T) {
		t.Parallel()
		rules := policy.Spec.MatchConstraints.ResourceRules
		if len(rules) != 1 {
			t.Fatalf("the policy has %d resource rules, want one", len(rules))
		}
		ops := slices.Clone(rules[0].Operations)
		slices.Sort(ops)
		if !slices.Equal(ops, []string{"CREATE", "UPDATE"}) {
			t.Errorf("the policy matches operations %v, want CREATE and UPDATE", ops)
		}
		if !slices.Equal(rules[0].APIGroups, []string{""}) || !slices.Equal(rules[0].Resources, []string{"services"}) {
			t.Errorf("the policy matches %v %v, want the core group's services", rules[0].APIGroups, rules[0].Resources)
		}
	})
	t.Run("the policy fails closed", func(t *testing.T) {
		t.Parallel()
		if policy.Spec.FailurePolicy != "Fail" {
			t.Errorf("the policy's failurePolicy is %q, want Fail", policy.Spec.FailurePolicy)
		}
	})
	t.Run("the binding denies and names the policy", func(t *testing.T) {
		t.Parallel()
		if binding.Spec.PolicyName != policy.Metadata.Name {
			t.Errorf("the binding names policy %q, want %q", binding.Spec.PolicyName, policy.Metadata.Name)
		}
		if !slices.Equal(binding.Spec.ValidationActions, []string{"Deny"}) {
			t.Errorf("the binding's validationActions are %v, want Deny", binding.Spec.ValidationActions)
		}
	})
	t.Run("the binding selects only namespace gt", func(t *testing.T) {
		t.Parallel()
		want := map[string]string{"kubernetes.io/metadata.name": "gt"}
		if got := binding.Spec.MatchResources.NamespaceSelector.MatchLabels; !maps.Equal(got, want) {
			t.Errorf("the binding selects namespaces %v, want %v", got, want)
		}
	})
	t.Run("each validation has an expression and a message", func(t *testing.T) {
		t.Parallel()
		for i, v := range policy.Spec.Validations {
			if strings.TrimSpace(v.Expression) == "" || strings.TrimSpace(v.Message) == "" {
				t.Errorf("validation %d has an empty expression or message", i+1)
			}
		}
	})
	t.Run("the other-Service validation exempts the two gateway Service names", func(t *testing.T) {
		t.Parallel()
		others := policy.Spec.Validations[2].Expression
		if !strings.Contains(others, "object.metadata.name == 'bd-gateway' ||") {
			t.Errorf("the other-Service validation does not exempt the Service named bd-gateway: %q", others)
		}
		if !strings.Contains(others, "object.metadata.name == 'bd-gateway-push' ||") {
			t.Errorf("the other-Service validation does not exempt the Service named bd-gateway-push: %q", others)
		}
	})
	// The other-Service refusal is validation index 2 because the push validation (gt-ozn0j0) sits at index 1.
	t.Run("the two pinned validations match the policy text", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			index int
			role  string
			got   string
			want  string
		}{
			{0, "the Service named bd-gateway", policy.Spec.Validations[0].Expression, bdGatewayShapeExpression},
			{2, "the other-Service refusal", policy.Spec.Validations[2].Expression, bdGatewayOtherServiceExpression},
		} {
			t.Run(tc.role, func(t *testing.T) {
				t.Parallel()
				if got := strings.Join(strings.Fields(tc.got), " "); got != tc.want {
					t.Errorf("spec.validations[%d] (%s) in the policy file is\n%q\nwant the pinned text\n%q; after review, change the pinned text together with the YAML", tc.index, tc.role, got, tc.want)
				}
			})
		}
	})

	gateway := bdGatewayLabels()
	for _, svc := range []struct {
		name     string
		selector map[string]string
		admitted bool
	}{
		{name: "the daemon's pods", selector: selectorLabels(), admitted: true},
		{name: "the dashboard's Service", selector: dashboardSelectorLabels(), admitted: true},
		{name: "the tracker's Service", selector: trackerSelectorLabels(), admitted: true},
		{name: "an empty selector", selector: map[string]string{}, admitted: true},
		{name: "the gateway's own labels", selector: gateway, admitted: false},
		{name: "the gateway's managed-by label alone", selector: map[string]string{"app.kubernetes.io/managed-by": "gt"}, admitted: false},
	} {
		verb := "refuses "
		if svc.admitted {
			verb = "admits "
		}
		t.Run("the model "+verb+"a Service selecting "+svc.name, func(t *testing.T) {
			t.Parallel()
			if got := otherServiceGuardAdmits(serviceFixture{name: "dolt", selector: svc.selector}, gateway); got != svc.admitted {
				t.Errorf("the model admits a Service selecting %v as %v, want %v", svc.selector, got, svc.admitted)
			}
		})
	}
}

// otherServiceGuardAdmits is the Go model of the Service guard's other-Service
// validation: the two gateway names are exempt, an empty selector selects nothing so is
// admitted, a selector naming a pair the gateway's pods do not carry is admitted, and a
// selector made only of the gateway's pairs is refused. It also applies the
// bd-gateway-push validation to the push name, which is exempt from the other-Service
// rule; a Service named bd-gateway is admitted here because its own validation is held
// to the renderer by TestBdGatewayServicePolicyPinsRenderedService.
//
// The pinned text in TestBdGatewayServicePolicyStructure ties the other-Service rule to
// the YAML, and the push expression pinned in
// TestBdGatewayServiceAdmissionPushRefusalsPinsExpressions ties pushServiceShapeAdmits,
// so a weakened policy fails one of them until the model and the pinned text change with
// the file. The policy itself, evaluated by the API server, is proven on the cluster at
// cutover (gt-wwo7ew).
func otherServiceGuardAdmits(s serviceFixture, gateway map[string]string) bool {
	if s.name == BdGatewayPushName && !pushServiceShapeAdmits(s) {
		return false
	}
	if s.name == "bd-gateway" || s.name == BdGatewayPushName {
		return true
	}
	if len(s.selector) == 0 {
		return true
	}
	for k, v := range s.selector {
		if want, ok := gateway[k]; !ok || want != v {
			return true
		}
	}
	return false
}

// The Service guard's three validations, with the whitespace the YAML's folded scalars
// leave collapsed. A changed expression in the policy file fails the test that pins it
// until the pinned text is changed with the file: bdGatewayShapeExpression and
// bdGatewayOtherServiceExpression in TestBdGatewayServicePolicyStructure, the push one in
// TestBdGatewayServiceAdmissionPushRefusalsPinsExpressions.
const (
	bdGatewayShapeExpression        = "object.metadata.name != 'bd-gateway' || (has(object.spec.type) && object.spec.type == 'ClusterIP' && (!has(object.spec.externalIPs) || size(object.spec.externalIPs) == 0) && !has(object.spec.externalName) && has(object.spec.selector) && size(object.spec.selector) == 1 && object.spec.selector.exists(k, k == 'app.kubernetes.io/name' && object.spec.selector[k] == 'bd-gateway'))"
	bdGatewayPushExpression         = "object.metadata.name != 'bd-gateway-push' || (has(object.spec.type) && object.spec.type == 'ClusterIP' && (!has(object.spec.externalIPs) || size(object.spec.externalIPs) == 0) && !has(object.spec.externalName) && has(object.spec.selector) && size(object.spec.selector) == 1 && object.spec.selector.exists(k, k == 'app.kubernetes.io/name' && object.spec.selector[k] == 'bd-gateway') && has(object.spec.ports) && size(object.spec.ports) == 1 && object.spec.ports.all(p, p.port == 7441))"
	bdGatewayOtherServiceExpression = "object.metadata.name == 'bd-gateway' || object.metadata.name == 'bd-gateway-push' || !has(object.spec.selector) || size(object.spec.selector) == 0 || !object.spec.selector.all(k, (k == 'app.kubernetes.io/name' && object.spec.selector[k] == 'bd-gateway') || (k == 'app.kubernetes.io/managed-by' && object.spec.selector[k] == 'gt'))"
)

// serviceFixture is the part of a Service the Service guard reads: its name, type,
// external addresses, selector and ports.
type serviceFixture struct {
	name         string
	typ          string
	externalName string
	externalIPs  []string
	selector     map[string]string
	ports        []int
}

// pushServiceShapeAdmits restates the bd-gateway-push validation on one Service: the
// bounds the bd-gateway validation gives, the one selector pair, and the one port.
func pushServiceShapeAdmits(s serviceFixture) bool {
	if s.typ != "ClusterIP" || len(s.externalIPs) != 0 || s.externalName != "" {
		return false
	}
	if len(s.selector) != 1 || s.selector["app.kubernetes.io/name"] != "bd-gateway" {
		return false
	}
	return len(s.ports) == 1 && s.ports[0] == BdGatewayPushPort
}

// TestBdGatewayServiceAdmissionPushRefusals runs the push Service and the other-Service
// fixtures through the guard's model. The rendered push Service is admitted; a NodePort,
// an extra port, a second selector pair, and any other Service name that selects the
// gateway's pods are refused. The repo carries no CEL evaluator, so the refusals are the
// Go restatement, and the cluster dry-run refusals are proven at cutover (gt-wwo7ew).
func TestBdGatewayServiceAdmissionPushRefusals(t *testing.T) {
	t.Parallel()
	gateway := bdGatewayLabels()
	rendered := serviceFixture{
		name:     BdGatewayPushName,
		typ:      "ClusterIP",
		selector: map[string]string{"app.kubernetes.io/name": "bd-gateway"},
		ports:    []int{BdGatewayPushPort},
	}
	with := func(edit func(*serviceFixture)) serviceFixture {
		s := rendered
		s.selector = map[string]string{"app.kubernetes.io/name": "bd-gateway"}
		s.ports = []int{BdGatewayPushPort}
		edit(&s)
		return s
	}
	for _, tc := range []struct {
		why      string
		svc      serviceFixture
		admitted bool
	}{
		{"the rendered push Service", rendered, true},
		{"a NodePort push Service", with(func(s *serviceFixture) { s.typ = "NodePort" }), false},
		{"a LoadBalancer push Service", with(func(s *serviceFixture) { s.typ = "LoadBalancer" }), false},
		{"an ExternalName push Service", with(func(s *serviceFixture) {
			s.typ, s.externalName = "ExternalName", "dolt.example.com"
		}), false},
		{"a push Service with an external IP", with(func(s *serviceFixture) { s.externalIPs = []string{"192.168.5.2"} }), false},
		{"a push Service with a second port", with(func(s *serviceFixture) { s.ports = []int{BdGatewayPushPort, 8080} }), false},
		{"a push Service on another port", with(func(s *serviceFixture) { s.ports = []int{7440} }), false},
		{"a push Service with no port", with(func(s *serviceFixture) { s.ports = nil }), false},
		{"a push Service with a second selector pair", with(func(s *serviceFixture) {
			s.selector["app.kubernetes.io/managed-by"] = "gt"
		}), false},
		{"a push Service with an empty selector", with(func(s *serviceFixture) { s.selector = map[string]string{} }), false},
		{"a push Service selecting only managed-by", with(func(s *serviceFixture) {
			s.selector = map[string]string{"app.kubernetes.io/managed-by": "gt"}
		}), false},
		{"another Service selecting the gateway's pods", serviceFixture{
			name:     "dolt-proxy",
			typ:      "ClusterIP",
			selector: map[string]string{"app.kubernetes.io/name": "bd-gateway"},
			ports:    []int{BdGatewayPushPort},
		}, false},
		{"another Service selecting the gateway's labels and managed-by", serviceFixture{
			name:     "dolt",
			typ:      "ClusterIP",
			selector: map[string]string{"app.kubernetes.io/name": "bd-gateway", "app.kubernetes.io/managed-by": "gt"},
		}, false},
		{"another Service selecting its own pods", serviceFixture{
			name:     "dolt",
			typ:      "ClusterIP",
			selector: map[string]string{"app.kubernetes.io/name": "dolt"},
		}, true},
	} {
		t.Run(tc.why, func(t *testing.T) {
			t.Parallel()
			if got := otherServiceGuardAdmits(tc.svc, gateway); got != tc.admitted {
				t.Errorf("%s (%+v) is admitted = %v, want %v", tc.why, tc.svc, got, tc.admitted)
			}
		})
	}
}

// TestBdGatewayServiceAdmissionPushRefusalsPinsExpressions compares the push validation
// in the policy file with its pinned constant, with
// whitespace collapsed on both sides. The refusal model above restates these rules, so
// a change to the policy text that is not made to the model and the constants fails here.
func TestBdGatewayServiceAdmissionPushRefusalsPinsExpressions(t *testing.T) {
	t.Parallel()
	exprs := bdGatewayServicePolicyExpressions(t, filepath.Join(packageDir(t), "..", "bd-gateway-service-admission.yaml"))
	if len(exprs) != 3 {
		t.Fatalf("the Service guard holds %d validations, want 3", len(exprs))
	}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"the bd-gateway-push validation", exprs[1], bdGatewayPushExpression},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := strings.Join(strings.Fields(tc.got), " "); got != tc.want {
				t.Errorf("%s in the policy file is\n%q\nwant the pinned text\n%q", tc.name, got, tc.want)
			}
		})
	}
}
