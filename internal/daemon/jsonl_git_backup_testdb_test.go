package daemon

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testdb"
)

// The backup's test-pollution filters derive from internal/testdb, so every
// prefix in the one list is scrubbed, including the ones the old copies
// missed (gt-fcxe9.9, deep review B2-03).
func TestBackupScrubsEveryTestDatabasePrefix(t *testing.T) {
	t.Parallel()
	pat := testDatabaseIDPattern()
	for _, p := range testdb.Prefixes() {
		id := p + "abc-1"
		if !pat.MatchString(id) {
			t.Errorf("testDatabaseIDPattern does not match %q", id)
		}
		term := `id NOT LIKE '` + strings.ReplaceAll(p, "_", `\\_`) + `%'`
		if q := backupIssuesQuery("hq", true).String(); !strings.Contains(q, term) {
			t.Errorf("scrubbed issues query lacks %s: %s", term, q)
		}
	}
	for _, id := range []string{"gt-abc", "hq-1", "beads-xyz"} {
		if pat.MatchString(id) {
			t.Errorf("testDatabaseIDPattern matches production id %q", id)
		}
	}
}
