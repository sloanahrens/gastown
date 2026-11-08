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
	"strconv"
	"strings"
	"testing"
)

// getwdAllowlistPath names the ratchet file listing every non-test os.Getwd
// site outside this package that must stay on the process directory.
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

// getwdSite is one non-test os.Getwd use. owner is the enclosing function
// ("Receiver.Func" for a method) or "package" for a package-level declaration;
// the allowlist keys on file:owner so an unrelated edit above the line does not
// invalidate the entry, and a method never shares a key with a function or a
// closure of the same name.
type getwdSite struct {
	file  string
	owner string
	line  int
}

func (s getwdSite) key() string { return s.file + ":" + s.owner }

// allowClass is the only allowlist class: a site that stays on the real
// process directory because it decides something a caller-supplied variable
// must not steer.
const allowClass = "real"

// TestNoDirectGetwd is the ratchet: a non-test file outside this package that
// reads the process directory with os.Getwd fails here, naming the file and
// line, unless it is an allowlisted real site. Every conversion moves its call
// behind Getwd or Require, so the seam covers every command and a new command
// cannot bypass it by copying the old pattern.
func TestNoDirectGetwd(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	if err := checkNoDirectGetwdAt(root, filepath.Join(root, getwdAllowlistPath)); err != nil {
		t.Fatal(err)
	}
}

// TestGetwdAllowlistCanOnlyShrink keeps the allowlist a record of what stays
// real: an allowlisted site that no longer calls os.Getwd fails here until its
// line is removed, so the list cannot outlive the code it excuses.
func TestGetwdAllowlistCanOnlyShrink(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	if err := checkAllowlistShrinksAt(root, filepath.Join(root, getwdAllowlistPath)); err != nil {
		t.Fatal(err)
	}
}

