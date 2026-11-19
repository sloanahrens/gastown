package deployercred

import (
	"errors"
	"strings"
	"testing"
)

// TestDeployerCredStoreKubeconfigMarshal drives the one call kubeconfigDocument
// makes that can fail: yaml.Marshal does not fail for the fixed document.
func TestDeployerCredStoreKubeconfigMarshal(t *testing.T) {
	t.Parallel()
	boom := errors.New("injected failure")
	const ca = "-----BEGIN CERTIFICATE-----\nY2E=\n-----END CERTIFICATE-----\n"
	out, err := kubeconfigDocument(
		"https://127.0.0.1:6443", []byte(ca),
		"/home/example/.gt/deployer/gt-deployer-apply.crt",
		"/home/example/.gt/deployer/gt-deployer-apply.key",
		"gt-deployer-apply",
		func(any) ([]byte, error) { return nil, boom },
	)
	if err == nil {
		t.Fatal("kubeconfigDocument accepted a marshal that failed")
	}
	if !strings.Contains(err.Error(), "marshal the gt-deployer-apply kubeconfig: injected failure") {
		t.Fatalf("the error %q does not name the marshal", err)
	}
	if out != nil {
		t.Fatalf("the failure returned a kubeconfig: %q", out)
	}
}
