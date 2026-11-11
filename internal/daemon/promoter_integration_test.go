//go:build integration

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestIntegrationNewRigPromoterKeyFileFromRealEnvironment runs the exported constructor,
// the one gt promote and the landing worker call, against the real XDG_CONFIG_HOME
// and real files under t.TempDir(). promoter_test.go reaches only the
// unexported newRigPromoter with fakes, so this is what checks NewRigPromoter's
// own wiring of os.Getenv and the file-existence stat. It is tagged integration
// because it sets the process environment, which a unit test may not do.
func TestIntegrationNewRigPromoterKeyFileFromRealEnvironment(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	mounted := filepath.Join(configHome, "gt", "promote-rig.key")
	if err := os.MkdirAll(filepath.Dir(mounted), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mounted, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "mac", "promote-rig.key")
	present := filepath.Join(t.TempDir(), "promote-rig.key")
	if err := os.WriteFile(present, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		configured string
		want       string
	}{
		{name: "configured path missing resolves to the config-home key", configured: absent, want: mounted},
		{name: "configured path present is used unchanged", configured: present, want: present},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fj := &config.ForgejoConfig{PromoteTarget: "git@github.com:org/rig.git", PromoteKeyFile: tt.configured}
			p := NewRigPromoter("/town", "rig", fj, nil, nil, nil)
			if p == nil {
				t.Fatal("NewRigPromoter = nil, want a promoter for a rig with a target and a key")
			}
			if p.KeyFile != tt.want {
				t.Errorf("KeyFile = %q, want %q", p.KeyFile, tt.want)
			}
		})
	}
}
