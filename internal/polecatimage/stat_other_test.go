//go:build !linux

package polecatimage

import (
	"testing"
	"time"
)

// TestNonLinuxStatFallbacks pins stat_other.go where the Linux-only trim tests
// skip: the free-space read refuses, naming the platform, and lastUsed answers
// the modification time, never a later access time (gt-qq5kv4).
func TestNonLinuxStatFallbacks(t *testing.T) {
	t.Parallel()
	want := "cannot measure the filesystem's free space on this platform"
	if _, err := filesystemFree(t.TempDir()); err == nil || err.Error() != want {
		t.Fatalf("filesystemFree = %v, want the refusal %q", err, want)
	}
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if got := lastUsed(writeWithTimes(t, base.Add(time.Hour), base)); !got.Equal(base) {
		t.Errorf("lastUsed = %v, want the modification time %v", got, base)
	}
}
