package beadversions

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/metrics"
)

// archiveActor is the actor recorded for a text imported from the archive's
// first-filed row.
const archiveActor = "archive"

// FiledText returns a bead's first-filed text as one string: the description,
// a blank line, then the acceptance criteria.
func FiledText(row metrics.ArchiveIssueRow) string {
	return row.Description + "\n\n" + row.AcceptanceCriteria
}

// ImportFirstFiled records each row as the bead's version at its created_at
// under the archive actor, and returns how many it recorded and how many the
// store already held.
//
// It records every row it is given, so the caller must not pass a row whose
// text may not be the text as filed: metrics.FirstFiled.EditedSameDay marks a
// row first carried by a commit on its own filed day, which may already hold
// an edit, and the command leaves those out.
func ImportFirstFiled(store Store, first []metrics.FirstFiled, text func(metrics.ArchiveIssueRow) string) (put, skipped int, err error) {
	if store == nil {
		return 0, 0, errors.New("beadversions: import with no store")
	}
	if text == nil {
		return 0, 0, errors.New("beadversions: import with no text function")
	}
	for _, f := range first {
		bead := f.Row.ID
		versions, err := store.Versions(bead)
		if err != nil {
			return put, skipped, fmt.Errorf("importing bead %s: %w", bead, err)
		}
		t := text(f.Row)
		if len(versions) > 0 && versions[len(versions)-1].Text == t {
			// A second import of the same rows lands here: the store already
			// ends with this text, and Put would refuse the row's time as not
			// after the latest, so recording it again would only be an error.
			skipped++
			continue
		}
		recorded, err := store.Put(bead, t, archiveActor, f.Row.CreatedAt)
		if err != nil {
			return put, skipped, fmt.Errorf("importing bead %s: %w", bead, err)
		}
		if !recorded {
			skipped++
			continue
		}
		put++
	}
	return put, skipped, nil
}
