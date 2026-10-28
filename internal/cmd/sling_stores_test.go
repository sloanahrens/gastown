package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// slingFake is a fake database as sling's stores see it. The molecule detach
// is the beads free function over it, which clears the attachment fields the
// way the store does (the audit log is not modeled).
type slingFake struct {
	*beadsfake.Fake
}

// fakeSlingStores answers every pinned and routed store from db.
func fakeSlingStores(db *beadsfake.Fake) slingStores {
	store := slingFake{db}
	return slingStores{
		pinned: func(string) slingStore { return store },
		routed: func(string) slingStore { return store },
	}
}

// dirSlingStores answers the pinned store of each beads directory in dbs
// from its database, and routed reads from routed; any other directory has
// an empty database.
func dirSlingStores(dbs map[string]*beadsfake.Fake, routed *beadsfake.Fake) slingStores {
	return slingStores{
		pinned: func(beadsDir string) slingStore {
			if db, ok := dbs[beadsDir]; ok {
				return slingFake{db}
			}
			return slingFake{beadsfake.New()}
		},
		routed: func(string) slingStore { return slingFake{routed} },
	}
}

// TestRecordBlockerOverrideWritesTheComment: the --ignore-blockers record is
// a comment on the bead, authored by the actor and naming the blockers, so a
// person reading the bead later can see who dispatched over what (gt-e3hl2).
func TestRecordBlockerOverrideWritesTheComment(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	issue, err := db.Create(beads.CreateOptions{Title: "Blocked work"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	err = fakeSlingStores(db).recordBlockerOverride("/town", issue.ID, "gastown/crew/sloan",
		[]string{"gt-block (open)", "gt-later (deferred)"})
	if err != nil {
		t.Fatalf("recordBlockerOverride: %v", err)
	}
	comments, err := db.Comments(issue.ID)
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %v, want exactly the override record", comments)
	}
	got := comments[0]
	if got.Author != "gastown/crew/sloan" {
		t.Errorf("author = %q, want the actor", got.Author)
	}
	for _, want := range []string{"--ignore-blockers", "gastown/crew/sloan", "gt-block (open)", "gt-later (deferred)"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("comment %q does not name %q", got.Text, want)
		}
	}
}

// TestRecordBlockerOverrideReturnsAWriteFailure: the record is the condition
// the override is granted on, so a failed write is an error naming the bead,
// never a silent dispatch (the caller refuses the sling on it).
func TestRecordBlockerOverrideReturnsAWriteFailure(t *testing.T) {
	t.Parallel()
	err := fakeSlingStores(beadsfake.New()).recordBlockerOverride("/town", "gt-gone", "gastown/crew/sloan",
		[]string{"gt-block (open)"})
	if err == nil {
		t.Fatal("recordBlockerOverride = nil error for a bead the store does not hold")
	}
	for _, want := range []string{"--ignore-blockers", "gt-gone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}
