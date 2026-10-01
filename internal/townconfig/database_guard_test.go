package townconfig

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// dolt_database is a key of bd's metadata.json and of the rig registry.
// These are the production functions (or "<decl>" for a type declaration)
// allowed to name it, with the reason. Every other reader goes through
// database.go: DatabaseForBeadsDir, RigDatabaseName, RegistryDatabase, or
// config.BeadsMetadataDatabase for bd's own copy (gt-y3pgh.11).
var doltDatabaseKeyAllowed = map[string]string{
	"internal/config/types.go:<decl>":                             "the registry field",
	"internal/config/bd_metadata.go:BeadsFileDatabase":            "the one read of bd's copy",
	"internal/beads/beads_metadata.go:EnsureMetadataDatabase":     "writes bd's metadata.json",
	"internal/doltserver/doltserver.go:EnsureMetadataForBeadsDir": "writes bd's metadata.json",
	"internal/doctor/migration_check.go:writeDoltMetadata":        "writes bd's metadata.json (gt doctor --fix)",
	"internal/doctor/rig_config_sync_check.go:Fix":                "writes bd's metadata.json (gt doctor --fix)",
}

// TestDoltDatabaseIsReadThroughTheKernel fails when production code names the
// dolt_database key outside the allowed writers: a struct tag or map key
// reading metadata.json by hand would skip the registry.
func TestDoltDatabaseIsReadThroughTheKernel(t *testing.T) {
	t.Parallel()
	sites, err := scanDoltDatabaseKey(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// A scanner gone blind passes everything: the allowed sites must show.
	seen := map[string]bool{}
	for _, site := range sites {
		seen[site] = true
		if _, ok := doltDatabaseKeyAllowed[site]; !ok {
			t.Errorf("%s names the dolt_database key; read it through townconfig.DatabaseForBeadsDir / RigDatabaseName (or config.BeadsMetadataDatabase for bd's own copy) (gt-y3pgh.11)", site)
		}
	}
	for site := range doltDatabaseKeyAllowed {
		if !seen[site] {
			t.Errorf("doltDatabaseKeyAllowed lists %s, which no longer names dolt_database; delete the entry", site)
		}
	}
}

func TestDoltDatabaseKeyScanFindsTagsAndMapKeys(t *testing.T) {
	t.Parallel()
	src := "package p\n\n" +
		"type meta struct {\n\tDB string `json:\"dolt_database\"`\n}\n\n" +
		"func tag() { var m struct{ DB string `json:\"dolt_database\"` }; _ = m }\n\n" +
		"func key(m map[string]any) any { return m[\"dolt_database\"] }\n\n" +
		"func prose() error { return errorf(\"no dolt_database in metadata.json\") }\n"
	sites, err := doltDatabaseKeyInFile("x.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if want := "x.go:<decl>,x.go:key,x.go:tag"; strings.Join(sites, ",") != want {
		t.Errorf("sites = %v, want %s", sites, want)
	}
}

func scanDoltDatabaseKey(root string) ([]string, error) {
	var sites []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				// internal/testutil builds test databases.
				if d.Name() == "testdata" || rel == "internal/testutil" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path) //nolint:gosec // G304: repo source under the test's root
			if err != nil {
				return err
			}
			if !bytes.Contains(src, []byte("dolt_database")) {
				return nil
			}
			s, err := doltDatabaseKeyInFile(rel, src)
			if err != nil {
				return err
			}
			sites = append(sites, s...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(sites)
	return sites, nil
}

// doltDatabaseKeyInFile returns "<rel>:<func>" (or "<rel>:<decl>" outside a
// function) for each declaration in src holding a string literal that is the
// key dolt_database or a struct tag naming it.
func doltDatabaseKeyInFile(rel string, src []byte) ([]string, error) {
	af, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var sites []string
	for _, decl := range af.Decls {
		name := "<decl>"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			name = fn.Name.Name
		}
		found := false
		ast.Inspect(decl, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return !found
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && (s == "dolt_database" || strings.Contains(s, `json:"dolt_database`)) {
				found = true
			}
			return !found
		})
		if found {
			sites = append(sites, rel+":"+name)
		}
	}
	sort.Strings(sites)
	return sites, nil
}
