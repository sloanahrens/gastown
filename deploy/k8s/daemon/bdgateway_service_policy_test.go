package daemon

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// bdGatewayServicePolicyExpressions returns the validation expressions of the
// ValidatingAdmissionPolicy in the file at path, in document order.
func bdGatewayServicePolicyExpressions(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the Service guard: %v", err)
	}
	var exprs []string
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Validations []struct {
					Expression string `yaml:"expression"`
				} `yaml:"validations"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return exprs
		}
		if err != nil {
			t.Fatalf("the Service guard is not valid YAML: %v", err)
		}
		if doc.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		for _, v := range doc.Spec.Validations {
			exprs = append(exprs, v.Expression)
		}
	}
}
