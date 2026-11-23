package testutil

import (
	"os"
	"testing"
)

// PrivateTempDir returns a temporary directory only this account can write.
// t.TempDir is 0777 minus the process umask, so under an open umask a test of
// a mode check — or of a refusal of a group- or world-writable path — gets a
// directory that check would reject (gt-tyl0vp, gt-aq8dej). The one chmod
// lives here rather than in a test file, so no test carries a testpolicy
// allow annotation. A directory that cannot be made private fails the test.
func PrivateTempDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("make %s private: %v", dir, err)
	}
	return dir
}
