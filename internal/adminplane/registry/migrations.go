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

// migrationName is the only file name loadMigrations accepts: a four-digit
// version, an underscore, a lower-case name, and .sql.
var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migration is one schema file: the version its name numbers it, the name the
// migrator records, and the SQL it applies.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded schema files ordered by version, or an error
// naming the first file that is misnamed, empty, or out of sequence.
func Migrations() ([]Migration, error) {
	return loadMigrations(schemaFS, "schema")
}

// loadMigrations reads the NNNN_name.sql files in dir of fsys, ordered by
// version. A misnamed or empty file, and a numbering that is not a contiguous
// run from 0001, is an error, so a migration cannot be skipped silently.
func loadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", dir, err)
	}
	var out []Migration
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
		out = append(out, Migration{Version: v, Name: m[2], SQL: string(body)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("registry: no migrations in %s", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if want := i + 1; m.Version != want {
			return nil, fmt.Errorf("registry: migrations must be numbered 0001 upward with no gap or duplicate; found %04d where %04d belongs", m.Version, want)
		}
	}
	return out, nil
}
