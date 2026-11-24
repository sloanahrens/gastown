package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// TestTickEffRigDatabases: a rig's database is the dolt_database its metadata
// file records, not its name; a rig whose metadata names none, does not parse,
// or is absent is left out.
func TestTickEffRigDatabases(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMetadata := func(rig, content string) {
		t.Helper()
		dir := filepath.Join(town, rig, ".beads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata("gastown", `{"dolt_database":"gt"}`)
	writeMetadata("beads", `{"dolt_database":"be"}`)
	writeMetadata("third", `{}`)
	writeMetadata("broken", `{"dolt_database":`)

	got := rigDatabases(town, []string{"gastown", "beads", "third", "broken", "absent"})
	want := map[string]string{"gastown": "gt", "beads": "be"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rigDatabases = %v, want %v", got, want)
	}
}

// TestTickEffRigHeads: the parsed rows are keyed by rig name, each rig's two
// hashes found through the database its metadata names — gastown reads gt and
// beads reads be, so keying by database would name neither rig — and a rig
// whose metadata names no database is keyed nowhere (gt-j3vdg4).
func TestTickEffRigHeads(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMetadata := func(rig, content string) {
		t.Helper()
		dir := filepath.Join(town, rig, ".beads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata("gastown", `{"dolt_database":"gt"}`)
	writeMetadata("beads", `{"dolt_database":"be"}`)
	writeMetadata("third", `{}`)

	read := sqlReaderFunc(func(beadsql.Query) ([][]string, error) {
		return [][]string{
			{"be_head", "gt_head", "be_working", "gt_working"},
			{"b1", "g1", "b2", "g2"},
		}, nil
	})

	got, err := rigHeads(town, []string{"gastown", "beads", "third"}, read)
	if err != nil {
		t.Fatalf("rigHeads: %v", err)
	}
	want := map[string]beadsql.RigHash{
		"gastown": {Head: "g1", Working: "g2"},
		"beads":   {Head: "b1", Working: "b2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rigHeads = %v, want %v", got, want)
	}
	if _, ok := got["third"]; ok {
		t.Error("rigHeads keyed a rig whose metadata names no database")
	}
	if _, ok := got["gt"]; ok {
		t.Error("rigHeads keyed a database name as a rig")
	}
}

// TestTickEffRigHeadsQuery: the statement is beadsql.RigHeads' for the
// databases in the order given, and a list RigHeads refuses keeps its refusal.
func TestTickEffRigHeadsQuery(t *testing.T) {
	t.Parallel()
	got, err := rigHeadsQuery([]string{"om", "gt"}).Inline()
	if err != nil {
		t.Fatalf("rigHeadsQuery: %v", err)
	}
	want, err := beadsql.RigHeads([]string{"om", "gt"}).Inline()
	if err != nil {
		t.Fatalf("beadsql.RigHeads: %v", err)
	}
	if got != want {
		t.Errorf("rigHeadsQuery = %q, want beadsql.RigHeads' %q", got, want)
	}
	if om, gt := strings.Index(got, "om_head"), strings.Index(got, "gt_head"); om < 0 || gt < 0 || om > gt {
		t.Errorf("rigHeadsQuery names om at %d and gt at %d, want om first:\n%s", om, gt, got)
	}
	bad := []struct {
		name string
		dbs  []string
	}{
		{"unsafe name", []string{"gt", "x`; DROP TABLE issues"}},
		{"no database", nil},
	}
	for _, tc := range bad {
		if _, err := rigHeadsQuery(tc.dbs).Inline(); err == nil {
			t.Errorf("%s: rigHeadsQuery = <nil>, want beadsql's refusal", tc.name)
		}
	}
}

// TestTickEffRigHeadsOneCall: every named rig's database is asked for in one
// SQLCSV round trip carrying the statement rigHeadsQuery builds, so a rig costs
// no call of its own.
func TestTickEffRigHeadsOneCall(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMetadata := func(rig, content string) {
		t.Helper()
		dir := filepath.Join(town, rig, ".beads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata("gastown", `{"dolt_database":"gt"}`)
	writeMetadata("beads", `{"dolt_database":"be"}`)
	writeMetadata("third", `{}`)

	header := []string{"be_head", "be_working", "gt_head", "gt_working"}
	row := []string{"b1", "b2", "g1", "g2"}
	calls := 0
	var sent beadsql.Query
	read := sqlReaderFunc(func(q beadsql.Query) ([][]string, error) {
		calls++
		sent = q
		if _, err := q.Inline(); err != nil {
			return nil, err
		}
		return [][]string{header, row}, nil
	})

	got, err := rigHeads(town, []string{"gastown", "beads", "third"}, read)
	if err != nil {
		t.Fatalf("rigHeads: %v", err)
	}
	if calls != 1 {
		t.Errorf("SQLCSV calls = %d, want one for every rig", calls)
	}
	wantQuery, err := rigHeadsQuery([]string{"be", "gt"}).Inline()
	if err != nil {
		t.Fatalf("rigHeadsQuery: %v", err)
	}
	if query, err := sent.Inline(); err != nil || query != wantQuery {
		t.Errorf("statement = %q, %v, want rigHeadsQuery's %q", query, err, wantQuery)
	}
	if want := map[string]beadsql.RigHash{
		"beads":   {Head: "b1", Working: "b2"},
		"gastown": {Head: "g1", Working: "g2"},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("rigHeads rows = %v, want %v", got, want)
	}

	// A rig set naming no database has no statement to send: the refusal is
	// returned, never an empty answer.
	if _, err := rigHeads(town, []string{"third"}, read); err == nil {
		t.Error("no database to ask: err = nil, want beadsql's refusal")
	}
}

// TestTickEffRigHeadsUnknownDatabase: a database the server does not know —
// Dolt's 1105 "Unknown system variable '@@<database>_head'" — is dropped by its
// exact name and the statement sent once more without it, so its rig is absent
// and the other rigs are answered (gt-a8ak44). The refusal's words are matched
// case-insensitively; a name that only starts with one of the databases asked
// for or goes on past its variable, a 1105 that names no database asked for,
// that same text under another error code or a longer one that starts with
// 1105, and a failure that is no such refusal keep their error and cost no
// second call. A refusal that leaves no database to ask is returned too, never
// an empty answer.
func TestTickEffRigHeadsUnknownDatabase(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMetadata := func(rig, content string) {
		t.Helper()
		dir := filepath.Join(town, rig, ".beads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata("gastown", `{"dolt_database":"gt"}`)
	writeMetadata("second", `{"dolt_database":"be"}`)
	rigs := []string{"gastown", "second"}

	// reads refuses the first SQLCSV call with refusal and answers any later one
	// with database's two hashes, keeping every statement sent.
	reads := func(refusal, database, head, working string) (*[]beadsql.Query, sqlReaderFunc) {
		sent := []beadsql.Query{}
		return &sent, func(q beadsql.Query) ([][]string, error) {
			sent = append(sent, q)
			if len(sent) == 1 {
				return nil, errors.New(refusal)
			}
			return [][]string{
				{database + "_head", database + "_working"},
				{head, working},
			}, nil
		}
	}

	t.Run("dropped by exact name", func(t *testing.T) {
		t.Parallel()
		sent, read := reads(`Error 1105 (HY000): Unknown system variable '@@gt_head'`, "be", "b1", "b2")
		got, err := rigHeads(town, rigs, read)
		if err != nil {
			t.Fatalf("rigHeads: %v", err)
		}
		want := map[string]beadsql.RigHash{"second": {Head: "b1", Working: "b2"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rigHeads = %v, want %v", got, want)
		}
		if _, ok := got["gastown"]; ok {
			t.Error("rigHeads keyed the rig whose database the server does not know")
		}
		if len(*sent) != 2 {
			t.Fatalf("SQLCSV calls = %d, want the first read and one retry without gt", len(*sent))
		}
		wantQuery, err := rigHeadsQuery([]string{"be"}).Inline()
		if err != nil {
			t.Fatalf("rigHeadsQuery: %v", err)
		}
		if retry, err := (*sent)[1].Inline(); err != nil || retry != wantQuery {
			t.Errorf("retry statement = %q, %v, want %q without the dropped database", retry, err, wantQuery)
		}
	})

	t.Run("refusal in another case", func(t *testing.T) {
		t.Parallel()
		_, read := reads(`error 1105 (hy000): unknown system variable '@@be_working'`, "gt", "g1", "g2")
		got, err := rigHeads(town, rigs, read)
		if err != nil {
			t.Fatalf("rigHeads: %v", err)
		}
		want := map[string]beadsql.RigHash{"gastown": {Head: "g1", Working: "g2"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rigHeads = %v, want %v", got, want)
		}
	})

	for _, tc := range []struct {
		name    string
		refusal string
	}{
		{"a name that only starts with ours", `Error 1105 (HY000): Unknown system variable '@@gtx_head'`},
		{"a longer variable name", `Error 1105 (HY000): Unknown system variable '@@gt_head_ref'`},
		{"a database never asked for", `Error 1105 (HY000): Unknown system variable '@@om_head'`},
		{"a 1105 that is no unknown variable", `Error 1105 (HY000): gc failed: boom`},
		{"a 1105 naming ours that is no unknown variable", `Error 1105 (HY000): @@gt_head is unreadable: manifest lock`},
		{"that way of saying it under another code", `Error 1193 (HY000): Unknown system variable '@@gt_head'`},
		{"a longer code that starts with 1105", `Error 11050 (HY000): Unknown system variable '@@gt_head'`},
		{"a failure that is no 1105", `dial tcp 127.0.0.1:3307: connect: connection refused`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sent, read := reads(tc.refusal, "be", "b1", "b2")
			got, err := rigHeads(town, rigs, read)
			if err == nil {
				t.Fatalf("rigHeads = %v, nil, want the read's error", got)
			}
			if len(*sent) != 1 {
				t.Errorf("SQLCSV calls = %d, want no retry when no database asked for is unknown", len(*sent))
			}
		})
	}

	t.Run("a second unknown database fails the one retry", func(t *testing.T) {
		t.Parallel()
		second := errors.New(`Error 1105 (HY000): Unknown system variable '@@be_head'`)
		calls := 0
		read := sqlReaderFunc(func(beadsql.Query) ([][]string, error) {
			calls++
			if calls == 1 {
				return nil, errors.New(`Error 1105 (HY000): Unknown system variable '@@gt_head'`)
			}
			return nil, second
		})
		got, err := rigHeads(town, rigs, read)
		if !errors.Is(err, second) {
			t.Fatalf("rigHeads = %v, %v, want the retry's refusal: one database is dropped, not two", got, err)
		}
		if calls != 2 {
			t.Errorf("SQLCSV calls = %d, want the first read and exactly one retry", calls)
		}
	})

	t.Run("every database unknown refuses", func(t *testing.T) {
		t.Parallel()
		refusal := errors.New(`Error 1105 (HY000): Unknown system variable '@@gt_working'`)
		calls := 0
		read := sqlReaderFunc(func(beadsql.Query) ([][]string, error) {
			calls++
			return nil, refusal
		})
		got, err := rigHeads(town, []string{"gastown"}, read)
		if !errors.Is(err, refusal) {
			t.Fatalf("rigHeads = %v, %v, want the read's refusal rather than an empty answer", got, err)
		}
		if calls != 1 {
			t.Errorf("SQLCSV calls = %d, want no retry with no database left", calls)
		}
	})

	t.Run("the retry fails", func(t *testing.T) {
		t.Parallel()
		retryErr := errors.New("connection reset by peer")
		calls := 0
		read := sqlReaderFunc(func(beadsql.Query) ([][]string, error) {
			calls++
			if calls == 1 {
				return nil, errors.New(`Error 1105 (HY000): Unknown system variable '@@gt_head'`)
			}
			return nil, retryErr
		})
		if got, err := rigHeads(town, rigs, read); !errors.Is(err, retryErr) {
			t.Errorf("rigHeads = %v, %v, want the retry's error", got, err)
		}
	})
}

// TestTickEffRigHeadsFailedQuery: a failed read that is not Dolt's unknown-
// database 1105 is returned as the read's error with no map, and the statement
// is not sent again. A failure that names no unknown database, or names one
// under another code, must not read as an answer with some rigs missing.
func TestTickEffRigHeadsFailedQuery(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMetadata := func(rig, content string) {
		t.Helper()
		dir := filepath.Join(town, rig, ".beads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata("gastown", `{"dolt_database":"gt"}`)
	writeMetadata("second", `{"dolt_database":"be"}`)
	rigs := []string{"gastown", "second"}

	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"a failure that is no 1105", errors.New(`dial tcp 127.0.0.1:3307: connect: connection refused`)},
		{"the unknown variable under another code", errors.New(`Error 1193 (HY000): Unknown system variable '@@gt_head'`)},
		{"a longer code that starts with 1105", errors.New(`Error 11050 (HY000): Unknown system variable '@@gt_head'`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			read := sqlReaderFunc(func(beadsql.Query) ([][]string, error) {
				calls++
				return nil, tc.failure
			})
			got, err := rigHeads(town, rigs, read)
			if !errors.Is(err, tc.failure) {
				t.Errorf("rigHeads error = %v, want the read's error %v", err, tc.failure)
			}
			if got != nil {
				t.Errorf("rigHeads = %v, want no map with a failed read", got)
			}
			if calls != 1 {
				t.Errorf("SQLCSV calls = %d, want no retry for a failure that is no unknown database", calls)
			}
		})
	}
}

// TestTickEffRigHeadsParse: the SQLCSV header and row pair by column name into
// one rig per database — the row interleaves two databases, so positional
// pairing misattributes it — and a result that is not a parseable header and
// one row is refused (gt-5v8y37).
func TestTickEffRigHeadsParse(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMetadata := func(rig, content string) {
		t.Helper()
		dir := filepath.Join(town, rig, ".beads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata("gastown", `{"dolt_database":"gt"}`)
	writeMetadata("second", `{"dolt_database":"om"}`)
	rigs := []string{"gastown", "second"}
	parse := func(records [][]string) (map[string]beadsql.RigHash, error) {
		t.Helper()
		read := sqlReaderFunc(func(beadsql.Query) ([][]string, error) { return records, nil })
		return rigHeads(town, rigs, read)
	}

	got, err := parse([][]string{
		{"gt_head", "om_head", "gt_working", "om_working"},
		{"g1", "o1", "g2", "o2"},
	})
	if err != nil {
		t.Fatalf("rigHeads: %v", err)
	}
	want := map[string]beadsql.RigHash{
		"gastown": {Head: "g1", Working: "g2"},
		"second":  {Head: "o1", Working: "o2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rigHeads = %v, want %v", got, want)
	}

	for _, tc := range []struct {
		name    string
		records [][]string
	}{
		{"no records", nil},
		{"header only", [][]string{{"gt_head", "gt_working"}}},
		{"two data rows", [][]string{{"gt_head", "gt_working"}, {"h1", "w1"}, {"h2", "w2"}}},
		{"head without working", [][]string{{"gt_head", "om_head"}, {"h1", "h2"}}},
		{"short row", [][]string{{"gt_head", "gt_working"}, {"h1"}}},
	} {
		if rows, err := parse(tc.records); err == nil {
			t.Errorf("%s: rigHeads = %v, nil, want an error", tc.name, rows)
		}
	}
}
