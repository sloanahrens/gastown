package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	beadsdk "github.com/steveyegge/beads"
)

type dbAccessor interface {
	beadsDBAccessor
}

// TestSchemaLevelProblem is the guard's verdict without a database: equal
// levels pass, anything else (including a read failure or an unknown bd
// level) is a problem naming both integers (B5-02).
func TestSchemaLevelProblem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		level   int
		readErr error
		want    int
		problem string
	}{
		{"equal", 66, nil, 66, ""},
		{"database ahead", 67, nil, 66, "database schema 67 does not match bd schema 66"},
		{"database behind", 60, nil, 66, "database schema 60 does not match bd schema 66"},
		{"read failed", 0, errors.New("table not found: schema_migrations"), 66, "cannot read schema_migrations: table not found: schema_migrations"},
		{"no bd level", 66, nil, 0, "bd schema level unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := schemaLevelProblem(tc.level, tc.readErr, tc.want)
			if tc.problem == "" && got != "" || tc.problem != "" && !strings.Contains(got, tc.problem) {
				t.Fatalf("schemaLevelProblem(%d, %v, %d) = %q, want %q", tc.level, tc.readErr, tc.want, got, tc.problem)
			}
		})
	}
}

// storeSchemaLevel reads the level the store's database reports, the way
// the guard does, so the Docker tests assert against the real table.
func storeSchemaLevel(t *testing.T, store beadsdk.Storage) int {
	t.Helper()
	level, err := readStoreSchemaLevel(context.Background(), store)
	if err != nil {
		t.Fatalf("readStoreSchemaLevel: %v", err)
	}
	return level
}

func TestCheckBeadsStoreCompatibility_AllowsMatchingSchemaLevel(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	store, cleanup := setupTestStore(t)
	defer cleanup()

	level := storeSchemaLevel(t, store)
	if level <= 0 {
		t.Fatalf("a migrated store reports schema level %d", level)
	}
	if err := checkBeadsStoreCompatibility(context.Background(), map[string]beadsdk.Storage{"hq": store}, level); err != nil {
		t.Fatalf("checkBeadsStoreCompatibility returned unexpected error: %v", err)
	}
}

// TestCheckBeadsStoreCompatibility_RejectsSchemaAheadOfBD seeds
// schema_migrations, the table bd actually advances, not metadata.bd_version,
// which the fork never writes (the old guard read it and could never fire).
func TestCheckBeadsStoreCompatibility_RejectsSchemaAheadOfBD(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	acc, ok := store.(dbAccessor)
	if !ok || acc.DB() == nil {
		t.Fatal("test store does not expose its database")
	}
	level := storeSchemaLevel(t, store)
	if _, err := acc.DB().ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", level+1); err != nil {
		t.Fatalf("seed schema_migrations: %v", err)
	}

	err := checkBeadsStoreCompatibility(ctx, map[string]beadsdk.Storage{"hq": store}, level)
	if err == nil {
		t.Fatal("expected incompatibility error, got nil")
	}
	want := "database schema " + itoa(level+1) + " does not match bd schema " + itoa(level)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q lacks %q", err, want)
	}
}
