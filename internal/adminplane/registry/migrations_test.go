package registry

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// fsOf returns an in-memory filesystem holding files, keyed by their full
// path.
func fsOf(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// unreadableFS lists its MapFS's files but fails to open the named one, so a
// test can drive loadMigrations' refusal of a file it cannot read.
type unreadableFS struct {
	fsys fstest.MapFS
	fail string
}

func (u unreadableFS) ReadDir(name string) ([]fs.DirEntry, error) { return u.fsys.ReadDir(name) }

func (u unreadableFS) Open(name string) (fs.File, error) {
	if name == u.fail {
		return nil, fs.ErrPermission
	}
	return u.fsys.Open(name)
}

func TestMigrationsAreOrderedAndNamed(t *testing.T) {
	t.Parallel()
	ms, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations() = %v; the embedded schema must load", err)
	}
	if len(ms) == 0 {
		t.Fatal("Migrations() returned no schema files")
	}
	for i, m := range ms {
		if m.Version != i+1 {
			t.Errorf("migration %d is version %d; want %d", i, m.Version, i+1)
		}
		if m.Name == "" || strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %d has name %q and empty SQL; want both set", i, m.Name)
		}
	}
}

// TestLoadMigrationsRejectsBadSets drives every refusal. The duplicate row
// wants the earlier of the two files, which the gap refusal does not name: it
// names only the file it found.
func TestLoadMigrationsRejectsBadSets(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fsys fs.FS
		want string
	}{
		"missing dir":  {fsOf(nil), "read schema"},
		"unreadable":   {unreadableFS{fsys: fsOf(map[string]string{"schema/0001_a.sql": "x"}), fail: "schema/0001_a.sql"}, "0001_a.sql"},
		"gap":          {fsOf(map[string]string{"schema/0001_a.sql": "x", "schema/0003_c.sql": "x"}), "0002"},
		"starts at 2":  {fsOf(map[string]string{"schema/0002_a.sql": "x"}), "0001"},
		"bad name":     {fsOf(map[string]string{"schema/0001_a.sql": "x", "schema/core.sql": "x"}), "core.sql"},
		"upper case":   {fsOf(map[string]string{"schema/0001_Core.sql": "x"}), "0001_Core.sql"},
		"duplicate":    {fsOf(map[string]string{"schema/0001_a.sql": "x", "schema/0001_b.sql": "x"}), "0001_a.sql"},
		"none":         {fsOf(map[string]string{"schema/readme.txt": "x"}), "no migrations"},
		"empty sql":    {fsOf(map[string]string{"schema/0001_a.sql": "  \n"}), "empty"},
		"non-sql file": {fsOf(map[string]string{"schema/0001_a.sql": "x", "schema/0002_b.txt": "x"}), "0002_b.txt"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadMigrations(tc.fsys, "schema")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadMigrations error = %v; want one naming %q", err, tc.want)
			}
		})
	}
}

// A subdirectory is not a migration; the loader lists only the files in dir.
func TestLoadMigrationsSkipsDirectories(t *testing.T) {
	t.Parallel()
	got, err := loadMigrations(fsOf(map[string]string{
		"schema/0001_a.sql":   "x",
		"schema/nested/b.sql": "y",
	}), "schema")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("loadMigrations = %+v; want only the file in schema and not the one in a subdirectory", got)
	}
}

// stateCheck matches the workspaces table's check constraint on its state
// column and captures the state list inside it.
var stateCheck = regexp.MustCompile(`CHECK\s*\(\s*state\s+IN\s*\(([^)]*)\)\s*\)`)

func TestStateCheckConstraintListsTheFourStates(t *testing.T) {
	t.Parallel()
	sql := workspacesSQL(t)
	m := stateCheck.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("the workspaces migration has no CHECK (state IN (...)) constraint; its SQL is:\n%s", sql)
	}
	var got []string
	for _, s := range strings.Split(m[1], ",") {
		got = append(got, strings.Trim(strings.TrimSpace(s), "'"))
	}
	want := []string{string(Active), string(Paused), string(Suspended), string(Closed)}
	if !slices.Equal(got, want) {
		t.Errorf("the state check constraint lists %v; want the registry's constants %v", got, want)
	}
}

// workspacesSQL returns the SQL of the embedded workspaces migration, failing
// the test when the embedded set has no migration by that name.
func workspacesSQL(t *testing.T) string {
	t.Helper()
	ms, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations() = %v", err)
	}
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Name == "workspaces" {
			return m.SQL
		}
		names = append(names, m.Name)
	}
	t.Fatalf("no embedded migration is named workspaces; Migrations() names %v", names)
	return ""
}
