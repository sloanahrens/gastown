package config

import (
	"strings"
	"testing"
)

// A kubernetes profile may name a shared cache claim the driver mounts at
// /sandbox/.cache; a docker or unset-driver profile may not, since the Docker
// cache path is closed (gt-jofne).
func TestSandboxProfileKubernetesCacheClaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		profile SandboxProfile
		wantErr string // a substring the error must carry; "" means valid
	}{
		{
			name:    "a valid DNS-1123 claim on a kubernetes profile validates",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Driver: SandboxDriverKubernetes, KubernetesCacheClaim: "gt-shared-cache"},
		},
		{
			name:    "no claim on a kubernetes profile is no cache mount, as today",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Driver: SandboxDriverKubernetes},
		},
		{
			name:    "an uppercase name fails and names the field",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Driver: SandboxDriverKubernetes, KubernetesCacheClaim: "Gt_Cache"},
			wantErr: "kubernetes_cache_claim",
		},
		{
			name:    "a name longer than 63 characters fails and names the field",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Driver: SandboxDriverKubernetes, KubernetesCacheClaim: strings.Repeat("a", 64)},
			wantErr: "kubernetes_cache_claim",
		},
		{
			name:    "a claim on a docker profile fails",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Driver: SandboxDriverDocker, KubernetesCacheClaim: "gt-shared-cache"},
			wantErr: "kubernetes_cache_claim",
		},
		{
			name:    "a claim on an unset-driver profile fails",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", KubernetesCacheClaim: "gt-shared-cache"},
			wantErr: "kubernetes_cache_claim",
		},
		{
			name:    "cache_volume on a kubernetes profile still fails",
			profile: SandboxProfile{Image: "img", Policy: "p.yaml", Driver: SandboxDriverKubernetes, CacheVolume: "{rig}"},
			wantErr: "cache_volume",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.profile.Validate("gastown")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}
