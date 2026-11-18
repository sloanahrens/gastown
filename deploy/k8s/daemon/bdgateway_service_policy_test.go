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
	if len(exprs) != 2 {
		t.Fatalf("the Service guard holds %d validations, want 2: the bd-gateway shape and the other-Service refusal", len(exprs))
	}
	shape, others := exprs[0], exprs[1]

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
		if n := len(policy.Spec.Validations); n != 2 {
			t.Fatalf("the policy holds %d validations, want 2", n)
		}
		for i, v := range policy.Spec.Validations {
			if strings.TrimSpace(v.Expression) == "" || strings.TrimSpace(v.Message) == "" {
				t.Errorf("validation %d has an empty expression or message", i+1)
			}
		}
	})
	t.Run("the other-Service validation exempts the bd-gateway name", func(t *testing.T) {
		t.Parallel()
		others := policy.Spec.Validations[1].Expression
		if !strings.Contains(others, "object.metadata.name == 'bd-gateway' ||") {
			t.Errorf("the other-Service validation does not exempt the Service named bd-gateway: %q", others)
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
		t.Run(verb+"a Service selecting "+svc.name, func(t *testing.T) {
			t.Parallel()
			if got := otherServiceGuardAdmits(svc.selector, gateway); got != svc.admitted {
				t.Errorf("the other-Service guard admits a Service selecting %v as %v, want %v", svc.selector, got, svc.admitted)
			}
		})
	}
}

// otherServiceGuardAdmits reports whether the other-Service validation admits a Service
// with the selector: any selector that names a pair the gateway's pods do not carry is
// admitted, and so is an empty one, which selects nothing. A selector made only of the
// gateway's pairs is refused.
func otherServiceGuardAdmits(selector, gateway map[string]string) bool {
	if len(selector) == 0 {
		return true
	}
	for k, v := range selector {
		if want, ok := gateway[k]; !ok || want != v {
			return true
		}
	}
	return false
}
