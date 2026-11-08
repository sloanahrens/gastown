package callercwd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// getwdAllowlistPath names the ratchet file listing every non-test os.Getwd
// site outside this package: the `real` sites that must stay on the process
// directory, and the `deferred` ones the size guard left to the follow-up bead.
const getwdAllowlistPath = "internal/callercwd/testdata/getwd-allow.txt"

// skippedScanDirs are directories the scan does not descend into: version
// control, dependencies, fixture trees, and the package's own testdata.
var skippedScanDirs = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
	"testdata":     true,
	"scratch":      true,
}

// getwdSite is one non-test os.Getwd use. owner is the enclosing function, or
// "package" for a package-level declaration; the allowlist keys on file:owner
// so an unrelated edit above the line does not invalidate the entry.
type getwdSite struct {
	file  string
	owner string
	line  int
}

func (s getwdSite) key() string { return s.file + ":" + s.owner }

// TestNoDirectGetwd is the ratchet: a non-test file outside this package that
// reads the process directory with os.Getwd fails here, naming the file and
// line, unless it is an allowlisted real site. Every conversion moves its call
// behind Getwd or Require, so the seam covers every command and a new command
// cannot bypass it by copying the old pattern.
func TestNoDirectGetwd(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	allowed := map[string]allowEntry{}
	for _, entry := range loadGetwdAllowlist(t, root) {
		if _, dup := allowed[entry.key]; dup {
			t.Fatalf("%s:%d: duplicate entry for %s", getwdAllowlistPath, entry.line, entry.key)
		}
		allowed[entry.key] = entry
	}

	var unlisted []getwdSite
	for _, site := range getwdSites(t, root) {
		if _, ok := allowed[site.key()]; !ok {
			unlisted = append(unlisted, site)
		}
	}
	if len(unlisted) == 0 {
		return
	}
	sort.Slice(unlisted, func(i, j int) bool {
		if unlisted[i].file != unlisted[j].file {
			return unlisted[i].file < unlisted[j].file
		}
		return unlisted[i].line < unlisted[j].line
	})
	var b strings.Builder
	b.WriteString("non-test code reads the process directory directly with os.Getwd; use internal/callercwd instead (Getwd for identity and display, Require where git runs or a write lands), or allowlist the site as real in " + getwdAllowlistPath + ":\n")
	for _, site := range unlisted {
		fmt.Fprintf(&b, "  %s:%d %s\n", site.file, site.line, site.key())
	}
	t.Fatal(b.String())
}

// TestGetwdAllowlistCanOnlyShrink keeps the allowlist a record of what is
// still to convert: an allowlisted site that no longer calls os.Getwd fails
// here until its line is removed, so the list cannot outlive the code it
// excuses.
func TestGetwdAllowlistCanOnlyShrink(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	live := map[string]bool{}
	for _, site := range getwdSites(t, root) {
		live[site.key()] = true
	}

	var stale []allowEntry
	for _, entry := range loadGetwdAllowlist(t, root) {
		if !live[entry.key] {
			stale = append(stale, entry)
		}
	}
	if len(stale) == 0 {
		return
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].key < stale[j].key })
	var b strings.Builder
	b.WriteString("allowlisted sites no longer call os.Getwd; remove each line from " + getwdAllowlistPath + " so the allowlist only shrinks:\n")
	for _, entry := range stale {
		fmt.Fprintf(&b, "  %s:%d %s\n", getwdAllowlistPath, entry.line, entry.key)
	}
	t.Fatal(b.String())
}

// allowEntry is one line of the allowlist: path:owner, the class that keeps it
// (real or deferred), and the one-line reason.
type allowEntry struct {
	key    string
	class  string
	reason string
	line   int
}

// loadGetwdAllowlist reads testdata/getwd-allow.txt. A malformed line is an
// error, never skipped: a line the guard cannot parse would silently exempt
// nothing.
func loadGetwdAllowlist(t *testing.T, root string) []allowEntry {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, getwdAllowlistPath))
	if err != nil {
		t.Fatalf("read %s: %v", getwdAllowlistPath, err)
	}
	var entries []allowEntry
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.SplitN(trimmed, "\t", 3)
		if len(fields) != 3 {
			t.Fatalf("%s:%d: want path:owner<TAB>class<TAB>reason, got %q", getwdAllowlistPath, i+1, trimmed)
		}
		class := strings.TrimSpace(fields[1])
		if class != "real" && class != "deferred" {
			t.Fatalf("%s:%d: class %q is not real or deferred", getwdAllowlistPath, i+1, class)
		}
		if strings.TrimSpace(fields[2]) == "" {
			t.Fatalf("%s:%d: entry for %s has no reason", getwdAllowlistPath, i+1, fields[0])
		}
		entries = append(entries, allowEntry{
			key:    strings.TrimSpace(fields[0]),
			class:  class,
			reason: strings.TrimSpace(fields[2]),
			line:   i + 1,
		})
	}
	return entries
}

// getwdSites scans every non-test Go file under root outside this package and
// returns the os.Getwd uses in them. It reads repository source only, rooted
// at the module root, and fails on a directory or parse error rather than
// reporting a clean scan.
func getwdSites(t *testing.T, root string) []getwdSite {
	t.Helper()
	var sites []getwdSite
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedScanDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/callercwd/") {
			return nil
		}
		found, err := parseGetwdSites(path, rel)
		if err != nil {
			return err
		}
		sites = append(sites, found...)
		return nil
	})
	if err != nil {
		t.Fatalf("scanning %s: %v", root, err)
	}
	return sites
}

// parseGetwdSites parses one file and returns its os.Getwd selectors with the
// enclosing function (or "package" for a package-level declaration).
func parseGetwdSites(path, rel string) ([]getwdSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	var sites []getwdSite
	scan := func(owner string, node ast.Node) {
		if node == nil {
			return
		}
		ast.Inspect(node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && isOsGetwd(sel) {
				sites = append(sites, getwdSite{file: rel, owner: owner, line: fset.Position(sel.Pos()).Line})
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			scan(d.Name.Name, d)
		case *ast.GenDecl:
			scan("package", d)
		}
	}
	return sites, nil
}

// isOsGetwd reports whether sel is os.Getwd.
func isOsGetwd(sel *ast.SelectorExpr) bool {
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "os" && sel.Sel.Name == "Getwd"
}
