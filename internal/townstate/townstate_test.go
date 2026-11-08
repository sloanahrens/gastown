package townstate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestManifestMirrorMatches fails when the YAML mirror and the Go data drift,
// and proves the parser refuses an entry that cannot name a town-root path.
func TestManifestMirrorMatches(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "daemon", "town-state.yaml"))
	if err != nil {
		t.Fatalf("reading the mirror: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing the mirror: %v", err)
	}
	if want := Default(); !reflect.DeepEqual(got, want) {
		t.Errorf("deploy/daemon/town-state.yaml does not match townstate.Default():\n got %+v\nwant %+v", got.Entries, want.Entries)
	}

	refusals := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "absolute path",
			yaml: "entries:\n  - path: /Users/sloan/gt/settings\n    owner: daemon\n    why: absolute\n",
			want: "relative to the town root",
		},
		{
			name: "parent segment",
			yaml: "entries:\n  - path: ../elsewhere\n    owner: daemon\n    why: escapes the town\n",
			want: `".." is not a town-root path segment`,
		},
		{
			name: "empty why",
			yaml: "entries:\n  - path: logs\n    owner: daemon\n    why: \"\"\n",
			want: "why is empty",
		},
		{
			name: "unknown owner",
			yaml: "entries:\n  - path: logs\n    owner: root\n    why: who is root\n",
			want: `unknown owner "root"`,
		},
		{
			name: "empty path",
			yaml: "entries:\n  - path: \"\"\n    owner: daemon\n    why: no path\n",
			want: "has an empty path",
		},
		{
			name: "empty path segment",
			yaml: "entries:\n  - path: logs//x\n    owner: daemon\n    why: double slash\n",
			want: "empty path segment",
		},
		{
			name: "unknown placeholder",
			yaml: "entries:\n  - path: <rigx>/crew\n    owner: session\n    why: a typo\n",
			want: `unknown placeholder "<rigx>"`,
		},
		{
			name: "not YAML",
			yaml: "entries: [unclosed\n",
			want: "reading the town-state manifest",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Parse accepted an entry it must refuse")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestTownStateRigPathsAreTheProductionNames ties the rig repository and
// canonical clone rows to the names the code that uses them is built from, not
// to a layout this test assumes. Three links: the manifest rows are spelled with
// constants.DirRepoGit, constants.DirMayor and constants.DirRig; rig.Rig's
// resolvers find a rig built from those constants and the rows class what they
// return daemon; and no code that runs git in a rig's repository (the daemon,
// the landing worker, the promoter, internal/rig) spells ".repo.git" itself, so
// a rename in constants moves every user and the manifest together.
func TestTownStateRigPathsAreTheProductionNames(t *testing.T) {
	t.Parallel()

	m := Default()
	for _, want := range []string{
		RigPlaceholder + "/" + constants.DirRepoGit,
		RigPlaceholder + "/" + constants.DirMayor + "/" + constants.DirRig,
	} {
		if !m.names(want) {
			t.Errorf("the manifest has no row %q; its rig rows must be spelled with the constants production builds paths from", want)
		}
	}

	town := t.TempDir()
	rigDir := filepath.Join(town, "gastown")
	bare := constants.RigBareRepoPath(rigDir)
	clone := constants.RigMayorPath(rigDir)
	for _, dir := range []string{bare, filepath.Join(clone, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bare, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &rig.Rig{Name: "gastown", Path: rigDir}
	entries := m.Expand([]string{"gastown"}, nil)
	for what, got := range map[string]string{"rig.BareRepoPath": r.BareRepoPath(), "rig.RepoPath": r.RepoPath()} {
		if got == "" {
			t.Fatalf("%s found nothing in a rig built from the constants", what)
		}
		rel, err := filepath.Rel(town, got)
		if err != nil {
			t.Fatal(err)
		}
		e, ok := Lookup(entries, filepath.ToSlash(rel))
		if !ok || e.Owner != OwnerDaemon {
			t.Errorf("%s resolves %s, which the manifest does not class daemon (covered %v, owner %q)", what, rel, ok, e.Owner)
		}
	}

	// No code that runs git in a rig repository spells the directory itself.
	fset := token.NewFileSet()
	for _, dir := range []string{"../daemon", "../landworker", "../promote"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		scanned := 0
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			scanned++
			af, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(af, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, constants.DirRepoGit) {
					t.Errorf("%s spells %s itself; use constants.RigBareRepoPath", fset.Position(lit.Pos()), constants.DirRepoGit)
				}
				return true
			})
		}
		if scanned == 0 {
			t.Errorf("the scan of %s read no source files; it has stopped seeing the package", dir)
		}
	}
}

