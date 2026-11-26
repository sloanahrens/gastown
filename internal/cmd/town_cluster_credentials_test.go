package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sbxcreds"
)

// TestClusterCredentialsRequestRoles fixes the client name and group of each role on gt-sbx, and that
// sbxcreds accepts the request.
func TestClusterCredentialsRequestRoles(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"read", "policy", "apply"} {
		got, err := credentialRequest(clusterSbx, role, "kubeconfig.yaml")
		if err != nil {
			t.Fatalf("credentialRequest(%s, %s): %v", clusterSbx, role, err)
		}
		want := "gt-deployer-" + role
		if got.ClientName != want || got.Group != want {
			t.Errorf("role %s = client %q group %q, want %q for both", role, got.ClientName, got.Group, want)
		}
		if _, err := got.Argv(); err != nil {
			t.Errorf("role %s: sbxcreds refuses the request: %v", role, err)
		}
	}
}

// TestClusterCredentialsRequestRefuses drives each refusal and asserts the message names what is allowed.
func TestClusterCredentialsRequestRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, cluster, role, out string
		want                     []string
	}{
		{"a wrong cluster", "control", "apply", "kubeconfig.yaml", []string{"gt-sbx"}},
		{"an empty cluster", "", "apply", "kubeconfig.yaml", []string{"gt-sbx"}},
		{"a missing role", clusterSbx, "", "kubeconfig.yaml", []string{"missing", "read, policy, apply"}},
		{"an unknown role", clusterSbx, "admin", "kubeconfig.yaml", []string{"unknown", "read, policy, apply"}},
		{"a role in the wrong case", clusterSbx, "Apply", "kubeconfig.yaml", []string{"unknown", "read, policy, apply"}},
		{"a missing out", clusterSbx, "apply", "", []string{"missing", "out"}},
	} {
		got, err := credentialRequest(tc.cluster, tc.role, tc.out)
		if err == nil {
			t.Errorf("%s: credentialRequest() = %+v, want a refusal", tc.name, got)
			continue
		}
		if got != (sbxcreds.Request{}) {
			t.Errorf("%s: request %+v returned beside error %v, want none", tc.name, got, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q does not name %q", tc.name, err, w)
			}
		}
	}
}
