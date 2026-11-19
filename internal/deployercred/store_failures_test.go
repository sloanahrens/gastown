package deployercred

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// marshalTestCA is a self-signed certificate, so the kubeconfig under the
// marshal seam passes the CA check.
func marshalTestCA(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("could not generate the test CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gt deployer test CA"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31-1, 0),
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("could not create the test CA certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestDeployerCredStoreKubeconfigMarshal drives the one call kubeconfigDocument
// makes that can fail: yaml.Marshal does not fail for the fixed document.
func TestDeployerCredStoreKubeconfigMarshal(t *testing.T) {
	t.Parallel()
	boom := errors.New("injected failure")
	out, err := kubeconfigDocument(
		"https://127.0.0.1:6443", marshalTestCA(t),
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
