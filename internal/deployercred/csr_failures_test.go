package deployercred

import (
	"crypto/rand"
	"crypto/x509"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestDeployerCredRequestFailurePaths drives each call newRequest makes that
// can fail. The standard library calls do not fail for this package's fixed
// key, template and object, so the seam is what makes the branches reachable.
func TestDeployerCredRequestFailurePaths(t *testing.T) {
	t.Parallel()
	boom := errors.New("injected failure")
	failKey := func(any) ([]byte, error) { return nil, boom }
	failRequest := func(io.Reader, *x509.CertificateRequest, any) ([]byte, error) { return nil, boom }
	failObject := func(any) ([]byte, error) { return nil, boom }

	cases := []struct {
		name          string
		marshalKey    func(any) ([]byte, error)
		createCSR     func(io.Reader, *x509.CertificateRequest, any) ([]byte, error)
		marshalObject func(any) ([]byte, error)
		wantErr       string
	}{
		{
			name:       "the key marshal fails",
			marshalKey: failKey, createCSR: x509.CreateCertificateRequest, marshalObject: yaml.Marshal,
			wantErr: "marshal the gt-deployer-apply client key: injected failure",
		},
		{
			name:       "the request fails",
			marshalKey: x509.MarshalPKCS8PrivateKey, createCSR: failRequest, marshalObject: yaml.Marshal,
			wantErr: "create the gt-deployer-apply signing request: injected failure",
		},
		{
			name:       "the object marshal fails",
			marshalKey: x509.MarshalPKCS8PrivateKey, createCSR: x509.CreateCertificateRequest, marshalObject: failObject,
			wantErr: "marshal the gt-deployer-apply signing request object: injected failure",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyPEM, csrYAML, err := newRequest(IdentityApply, 30*24*time.Hour, rand.Reader, tc.marshalKey, tc.createCSR, tc.marshalObject)
			if err == nil {
				t.Fatalf("newRequest accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("the error %q does not say %q", err, tc.wantErr)
			}
			if keyPEM != nil || csrYAML != nil {
				t.Fatalf("the failure returned output: key %q, object %q", keyPEM, csrYAML)
			}
		})
	}
}
