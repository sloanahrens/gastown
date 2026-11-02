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
// store already held. A row the store refuses does not stop the rest; the
// returned error names every refused row and still carries the counts reached.
//
// It records every row it is given, so the caller must not pass a row whose
// text may not be the text as filed: metrics.FirstFiled.AsFiled marks a row
// the archive shows unwritten since creation, and the command leaves the rest
// out.
func ImportFirstFiled(store Store, first []metrics.FirstFiled, text func(metrics.ArchiveIssueRow) string) (put, skipped int, err error) {
	if store == nil {
		return 0, 0, errors.New("beadversions: import with no store")
	}
	if text == nil {
		return 0, 0, errors.New("beadversions: import with no text function")
	}
	// One bead the store refuses must not cost the rest of the import: each row
	// stands alone, so the loop records what it can and returns every refusal at
	// the end with the counts it reached.
	var refused []error
	for _, f := range first {
		bead := f.Row.ID
		versions, err := store.Versions(bead)
		if err != nil {
			refused = append(refused, fmt.Errorf("importing bead %s: %w", bead, err))
			continue
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
			refused = append(refused, fmt.Errorf("importing bead %s: %w", bead, err))
			continue
		}
		if !recorded {
			skipped++
			continue
		}
		put++
	}
	return put, skipped, errors.Join(refused...)
}
