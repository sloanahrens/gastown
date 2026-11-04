package daemon

import "testing"

// TestDoltConfigArchiveLevelZeroWhenAutoGCOn: with auto gc on, the config the
// daemon writes keeps auto gc enabled and archives at level 0, the same level
// the maintenance gc passes (gt-k3u2e).
func TestDoltConfigArchiveLevelZeroWhenAutoGCOn(t *testing.T) {
	t.Parallel()
	got := writeAndReadDaemonAutoGC(t, `{"auto_gc":"on"}`)
	if !got.Enable {
		t.Fatalf("auto_gc_behavior.enable = false, want true")
	}
	if got.ArchiveLevel != 0 {
		t.Fatalf("auto_gc_behavior.archive_level = %d, want 0", got.ArchiveLevel)
	}
}
