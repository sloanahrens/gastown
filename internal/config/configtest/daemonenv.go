// Package configtest holds test fixtures for the town's config files. It is a
// leaf package with no gastown imports, so a config test and the packages that
// read the same file can all use it.
package configtest

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteDaemonEnv writes settings/daemon.env under townRoot with body, at mode
// 0600 as the loader expects. The path is config.DaemonEnvPath's.
func WriteDaemonEnv(t testing.TB, townRoot, body string) {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
