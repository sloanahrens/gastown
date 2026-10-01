package cmd

import (
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// slingFake is a fake database as sling's stores see it: the fake plus the
// audited molecule detach, which clears the attachment fields the way
// *beads.Beads does (the audit log is not modeled).
type slingFake struct {
	*beadsfake.Fake
}

func (f slingFake) DetachMoleculeWithAudit(id string, _ beads.DetachOptions) (*beads.Issue, error) {
	issue, err := f.Show(id)
	if err != nil {
		return nil, err
	}
	if beads.ParseAttachmentFields(issue) == nil {
		return issue, nil
	}
	desc := beads.SetAttachmentFields(issue, nil)
	if err := f.Update(id, beads.UpdateOptions{Description: &desc}); err != nil {
		return nil, err
	}
	return f.Show(id)
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
