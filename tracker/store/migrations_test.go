package store

import (
	"strings"
	"testing"
	"testing/fstest"
)

func fsOf(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

func TestLoadMigrationsOrdersAndNames(t *testing.T) {
	t.Parallel() // unit tier: pure parsing over an in-memory filesystem
	got, err := loadMigrations(fsOf(map[string]string{
		"schema/0002_labels.sql": "-- two",
		"schema/0001_core.sql":   "-- one",
	}), "schema")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].version != 1 || got[0].name != "core" || got[1].version != 2 || got[1].name != "labels" {
		t.Fatalf("loadMigrations = %+v, want versions 1 core, 2 labels in order", got)
	}
	if got[0].sql != "-- one" {
		t.Errorf("migration 1 sql = %q, want its file's text", got[0].sql)
	}
}

func TestLoadMigrationsRejectsBadSets(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"gap":          {map[string]string{"schema/0001_a.sql": "x", "schema/0003_c.sql": "x"}, "0002"},
		"starts at 2":  {map[string]string{"schema/0002_a.sql": "x"}, "0001"},
		"bad name":     {map[string]string{"schema/0001_a.sql": "x", "schema/core.sql": "x"}, "core.sql"},
		"upper case":   {map[string]string{"schema/0001_Core.sql": "x"}, "0001_Core.sql"},
		"duplicate":    {map[string]string{"schema/0001_a.sql": "x", "schema/0001_b.sql": "x"}, "0001"},
		"none":         {map[string]string{"schema/readme.txt": "x"}, "no migrations"},
		"empty sql":    {map[string]string{"schema/0001_a.sql": "  \n"}, "empty"},
		"non-sql file": {map[string]string{"schema/0001_a.sql": "x", "schema/0002_b.txt": "x"}, "0002_b.txt"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadMigrations(fsOf(tc.files), "schema")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadMigrations error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// The binary's own migrations must load: a bad file name or a gap is caught
// here, in the unit tier, not when a database is first opened.
func TestEmbeddedMigrationsLoad(t *testing.T) {
	t.Parallel()
	ms, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || SchemaLevel() != len(ms) {
		t.Errorf("SchemaLevel() = %d with %d migrations, want them equal and at least 1", SchemaLevel(), len(ms))
	}
}
