//go:build !windows

package feed

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

func TestCurator_FeedFilePermissions(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	feedPath := filepath.Join(tmpDir, FeedFile)

	curator := NewCurator(tmpDir)

	curator.writeFeedEvent(&events.Event{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Source:     "gt",
		Type:       events.TypeDone,
		Actor:      "test-actor",
		Payload:    map[string]interface{}{"bead": "test"},
		Visibility: events.VisibilityFeed,
	})

	info, err := os.Stat(feedPath)
	if err != nil {
		t.Fatalf("feed file not created: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("feed file permissions = %o, want 0600", perm)
	}
}
