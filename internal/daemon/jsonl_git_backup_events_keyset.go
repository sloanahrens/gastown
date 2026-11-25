package daemon

import (
	"encoding/json"
	"fmt"
)

// eventsPageKeyset reads a page's keyset from its last row: the created_at and
// id BackupEventsPage starts the next page after. A row that is not an exported
// events row is refused rather than read as an empty keyset, which would restart
// the paged read at the first row.
func eventsPageKeyset(row json.RawMessage) (createdAt, id string, err error) {
	parsed, err := parseEventRow(string(row))
	if err != nil {
		return "", "", fmt.Errorf("the page's last row: %w", err)
	}
	return parsed.createdAt, parsed.id, nil
}
