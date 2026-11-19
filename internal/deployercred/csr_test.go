// The reader this test passes to NewRequest is the key's only entropy source,
// so a refusal that reads none made no key. Since Go 1.26 crypto/ecdsa takes
// its bytes from crypto/rand unless this setting restores the caller's reader.
//go:debug cryptocustomrand=1

package deployercred_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/deployercred"
)

const (
	day        = 24 * time.Hour
	applyMax   = 30 * day
	policyMax  = day
	csrSigner  = "kubernetes.io/kube-apiserver-client"
	csrVersion = "certificates.k8s.io/v1"
)

// detRand is a deterministic byte stream, so a test key is fixed with no clock
// and no sleep.
type detRand struct {
	ctr  uint64
	rest []byte
}

func (r *detRand) Read(p []byte) (int, error) {
	for i := range p {
		if len(r.rest) == 0 {
			sum := sha256.Sum256(binary.BigEndian.AppendUint64(nil, r.ctr))
			r.ctr++
			r.rest = sum[:]
		}
		p[i] = r.rest[0]
		r.rest = r.rest[1:]
	}
	return len(p), nil
}

// countingReader records how many bytes a call took from its source: a refusal
// that read none made no key.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// failingReader makes the key generation itself fail.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("the entropy source is unavailable")
}

// csrDoc is the submitted object as the API server's schema reads it, decoded
// independently of the package's own struct.
type csrDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Request           string   `yaml:"request"`
		SignerName        string   `yaml:"signerName"`
		Usages            []string `yaml:"usages"`
		ExpirationSeconds int64    `yaml:"expirationSeconds"`
	} `yaml:"spec"`
}

// parseKey parses keyPEM as a P-256 PKCS8 private key.
func parseKey(t *testing.T, keyPEM []byte) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("the key is not a PRIVATE KEY PEM block: %q", keyPEM)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("the key does not parse as PKCS8: %v", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		t.Fatalf("the key is not an ECDSA P-256 key: %T", key)
	}
	return ec
}

// parseRequest decodes the object's request field and verifies its signature.
func parseRequest(t *testing.T, request string) *x509.CertificateRequest {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(request)
	if err != nil {
		t.Fatalf("the request is not standard base64: %v", err)
	}
	block, _ := pem.Decode(der)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("the request is not a CERTIFICATE REQUEST PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("the request does not parse: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("the request is not signed by its key: %v", err)
	}
	return csr
}

func TestDeployerCredRequestSubject(t *testing.T) {
	t.Parallel()
	// The reader is counted so the refusal tests' "read nothing" assertion is
	// known to be a real measurement: a success reads its entropy here.
	counter := &countingReader{r: &detRand{}}
	keyPEM, csrYAML, err := deployercred.NewRequest(deployercred.IdentityApply, applyMax, counter)
	if err != nil {
		t.Fatalf("NewRequest refused the maximum apply lifetime: %v", err)
	}
	if counter.n == 0 {
		t.Fatal("the key was not generated from the random reader passed in")
	}
	key := parseKey(t, keyPEM)

	var doc csrDoc
	if err := yaml.Unmarshal(csrYAML, &doc); err != nil {
		t.Fatalf("the object does not parse: %v", err)
	}
	csr := parseRequest(t, doc.Spec.Request)
	if csr.Subject.CommonName != string(deployercred.IdentityApply) {
		t.Fatalf("the subject common name is %q, want %q", csr.Subject.CommonName, deployercred.IdentityApply)
	}
	if len(csr.Subject.Organization) != 1 || csr.Subject.Organization[0] != "gt-deployers" {
		t.Fatalf("the subject organization is %q, want [gt-deployers]", csr.Subject.Organization)
	}
	if !key.PublicKey.Equal(csr.PublicKey) {
		t.Fatalf("the request's public key is not the generated key's")
	}
}

func TestDeployerCredRequestRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		id        deployercred.Identity
		lifetime  time.Duration
		nilReader bool
		wantErr   string
	}{
		{name: "an unknown identity", id: "gt-deployer-admin", lifetime: applyMax, wantErr: "the deployer identity is"},
		{name: "a lifetime over the apply maximum", id: deployercred.IdentityApply, lifetime: applyMax + time.Second, wantErr: "at most"},
		{name: "a lifetime over the policy maximum", id: deployercred.IdentityPolicy, lifetime: policyMax + time.Second, wantErr: "at most"},
		{name: "a lifetime under one hour", id: deployercred.IdentityApply, lifetime: time.Hour - time.Second, wantErr: "at least"},
		{name: "a zero lifetime", id: deployercred.IdentityApply, lifetime: 0, wantErr: "at least"},
		{name: "a negative lifetime", id: deployercred.IdentityPolicy, lifetime: -time.Hour, wantErr: "at least"},
		{name: "a sub-second lifetime", id: deployercred.IdentityApply, lifetime: time.Hour + 500*time.Millisecond, wantErr: "whole number"},
		{name: "a nil random reader", id: deployercred.IdentityApply, lifetime: applyMax, nilReader: true, wantErr: "random reader is nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := &countingReader{r: &detRand{}}
			var source io.Reader = counter
			if tc.nilReader {
				source = nil
			}
			keyPEM, csrYAML, err := deployercred.NewRequest(tc.id, tc.lifetime, source)
			if err == nil {
				t.Fatalf("NewRequest accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("the error %q does not say %q", err, tc.wantErr)
			}
			if counter.n != 0 {
				t.Fatalf("the refusal read %d bytes from the random reader: a key was generated", counter.n)
			}
			if keyPEM != nil || csrYAML != nil {
				t.Fatalf("the refusal returned output: key %q, object %q", keyPEM, csrYAML)
			}
		})
	}

	t.Run("a random reader that fails", func(t *testing.T) {
		keyPEM, csrYAML, err := deployercred.NewRequest(deployercred.IdentityApply, applyMax, failingReader{})
		if err == nil {
			t.Fatal("NewRequest accepted a failing random reader")
		}
		if !strings.Contains(err.Error(), "generate the gt-deployer-apply client key") {
			t.Fatalf("the error %q does not name the key generation", err)
		}
		if keyPEM != nil || csrYAML != nil {
			t.Fatalf("the failed generation returned output: key %q, object %q", keyPEM, csrYAML)
		}
	})
}

func TestDeployerCredRequestObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		id          deployercred.Identity
		lifetime    time.Duration
		wantSeconds int64
	}{
		{name: "the apply identity at 30 days", id: deployercred.IdentityApply, lifetime: applyMax, wantSeconds: 2592000},
		{name: "the policy identity at one day", id: deployercred.IdentityPolicy, lifetime: policyMax, wantSeconds: 86400},
		{name: "the shortest lifetime", id: deployercred.IdentityApply, lifetime: time.Hour, wantSeconds: 3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, csrYAML, err := deployercred.NewRequest(tc.id, tc.lifetime, &detRand{})
			if err != nil {
				t.Fatalf("NewRequest refused a permitted lifetime: %v", err)
			}
			var doc csrDoc
			if err := yaml.Unmarshal(csrYAML, &doc); err != nil {
				t.Fatalf("the object does not parse: %v", err)
			}
			if doc.APIVersion != csrVersion || doc.Kind != "CertificateSigningRequest" {
				t.Fatalf("the object is %s %s, want %s CertificateSigningRequest", doc.APIVersion, doc.Kind, csrVersion)
			}
			if doc.Metadata.Name != string(tc.id) {
				t.Fatalf("the object name is %q, want %q", doc.Metadata.Name, tc.id)
			}
			if doc.Spec.SignerName != csrSigner {
				t.Fatalf("the signer is %q, want %q", doc.Spec.SignerName, csrSigner)
			}
			if len(doc.Spec.Usages) != 1 || doc.Spec.Usages[0] != "client auth" {
				t.Fatalf("the usages are %q, want [client auth]", doc.Spec.Usages)
			}
			if doc.Spec.ExpirationSeconds != tc.wantSeconds {
				t.Fatalf("the expiration is %d seconds, want %d", doc.Spec.ExpirationSeconds, tc.wantSeconds)
			}
			parseRequest(t, doc.Spec.Request)
		})
	}
}
