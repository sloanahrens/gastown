package sbxcreds

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// keyMarker is planted as the key data; no refusal may echo it.
const keyMarker = "PLANTED-KEY-MARKER"

func kubeconfigDoc(certData string) []byte {
	return []byte("apiVersion: v1\nkind: Config\nusers:\n- name: deployer\n  user:\n    client-certificate-data: \"" + certData + "\"\n    client-key-data: \"" + keyMarker + "\"\n")
}

func testCertData(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gt-deployer-apply"},
		NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestCertInfo asserts the certificate's subject and not-after come back.
func TestCertInfo(t *testing.T) {
	t.Parallel()
	subject, notAfter, err := certInfo(kubeconfigDoc(testCertData(t)))
	if err != nil {
		t.Fatalf("certInfo() = %v, want the certificate's subject and not-after", err)
	}
	if want, wantTime := "CN=gt-deployer-apply", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); subject != want || !notAfter.Equal(wantTime) {
		t.Errorf("certInfo() = %q, %v, want %q, %v", subject, notAfter, want, wantTime)
	}
}

// TestCertInfoRefuses drives every refusal and asserts no error echoes the key.
func TestCertInfoRefuses(t *testing.T) {
	t.Parallel()
	b64 := func(typ string) string {
		return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: []byte(keyMarker)}))
	}
	for _, tc := range []struct {
		name, want string
		kubeconfig []byte
	}{
		{"plain text", "not a kubeconfig", []byte("users: " + keyMarker)},
		{"no users", "names no user", []byte("apiVersion: v1\nkind: Config\n")},
		{"no client certificate", "no client-certificate-data", kubeconfigDoc("")},
		{"not base64", "base64 CERTIFICATE", kubeconfigDoc("not base64!")},
		{"a key, not a certificate", "base64 CERTIFICATE", kubeconfigDoc(b64("PRIVATE KEY"))},
		{"an unparsable certificate", "does not parse", kubeconfigDoc(b64("CERTIFICATE"))},
	} {
		_, _, err := certInfo(tc.kubeconfig)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), keyMarker) {
			t.Fatalf("%s: certInfo() = %v, want a refusal naming %q and echoing no key", tc.name, err, tc.want)
		}
	}
}
