package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// TestSpecBlockedMemoryPersists: a list one tick saves is answered by the next
// tick under the same owner and hash.
func TestSpecBlockedMemoryPersists(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "annotated.json")
	hash := beadsql.RigHash{Head: "h1", Working: "w1"}
	list := []*beads.Issue{{ID: "gt-a", Priority: 1, BlockedBy: []string{"gt-x"}}, {ID: "gt-b"}}

	first := loadSpecAnnotated(path, "daemon-1")
	first.noteRigBlocked("gt", hash, blockedAt, list)
	first.save()

	heads := map[string]beadsql.RigHash{"gt": hash}
	if got, ok := loadSpecAnnotated(path, "daemon-1").rigBlocked("gt", heads, blockedAt.Add(time.Minute)); !ok || !reflect.DeepEqual(got, list) {
		t.Errorf("rigBlocked after reload = %v, %v; want %v, true", got, ok, list)
	}
}

// TestSpecBlockedMemoryWithoutBlockedKey: a file with no blocked key loads its
// beads as before and answers no list.
func TestSpecBlockedMemoryWithoutBlockedKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "annotated.json")
	if err := os.WriteFile(path, []byte(`{"owner":"daemon-1","beads":{"gt-a":{"k":2}}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded := loadSpecAnnotated(path, "daemon-1")
	if loaded.seen["gt-a"]["k"] != 2 {
		t.Errorf("seen = %v; want the file's beads kept", loaded.seen)
	}
	heads := map[string]beadsql.RigHash{"gt": {Head: "h1", Working: "w1"}}
	if got, ok := loaded.rigBlocked("gt", heads, blockedAt); ok {
		t.Errorf("rigBlocked = %v, true; want nothing", got)
	}
}

// TestSpecBlockedMemoryForeignOwner: a list another daemon saved is not answered.
func TestSpecBlockedMemoryForeignOwner(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "annotated.json")
	hash := beadsql.RigHash{Head: "h1", Working: "w1"}

	owner := loadSpecAnnotated(path, "daemon-1")
	owner.noteRigBlocked("gt", hash, blockedAt, []*beads.Issue{{ID: "gt-a"}})
	owner.save()

	heads := map[string]beadsql.RigHash{"gt": hash}
	if got, ok := loadSpecAnnotated(path, "daemon-2").rigBlocked("gt", heads, blockedAt.Add(time.Minute)); ok {
		t.Errorf("rigBlocked for foreign owner = %v, true; want nothing", got)
	}
}
