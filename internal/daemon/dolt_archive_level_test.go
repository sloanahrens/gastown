package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// TestDoltConfigArchiveLevelZeroWhenAutoGCOn: with auto gc on, the config the
// daemon writes carries archive_level 0 in the file itself. Reading the value
// back through the YAML decoder cannot tell a written zero from a missing key,
// and Dolt's own default for a missing key is not 0 (gt-k3u2e).
func TestDoltConfigArchiveLevelZeroWhenAutoGCOn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &DoltServerConfig{Port: 3307, DataDir: dir}
	if err := writeDaemonDoltConfig(cfg, configPath, doltserver.DefaultConfig(dir)); err != nil {
		t.Fatalf("writeDaemonDoltConfig: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading daemon Dolt config: %v", err)
	}
	const want = "auto_gc_behavior:\n    enable: true\n    archive_level: 0"
	if !strings.Contains(string(data), want) {
		t.Fatalf("generated daemon config lacks %q:\n%s", want, data)
	}
}
