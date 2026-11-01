package registry

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// migrationName is the name a file must have to be read as a migration: a
// four-digit version, an underscore, a lower-case name, and .sql. A file that
// misses it is refused only when it looks like a migration (it ends in .sql or
// begins with a zero); anything else is skipped, so a README may sit beside the
// schema.
var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migration is one schema file: the version its name numbers it, the name the
// migrator records, and the SQL it applies.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded schema files ordered by version, or an error
// naming the file that is misnamed, empty, duplicated, or out of sequence.
func Migrations() ([]Migration, error) {
	return loadMigrations(schemaFS, "schema")
}

// loadMigrations reads the NNNN_name.sql files in dir of fsys, ordered by
// version. An empty file, two files numbering one version, and a numbering that
// is not a contiguous run from 0001 are errors, so a migration cannot be
// skipped silently. A misnamed file is refused when it looks like a migration
// and skipped otherwise.
func loadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", dir, err)
	}
	type namedMigration struct {
		file string
		Migration
	}
	var found []namedMigration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			if strings.HasSuffix(e.Name(), ".sql") || strings.HasPrefix(e.Name(), "0") {
				return nil, fmt.Errorf("registry: migration file %s is not named NNNN_name.sql (lower case)", e.Name())
			}
			continue
		}
		body, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("registry: read %s: %w", e.Name(), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("registry: migration %s is empty", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		found = append(found, namedMigration{file: e.Name(), Migration: Migration{Version: v, Name: m[2], SQL: string(body)}})
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("registry: no migrations in %s", dir)
	}
	// Stable, so two files of one version keep the name order ReadDir listed
	// them in and the duplicate the error names is the one a reader can predict.
	sort.SliceStable(found, func(i, j int) bool { return found[i].Version < found[j].Version })
	firstOf := make(map[int]string, len(found))
	out := make([]Migration, 0, len(found))
	for _, f := range found {
		if prev, duplicate := firstOf[f.Version]; duplicate {
			return nil, fmt.Errorf("registry: migrations %s and %s both number version %04d", prev, f.file, f.Version)
		}
		firstOf[f.Version] = f.file
		if want := len(out) + 1; f.Version != want {
			return nil, fmt.Errorf("registry: migrations must be numbered 0001 upward with no gap; %s is %04d where %04d is expected", f.file, f.Version, want)
		}
		out = append(out, f.Migration)
	}
	return out, nil
}