// TestGetwdGuardFixtureCases drives every failing branch of the guard with
// fixture sources and allowlists in a temp directory, so each refusal is
// proven before the guard is trusted over the real tree.
func TestGetwdGuardFixtureCases(t *testing.T) {
	t.Parallel()

	const plain = "package pkg\n\nimport \"os\"\n\nfunc f() {\n\tos.Getwd()\n}\n"
	const aliased = "package pkg\n\nimport o \"os\"\n\nfunc f() {\n\to.Getwd()\n}\n"
	const dotted = "package pkg\n\nimport . \"os\"\n\nfunc f() {\n\tGetwd()\n}\n"
	const clean = "package pkg\n\nfunc f() {}\n"

	cases := []struct {
		name      string
		source    string
		allowlist string
		run       func(root, allowlistPath string) error
		want      string
	}{
		{
			name:      "a direct call outside the allowlist names the file and line",
			source:    plain,
			allowlist: "# no entries\n",
			run:       checkNoDirectGetwdAt,
			want:      "pkg/foo.go:6",
		},
		{
			name:      "an aliased os import is found",
			source:    aliased,
			allowlist: "# no entries\n",
			run:       checkNoDirectGetwdAt,
			want:      "pkg/foo.go:6",
		},
		{
			name:      "a dot os import is found",
			source:    dotted,
			allowlist: "# no entries\n",
			run:       checkNoDirectGetwdAt,
			want:      "pkg/foo.go:6",
		},
		{
			name:      "a stale entry fails the shrink test",
			source:    clean,
			allowlist: "pkg/foo.go:f\treal\ta guard\n",
			run:       checkAllowlistShrinksAt,
			want:      "pkg/foo.go:f",
		},
		{
			name:      "a malformed line is an error",
			source:    clean,
			allowlist: "pkg/foo.go:f real a guard\n",
			run:       loadOnly,
			want:      "want path:Receiver.Func<TAB>real<TAB>reason",
		},
		{
			name:      "an unknown class is an error",
			source:    clean,
			allowlist: "pkg/foo.go:f\tbogus\ta guard\n",
			run:       loadOnly,
			want:      `class "bogus"`,
		},
		{
			name:      "a deferred class is an error",
			source:    clean,
			allowlist: "pkg/foo.go:f\tdeferred\ta guard\n",
			run:       loadOnly,
			want:      "is not real",
		},
		{
			name:      "an empty reason is an error",
			source:    clean,
			allowlist: "pkg/foo.go:f\treal\t\n",
			run:       loadOnly,
			want:      "has no reason",
		},
		{
			name:      "a duplicate key is an error",
			source:    clean,
			allowlist: "pkg/foo.go:f\treal\tfirst\npkg/foo.go:f\treal\tsecond\n",
			run:       loadOnly,
			want:      "duplicate entry for pkg/foo.go:f",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/foo.go", tc.source)
			allowlistPath := filepath.Join(root, "allow.txt")
			writeFixture(t, root, "allow.txt", tc.allowlist)

			err := tc.run(root, allowlistPath)
			if err == nil {
				t.Fatalf("guard = nil, want the %q refusal", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("guard error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// loadOnly reads the allowlist and drops the scan, so a parse refusal can be
// driven without a matching source tree.
func loadOnly(_, allowlistPath string) error {
	_, err := loadGetwdAllowlist(allowlistPath)
	return err
}

// writeFixture writes one fixture file under root, creating its directory.
func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// skipCallercwd keeps this package's own calls to os.Getwd out of the scan:
// the seam and its tests are the one place allowed to read the real directory
// directly.
func skipCallercwd(rel string) bool { return strings.HasPrefix(rel, "internal/callercwd/") }

// checkNoDirectGetwdAt scans root and fails when a non-test site outside this
// package is not an allowlisted real entry. The allowlist is parsed (and its
// duplicates refused) before the scan result is compared, so a broken file
// never reads as an empty exemption list.
func checkNoDirectGetwdAt(root, allowlistPath string) error {
	sites, err := getwdSites(root, skipCallercwd)
	if err != nil {
		return err
	}
	entries, err := loadGetwdAllowlist(allowlistPath)
	if err != nil {
		return err
	}
	return unlistedGetwdSites(sites, entries)
}

// checkAllowlistShrinksAt fails for every allowlisted entry that no longer
// names a live os.Getwd site.
func checkAllowlistShrinksAt(root, allowlistPath string) error {
	sites, err := getwdSites(root, skipCallercwd)
	if err != nil {
		return err
	}
	entries, err := loadGetwdAllowlist(allowlistPath)
	if err != nil {
		return err
	}
	return staleGetwdEntries(entries, sites)
}

// unlistedGetwdSites reports the sites the allowlist does not excuse, naming
// each file and line.
func unlistedGetwdSites(sites []getwdSite, entries []allowEntry) error {
	allowed := map[string]bool{}
	for _, entry := range entries {
		allowed[entry.key] = true
	}
	var unlisted []getwdSite
	for _, site := range sites {
		if !allowed[site.key()] {
			unlisted = append(unlisted, site)
		}
	}
	if len(unlisted) == 0 {
		return nil
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
	return fmt.Errorf("%s", b.String())
}

// staleGetwdEntries reports the allowlist entries that no longer match a live
// site.
func staleGetwdEntries(entries []allowEntry, sites []getwdSite) error {
	live := map[string]bool{}
	for _, site := range sites {
		live[site.key()] = true
	}
	var stale []allowEntry
	for _, entry := range entries {
		if !live[entry.key] {
			stale = append(stale, entry)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].key < stale[j].key })
	var b strings.Builder
	b.WriteString("allowlisted sites no longer call os.Getwd; remove each line from " + getwdAllowlistPath + " so the allowlist only shrinks:\n")
	for _, entry := range stale {
		fmt.Fprintf(&b, "  %s:%d %s\n", getwdAllowlistPath, entry.line, entry.key)
	}
	return fmt.Errorf("%s", b.String())
}

// allowEntry is one line of the allowlist: path:Receiver.Func, the real class,
// and the one-line reason.
type allowEntry struct {
	key    string
	class  string
	reason string
	line   int
}

// loadGetwdAllowlist reads the allowlist. A malformed line, an unknown or
// non-real class, an empty reason and a duplicate key are errors, never
// skipped: a line the guard cannot parse would silently exempt nothing, and
// two lines for one key would leave the ratchet ambiguous.
func loadGetwdAllowlist(path string) ([]allowEntry, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var entries []allowEntry
	seen := map[string]int{}
	for i, line := range strings.Split(string(src), "\n") {
		raw := strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Split the untrimmed line so a trailing empty reason is a third field
		// and is refused, not absorbed by TrimSpace as a malformed two-field
		// line.
		fields := strings.SplitN(raw, "\t", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: want path:Receiver.Func<TAB>real<TAB>reason, got %q", path, i+1, trimmed)
		}
		key := strings.TrimSpace(fields[0])
		class := strings.TrimSpace(fields[1])
		reason := strings.TrimSpace(fields[2])
		if key == "" {
			return nil, fmt.Errorf("%s:%d: entry has no path:Receiver.Func key", path, i+1)
		}
		if class != allowClass {
			if class == "deferred" || class == "pending" {
				return nil, fmt.Errorf("%s:%d: class %q is not real: every site is converted or real, there is no deferred or pending class", path, i+1, class)
			}
			return nil, fmt.Errorf("%s:%d: class %q is not real", path, i+1, class)
		}
		if reason == "" {
			return nil, fmt.Errorf("%s:%d: entry for %s has no reason", path, i+1, key)
		}
		if first, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate entry for %s (first at line %d)", path, i+1, key, first)
		}
		seen[key] = i + 1
		entries = append(entries, allowEntry{key: key, class: class, reason: reason, line: i + 1})
	}
	return entries, nil
}

// getwdSites scans every non-test Go file under root and returns the os.Getwd
// uses in them, skipping a path for which skip reports true. It reads
// repository source only, rooted at the given root, and fails on a directory or
// parse error rather than reporting a clean scan.
func getwdSites(root string, skip func(rel string) bool) ([]getwdSite, error) {
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
		if skip(rel) {
			return nil
		}
		found, err := parseGetwdSitesFile(path, rel)
		if err != nil {
			return err
		}
		sites = append(sites, found...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", root, err)
	}
	return sites, nil
}

// parseGetwdSitesFile parses one file and returns its os.Getwd selectors with
// the enclosing function (or "package" for a package-level declaration). It
// resolves the file's os import, so `import o "os"` and `import . "os"` are
// found as well as the plain identifier.
func parseGetwdSitesFile(path, rel string) ([]getwdSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	osNames := importNames(file, "os")
	var sites []getwdSite
	scan := func(owner string, node ast.Node) {
		if node == nil {
			return
		}
		ast.Inspect(node, func(n ast.Node) bool {
			switch sel := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := sel.X.(*ast.Ident); ok && osNames[pkg.Name] && sel.Sel.Name == "Getwd" {
					sites = append(sites, getwdSite{file: rel, owner: owner, line: fset.Position(sel.Pos()).Line})
				}
			case *ast.Ident:
				if osNames["."] && sel.Name == "Getwd" {
					sites = append(sites, getwdSite{file: rel, owner: owner, line: fset.Position(sel.Pos()).Line})
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			scan(declOwner(d), d)
		case *ast.GenDecl:
			scan("package", d)
		}
	}
	return sites, nil
}

// importNames returns the local names a file binds importPath to: the package
// name for a plain or aliased import, and "." for a dot import. A blank import
// binds nothing.
func importNames(file *ast.File, importPath string) map[string]bool {
	names := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		switch {
		case imp.Name == nil:
			names[importPath] = true
		case imp.Name.Name == "_":
			// A blank import binds no name.
		default:
			names[imp.Name.Name] = true
		}
	}
	return names
}

// declOwner is the allowlist owner of a function: Receiver.Func for a method,
// Func for a plain function. A closure shares its enclosing function's owner,
// which is correct: the allowlist records the site, not an anonymous scope.
func declOwner(d *ast.FuncDecl) string {
	name := d.Name.Name
	receiver := receiverName(d.Recv)
	if receiver == "" {
		return name
	}
	return receiver + "." + name
}

// receiverName is the method receiver's type name without its pointer or type
// parameters.
func receiverName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch v := t.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}