// TestTownStateBirdNamesStayInTheCommandPackages is the second layer of the
// guard against daemon writes into a bird's directory. The first layer
// (internal/daemon, TestDaemonNeverWritesIntoABirdDirectory) reads write calls
// and cannot follow a path built at runtime. This one does not read write calls
// at all: a path under .runtime/<bird> can only be built by code that knows a
// bird's name, and the only packages that import the bird pool or the bird
// report reader are the commands that spawn birds and show their reports. A
// package that starts using either must be listed here, with the reason it
// cannot write below a bird's directory, or the test fails. The limit is stated,
// not hidden: a name read from data at run time (a directory listing, a
// registry file) does not import either package, and only the first layer and
// the verify command's static checks see that.
func TestTownStateBirdNamesStayInTheCommandPackages(t *testing.T) {
	t.Parallel()
	allowed := map[string]string{
		"internal/bird":        "the pool and the bird spawner itself",
		"internal/birdreports": "the reader of the reports a bird publishes; read-only",
		"internal/cmd":         "the bird commands and the dashboard reader; they run as the operator's commands, never from the daemon's loops",
		"internal/daemon":      "the bird_reports patrol resolves and reads the sidecar with the report reader; it opens that path only to read, and the first layer sees its writes",
		"internal/dashboard":   "the Birds panel and page route resolve and read a bird's reports with the report reader; read-only",
		"internal/townstate":   "classifies the bird directories as session-owned",
	}
	importers := map[string]bool{}
	for _, target := range []string{
		"github.com/steveyegge/gastown/internal/bird",
		"github.com/steveyegge/gastown/internal/birdreports",
	} {
		for _, pkg := range importersOf(t, target) {
			importers[pkg] = true
		}
	}
	if len(importers) == 0 {
		t.Fatal("the scan found no importer of the bird packages; it has stopped seeing the module")
	}
	for pkg := range importers {
		if _, ok := allowed[pkg]; !ok {
			t.Errorf("%s imports the bird pool or the bird report reader; list it in this test with the reason it cannot write below a bird's .runtime directory, or keep bird names out of it", pkg)
		}
	}
}

// importersOf returns the module-relative directories of the non-test Go files
// under internal/ and cmd/ that import target.
func importersOf(t *testing.T, target string) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, root := range []struct{ dir, prefix string }{{"..", "internal"}, {"../../cmd", "cmd"}} {
		err := filepath.WalkDir(root.dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			af, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range af.Imports {
				if strings.Trim(imp.Path.Value, `"`) != target {
					continue
				}
				rel, err := filepath.Rel(root.dir, filepath.Dir(p))
				if err != nil {
					return err
				}
				dir := path.Join(root.prefix, filepath.ToSlash(rel))
				if !seen[dir] {
					seen[dir] = true
					out = append(out, dir)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// SessionOwned is the check the compose renderer makes on a mount it would bind
// beside the town-state volume: a path under a session-owned entry is refused,
// and the placeholders stand for any rig or bird name because the renderer does
// not read the rig registry.
func TestSessionOwned(t *testing.T) {
	t.Parallel()
	m := Default()
	cases := []struct {
		rel  string
		want bool
		path string
	}{
		{rel: "gastown/polecats", want: true, path: "<rig>/polecats"},
		{rel: "gastown/polecats/cheedo/gastown", want: true, path: "<rig>/polecats"},
		{rel: "gastown/crew/dipper", want: true, path: "<rig>/crew"},
		{rel: "somerig/witness", want: true, path: "<rig>/witness"},
		{rel: "somerig/refinery/out", want: true, path: "<rig>/refinery"},
		{rel: ".runtime/some-bird/reports/2026", want: true, path: ".runtime/<bird>"},
		// The case fold matches the default macOS volume.
		{rel: "Gastown/Polecats/Cheedo", want: true, path: "<rig>/polecats"},
		// A daemon entry at the same depth wins, so a daemon subsystem under
		// .runtime is not read as a bird's directory.
		{rel: ".runtime/usage/2026-11-08", want: false},
		{rel: ".runtime/jev", want: false},
		// A segment must match whole: polecatsX is not polecats.
		{rel: "gastown/polecatsX/cheedo", want: false},
		{rel: "gastown/config.json", want: false},
		{rel: "settings/daemon.json", want: false},
		{rel: "unclassified", want: false},
		{rel: "", want: false},
		{rel: ".", want: false},
	}
	for _, tc := range cases {
		got, ok := m.SessionOwned(tc.rel)
		if ok != tc.want {
			t.Errorf("SessionOwned(%q) = (%+v, %v), want covered %v", tc.rel, got, ok, tc.want)
			continue
		}
		if ok && got.Path != tc.path {
			t.Errorf("SessionOwned(%q) covered by %q, want %q", tc.rel, got.Path, tc.path)
		}
	}
}
