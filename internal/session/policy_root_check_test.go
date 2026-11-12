package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// policyRootCheckCheckout builds a gt source checkout under t.TempDir() that
// holds the policy file at each rel path, and returns its directory.
func policyRootCheckCheckout(t *testing.T, present ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range present {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("policy"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPolicyRootCheckNamesTheProfileAndPath(t *testing.T) {
	t.Parallel()
	const rel = "deploy/polecat/policy.yaml"
	cases := []struct {
		name     string
		present  []string
		profiles map[string]config.SandboxProfile
		// wantErr names what the refusal must carry; "" means the check passes.
		wantErr []string
	}{
		{
			name:     "a town with no profiles has no policy to check",
			profiles: nil,
		},
		{
			name:     "the profile's policy is in the checkout",
			present:  []string{rel},
			profiles: map[string]config.SandboxProfile{"polecat": {Policy: rel}},
		},
		{
			name:     "the profile's policy is missing from the checkout",
			profiles: map[string]config.SandboxProfile{"polecat": {Policy: rel}},
			wantErr:  []string{"sandbox profile polecat", "the policy", rel},
		},
		{
			name:     "a policy path that climbs out of the checkout is refused by name",
			present:  []string{rel},
			profiles: map[string]config.SandboxProfile{"escape": {Policy: "../" + rel}},
			wantErr:  []string{"sandbox profile escape", "stay inside it"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			checkout := policyRootCheckCheckout(t, c.present...)
			err := CheckPolicyFiles(checkout, c.profiles)
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("CheckPolicyFiles: %v, want a pass", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CheckPolicyFiles passed, want a refusal")
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
		})
	}
}

// The sling call site with no gt source checkout under the town: createSpec
// refuses with the checkout's own error and resolves no policy path. The pass
// case, a checkout that holds the policy, is TestOpenShellProductionPolicyRootIsTheTownCheckout.
func TestPolicyRootCheckAtTheSlingCallSiteWithoutACheckout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	work := filepath.Join(town, openshellTestRig, "polecats", openshellTestPolecat, openshellTestRig)
	b, err := newOpenShellBackend(town, openshellTestProfile)
	if err != nil {
		t.Fatalf("newOpenShellBackend: %v", err)
	}
	_, err = b.(*openshellBackend).createSpec(openshellSpawn(town, work), sandboxTracker{})
	if err == nil || !strings.Contains(err.Error(), "no gt source checkout") {
		t.Fatalf("createSpec = %v, want the no-checkout refusal", err)
	}
}
