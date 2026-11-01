package doltserver

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bdTableWrite matches DML or DDL aimed at a table bd owns. gastown reaches
// beads only through bd verbs (ADR 0001, gt-fcxe9.12): a raw write skips bd's
// is_blocked maintenance, the events journal and bd's own commits, and DDL
// makes gastown a second schema owner (B5-06, B5-16).
var bdTableWrite = regexp.MustCompile(`\b(INSERT\s+(IGNORE\s+)?INTO|REPLACE\s+INTO|UPDATE|DELETE\s+FROM|CREATE\s+TABLE(\s+IF\s+NOT\s+EXISTS)?|DROP\s+TABLE(\s+IF\s+EXISTS)?|ALTER\s+TABLE)\s+` +
	"(`?[%\\w]+`?\\.)?`?" +
	`(issues|wisps|labels|wisp_labels|comments|wisp_comments|events|wisp_events|dependencies|wisp_dependencies|config)\b`)

// allowedBdTableWrites is the complete set of source lines that carry the only
// DML gastown may send to a bd table: the events retention delete
// (reaper.PurgeEvents, gt-hk21d). A line is exempt only when, trimmed, it is
// exactly one of these, so a changed statement — another table, no LIMIT, or a
// second statement sharing the line — is still refused.
var allowedBdTableWrites = []string{
	`const deleteEventsBefore = "DELETE FROM events WHERE created_at < ? ORDER BY created_at LIMIT ?"`,
}

// bdTableWriteAllowed reports whether line is exactly one of
// allowedBdTableWrites.
func bdTableWriteAllowed(line string) bool {
	trimmed := strings.TrimSpace(line)
	for _, allowed := range allowedBdTableWrites {
		if trimmed == allowed {
			return true
		}
	}
	return false
}

// TestNoGoSourceWritesBdTables scans every non-test Go file under internal/
// for a write to a bd table.
func TestNoGoSourceWritesBdTables(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := bdTableWrite.FindString(line); m != "" && !bdTableWriteAllowed(line) {
				t.Errorf("%s:%d writes a bd table (%s): %s", path, i+1, m, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBdTableWriteAllowedIsTheEventsPurgeStatement: the exemption admits the
// events purge statement and nothing that differs from it, so it cannot be
// widened into a delete of another table, one without a LIMIT, or a line that
// carries a second statement beside it.
func TestBdTableWriteAllowedIsTheEventsPurgeStatement(t *testing.T) {
	t.Parallel()
	allowed := allowedBdTableWrites[0]
	if !bdTableWrite.MatchString(allowed) {
		t.Fatalf("the allowed statement is not one the guard would flag, so the exemption is dead: %s", allowed)
	}
	for _, line := range []string{allowed, "  " + allowed + "  "} {
		if !bdTableWriteAllowed(line) {
			t.Errorf("bdTableWriteAllowed(%q) = false, want true", line)
		}
	}
	for _, line := range []string{
		`deleteEventsBefore = "DELETE FROM events WHERE created_at < ?"`,
		`deleteEventsBefore = "DELETE FROM events WHERE created_at < ? ORDER BY created_at LIMIT 10"`,
		`deleteEventsBefore = "DELETE FROM wisp_events WHERE created_at < ? ORDER BY created_at LIMIT ?"`,
		`deleteEventsBefore = "DELETE FROM bd_events_journal WHERE created_at < ? ORDER BY created_at LIMIT ?"`,
		allowed + `; db.Exec("DELETE FROM wisp_events WHERE x")`,
	} {
		if bdTableWriteAllowed(line) {
			t.Errorf("bdTableWriteAllowed(%q) = true, want false", line)
		}
	}
}

func TestBdTableWritePattern(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"UPDATE `%s`.issues SET status = 'closed'",
		"DELETE FROM wisp_dependencies WHERE x",
		"INSERT IGNORE INTO labels (issue_id, label)",
		"REPLACE INTO config (`key`, `value`)",
		"CREATE TABLE wisps (",
		"DROP TABLE IF EXISTS wisp_dependencies",
	} {
		if !bdTableWrite.MatchString(q) {
			t.Errorf("pattern misses %q", q)
		}
	}
	for _, q := range []string{
		"SELECT id FROM issues",
		"UPDATE wanted SET claimed_by",
		"UPDATE dolt_rebase SET action = 'squash'",
		"could not update issues list",
	} {
		if bdTableWrite.MatchString(q) {
			t.Errorf("pattern flags %q", q)
		}
	}
}
