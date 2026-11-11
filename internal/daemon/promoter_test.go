package daemon

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestNewRigPromoterResolvesKeyFileThroughConfigHome: gt promote and the
// daemon's owners both build their promotion through newRigPromoter, so the
// key path it carries must be the resolved one. The environment and the
// file-existence reads are fakes, and a nil logf proves the owner builds
// without a log sink.
func TestNewRigPromoterResolvesKeyFileThroughConfigHome(t *testing.T) {
	t.Parallel()
	const configHome = "/run/gt/xdg"
	configured := filepath.Join("/mac", "rig", "promote-rig.key")
	mounted := filepath.Join(configHome, "gt", "promote-rig.key")
	getenv := func(name string) string {
		if name == "XDG_CONFIG_HOME" {
			return configHome
		}
		return ""
	}
	stat := func(path string) error {
		if path == mounted {
			return nil
		}
		return &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	fj := &config.ForgejoConfig{PromoteTarget: "git@github.com:org/rig.git", PromoteKeyFile: configured}

	p := newRigPromoter("/town", "rig", fj, nil, nil, nil, getenv, stat)
	if p == nil {
		t.Fatal("newRigPromoter = nil, want a promoter for a rig with a target and a key")
	}
	if p.KeyFile != mounted {
		t.Errorf("KeyFile = %q, want the config-home key %q", p.KeyFile, mounted)
	}
}
