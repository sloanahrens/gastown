// Package deployercred builds the deployer's cluster credentials: the client
// key and the CertificateSigningRequest the break-glass operator approves
// (gt-660bl8). It writes no file; the caller owns the key bytes.
package deployercred

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

// Identity is the deployer role a client certificate names: the subject's
// common name is this value, and the cluster binds its permissions to it.
type Identity string

const (
	// IdentityApply is the workload writer: namespaced workload kinds in the
	// allowlisted namespace, and nothing that grants permission.
	IdentityApply Identity = "gt-deployer-apply"
	// IdentityPolicy is the break-glass policy writer, issued per use because
	// its role can bind cluster-admin to itself (gt-924m6x).
	IdentityPolicy Identity = "gt-deployer-policy"

	// organization is the subject organization both certificates carry; one
	// group binding gives both identities the deployer read grant.
	organization = "gt-deployers"

	// signerName is what the request asks the control plane's client-
	// certificate signer for: the issued certificate serves only as a client.
	signerName = "kubernetes.io/kube-apiserver-client"

	// minLifetime is the shortest certificate issued: a request for less is a
	// caller mistake, not a credential.
	minLifetime = time.Hour
)

// csrObject is the CertificateSigningRequest as it is submitted to the API
// server; only the fields the deployer's request needs are set.
type csrObject struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   csrMetadata `yaml:"metadata"`
	Spec       csrSpec     `yaml:"spec"`
}

type csrMetadata struct {
	Name string `yaml:"name"`
}

type csrSpec struct {
	Request           string   `yaml:"request"`
	SignerName        string   `yaml:"signerName"`
	Usages            []string `yaml:"usages"`
	ExpirationSeconds int64    `yaml:"expirationSeconds"`
}

// MaxLifetime is the longest lifetime a certificate for id may be issued for:
// 30 days for the apply identity, one day for policy (gt-660bl8). An unknown
// identity is an error, never a zero lifetime a caller could print as
// unlimited.
func MaxLifetime(id Identity) (time.Duration, error) {
	switch id {
	case IdentityApply:
		return 30 * 24 * time.Hour, nil
	case IdentityPolicy:
		return 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("the deployer identity is %q, not %q or %q", id, IdentityApply, IdentityPolicy)
}

// NewRequest generates a P-256 client key (PKCS8 PEM) and the
// CertificateSigningRequest object (certificates.k8s.io/v1) that signs it for
// id with the lifetime asked for.
func NewRequest(id Identity, lifetime time.Duration, rand io.Reader) (keyPEM, csrYAML []byte, err error) {
	return newRequest(id, lifetime, rand, x509.MarshalPKCS8PrivateKey, x509.CreateCertificateRequest, yaml.Marshal)
}

// newRequest is NewRequest over the three calls that can fail, so a test can
// drive each failure path.
func newRequest(id Identity, lifetime time.Duration, rand io.Reader, marshalKey func(any) ([]byte, error), createCSR func(io.Reader, *x509.CertificateRequest, any) ([]byte, error), marshalObject func(any) ([]byte, error)) (keyPEM, csrYAML []byte, err error) {
	max, err := MaxLifetime(id)
	if err != nil {
		return nil, nil, err
	}
	if lifetime > max {
		return nil, nil, fmt.Errorf("a %s certificate lives at most %s, not %s", id, max, lifetime)
	}
	if lifetime < minLifetime {
		return nil, nil, fmt.Errorf("a %s certificate lives at least %s, not %s", id, minLifetime, lifetime)
	}
	if lifetime%time.Second != 0 {
		return nil, nil, fmt.Errorf("the lifetime %s is not a whole number of seconds", lifetime)
	}
	if rand == nil {
		return nil, nil, fmt.Errorf("the random reader is nil, so no %s key can be generated", id)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand)
	if err != nil {
		return nil, nil, fmt.Errorf("generate the %s client key: %w", id, err)
	}
	keyDER, err := marshalKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal the %s client key: %w", id, err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// The subject carries the identity as the common name and gt-deployers as
	// the organization; the role bindings name exactly those.
	csrDER, err := createCSR(rand, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: string(id), Organization: []string{organization}},
	}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create the %s signing request: %w", id, err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// The request is the PEM CSR in the base64 the API server's []byte field
	// decodes. The name is the identity, so a reissue submits the same name and
	// the operator deletes the issued request first (gt-negjz0).
	csrYAML, err = marshalObject(csrObject{
		APIVersion: "certificates.k8s.io/v1",
		Kind:       "CertificateSigningRequest",
		Metadata:   csrMetadata{Name: string(id)},
		Spec: csrSpec{
			Request:           base64.StdEncoding.EncodeToString(csrPEM),
			SignerName:        signerName,
			Usages:            []string{"client auth"},
			ExpirationSeconds: int64(lifetime / time.Second),
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal the %s signing request object: %w", id, err)
	}
	return keyPEM, csrYAML, nil
}
