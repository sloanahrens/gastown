package config

import (
	"strings"
	"testing"
)

// A kubernetes profile names the namespace its sandbox pods run in, defaulting
// to the OpenShell chart's namespace; a docker profile names none, and the
// value must be a DNS-1123 label (gt-1gtq5g).
func TestSandboxProfileKubernetesNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		profile       SandboxProfile
		wantErr       string // a substring the error must carry; "" means valid
		wantNamespace string // PodNamespace() when the profile validates
	}{
		{
			name:          "an unset namespace on a kubernetes profile is the OpenShell chart's namespace",
			profile:       SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", Driver: SandboxDriverKubernetes},
			wantNamespace: "openshell",
		},
		{
			name:          "a valid DNS-1123 namespace on a kubernetes profile validates and is the pod namespace",
			profile:       SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", Driver: SandboxDriverKubernetes, KubernetesNamespace: "sandboxes-prod"},
			wantNamespace: "sandboxes-prod",
		},
		{
			name:    "an uppercase namespace fails and names the field",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", Driver: SandboxDriverKubernetes, KubernetesNamespace: "OpenShell"},
			wantErr: "kubernetes_namespace",
		},
		{
			name:    "a namespace longer than 63 characters fails and names the field",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", Driver: SandboxDriverKubernetes, KubernetesNamespace: strings.Repeat("a", 64)},
			wantErr: "kubernetes_namespace",
		},
		{
			name:    "a namespace on a docker profile fails",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", Driver: SandboxDriverDocker, KubernetesNamespace: "openshell"},
			wantErr: "kubernetes_namespace",
		},
		{
			name:    "a namespace on an unset-driver profile fails",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Gateway: "k8s", KubernetesNamespace: "openshell"},
			wantErr: "kubernetes_namespace",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.profile.Validate("gastown")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate = %v, want an error naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := tc.profile.PodNamespace(); got != tc.wantNamespace {
				t.Errorf("PodNamespace() = %q, want %q", got, tc.wantNamespace)
			}
		})
	}
}
