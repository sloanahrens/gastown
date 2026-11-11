package townstate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
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
			name: "unknown placeholder beside text",
			yaml: "entries:\n  - path: ESTOP.<rigx>\n    owner: shared\n    why: a typo\n",
			want: `unknown placeholder "ESTOP.<rigx>"`,
		},
		{
			name: "two placeholders in one segment",
			yaml: "entries:\n  - path: <rig><bird>/crew\n    owner: session\n    why: two placeholders\n",
			want: `unknown placeholder "<rig><bird>"`,
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

// newManifestRows are the rows gt-plrm1y adds to the split, with the owner each
// must carry. TestManifestWhysNameTheirSource holds every one to a source file,
// and TestManifestHasNoOverlappingRows holds the set to one owner per path.
var newManifestRows = []manifestRow{
	// Town-root state.
	{path: ".events.jsonl.lock", owner: OwnerDaemon},
	{path: ".dolt-archive", owner: OwnerDaemon},
	{path: ".beads-wisp", owner: OwnerDaemon},

	// The dispatch hold and the ESTOP sentinels: the operator writes them on the
	// Mac and the pod reads them each tick.
	{path: "seat-refill.hold", owner: OwnerShared},
	{path: "ESTOP", owner: OwnerShared},
	{path: "ESTOP.<rig>", owner: OwnerShared},

	// Rig-level paths each registered rig carries.
	{path: "<rig>/.beads", owner: OwnerDaemon},
	{path: "<rig>/plugins", owner: OwnerDaemon},
	{path: "<rig>/settings", owner: OwnerShared},
	{path: "<rig>/directives", owner: OwnerShared},
	{path: "<rig>/scripts", owner: OwnerShared},
	{path: "<rig>/.gastown-harness-manifest.json", owner: OwnerShared},
	{path: "<rig>/.runtime/namepool-state.json", owner: OwnerDaemon},
	{path: "<rig>/.runtime/locks", owner: OwnerDaemon},
	{path: "<rig>/.runtime/session_id", owner: OwnerSession},
	{path: "<rig>/.runtime/session_start_emitted", owner: OwnerSession},
	{path: "<rig>/.runtime/last_handoff_ts", owner: OwnerSession},
	{path: "<rig>/.runtime/agent.lock", owner: OwnerSession},
	{path: "<rig>/.runtime/agent.lock.flock", owner: OwnerSession},
	{path: "<rig>/.runtime/handoff_to_successor", owner: OwnerSession},

	// The daemon's own .runtime paths.
	{path: ".runtime/ci-failures.jsonl", owner: OwnerDaemon},
	{path: ".runtime/cloud-check", owner: OwnerDaemon},
	{path: ".runtime/deploy-watch.json", owner: OwnerDaemon},
	{path: ".runtime/diskmaint", owner: OwnerDaemon},
	{path: ".runtime/gate-logs", owner: OwnerDaemon},
	{path: ".runtime/hm-src", owner: OwnerDaemon},
	{path: ".runtime/om-src", owner: OwnerDaemon},
	{path: ".runtime/host", owner: OwnerDaemon},
	{path: ".runtime/nudge_poller", owner: OwnerDaemon},
	{path: ".runtime/openshell", owner: OwnerDaemon},
	{path: ".runtime/patrol_scan", owner: OwnerDaemon},
	{path: ".runtime/pids", owner: OwnerDaemon},
	{path: ".runtime/polecat-image-install.log", owner: OwnerDaemon},
	{path: ".runtime/polecat-image.lock.flock", owner: OwnerDaemon},
	{path: ".runtime/polecat-image.published", owner: OwnerDaemon},
	{path: ".runtime/rigs", owner: OwnerDaemon},
	{path: ".runtime/sandbox-seat-ends", owner: OwnerDaemon},
	{path: ".runtime/sbx-k8s", owner: OwnerDaemon},
	{path: ".runtime/scheduler-dispatch.lock", owner: OwnerDaemon},
	{path: ".runtime/signing", owner: OwnerDaemon},
	{path: ".runtime/sling-startup-failures", owner: OwnerDaemon},
	{path: ".runtime/spec-lint", owner: OwnerDaemon},
	{path: ".runtime/supervisor", owner: OwnerDaemon},
	{path: ".runtime/tier-sweep", owner: OwnerDaemon},
	{path: ".runtime/tiertrigger", owner: OwnerDaemon},
	{path: ".runtime/town-metrics.lock", owner: OwnerDaemon},
	{path: ".runtime/townhealth.json", owner: OwnerDaemon},
	{path: ".runtime/update", owner: OwnerDaemon},
	{path: ".runtime/daemon-liveness.json", owner: OwnerDaemon},
	{path: ".runtime/daemon-respawn.json", owner: OwnerDaemon},

	// .runtime paths only gt commands write, daemon-owned after the wrapper
	// cutover, as the .runtime/agents row states.
	{path: ".runtime/polecat-admission", owner: OwnerDaemon},
	{path: ".runtime/polecat-pool-claims", owner: OwnerDaemon},
	{path: ".runtime/polecat-prompts", owner: OwnerDaemon},
	{path: ".runtime/rehearsal", owner: OwnerDaemon},
	{path: ".runtime/hooks", owner: OwnerDaemon},
	{path: ".runtime/composer_pending", owner: OwnerDaemon},
	{path: ".runtime/nudge_queue", owner: OwnerDaemon},
	{path: ".runtime/seat-refill.json", owner: OwnerDaemon},
	{path: ".runtime/bead-respawn-counts.json", owner: OwnerDaemon},
	{path: ".runtime/bead-respawn-counts.json.flock", owner: OwnerDaemon},
	{path: ".runtime/claude-plan", owner: OwnerDaemon},
	{path: ".runtime/watch", owner: OwnerDaemon},
	{path: ".runtime/jev-dataset", owner: OwnerDaemon},
	{path: ".runtime/dashboard-load.jsonl", owner: OwnerDaemon},
	{path: ".runtime/sling-context-scan", owner: OwnerDaemon},

	// Session state a session writes in its cwd at the town root.
	{path: ".runtime/session_id", owner: OwnerSession},
	{path: ".runtime/session_start_emitted", owner: OwnerSession},
	{path: ".runtime/last_handoff_ts", owner: OwnerSession},
}

// TestManifestHasNoOverlappingRows fails when two rows cover one path with a
// different owner, which verify reports as a problem and a volume seed cannot
// resolve. The rig/.runtime parent is refused outright: its children have a
// daemon owner and a session owner, so a parent row would overlap both.
func TestManifestHasNoOverlappingRows(t *testing.T) {
	t.Parallel()
	m := Default()
	if m.names(RigPlaceholder + "/" + constants.DirRuntime) {
		t.Errorf("the manifest names %s/%s as one row; its children have two owners, so name the children instead",
			RigPlaceholder, constants.DirRuntime)
	}
	entries := m.Expand([]string{"gastown", "beads"}, BirdRuntimeNames())
	if problems := overlaps(entries); len(problems) != 0 {
		t.Errorf("the manifest covers a path twice with different owners: %v", problems)
	}
}

// manifestRow is a row a test holds to an owner and a why.
type manifestRow struct {
	path  string
	owner Owner
}

// sourceExtensions are the endings of the source files a why cites.
var sourceExtensions = []string{".go", ".yaml", ".sh", ".md"}

// whySourceTokens returns the source files why names: every whitespace-separated
// token that holds a slash and ends in a source extension, once the punctuation
// around it is trimmed. It fails closed: the token is returned whatever its first
// segment, so a misspelled directory or a dot-prefixed path is still checked. A
// token starting with ~ or / is a host path (~/.claude/statusline-command.sh),
// not a file in this repository, and is skipped.
func whySourceTokens(why string) []string {
	var out []string
	for _, tok := range strings.Fields(why) {
		tok = strings.TrimRight(strings.Trim(tok, "([{'\"`"), ".,;:)]}'\"`")
		if !strings.Contains(tok, "/") || strings.HasPrefix(tok, "~") || strings.HasPrefix(tok, "/") {
			continue
		}
		for _, ext := range sourceExtensions {
			if strings.HasSuffix(tok, ext) {
				out = append(out, tok)
				break
			}
		}
	}
	return out
}

// checkRowWhys is the row loop TestManifestWhysNameTheirSource runs. Each row
// must be in m with the owner it names, its why must cite at least one source
// file, and every file it cites must exist under root.
func checkRowWhys(t testing.TB, m *Manifest, root string, rows []manifestRow) {
	t.Helper()
	for _, want := range rows {
		e, ok := entryNamed(m, want.path)
		if !ok {
			t.Errorf("the manifest has no row %q", want.path)
			continue
		}
		if e.Owner != want.owner {
			t.Errorf("row %q is %q, want %q", want.path, e.Owner, want.owner)
		}
		cited := whySourceTokens(e.Why)
		if len(cited) == 0 {
			t.Errorf("row %q's why cites no source file: %q", want.path, e.Why)
		}
		for _, src := range cited {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(src))); err != nil {
				t.Errorf("row %q's why cites %s, which the repository does not hold", want.path, src)
			}
		}
	}
}

// recordingTB collects the errors a check reports, so a test can drive the
// row loop with a fixture manifest and assert what it refuses.
type recordingTB struct {
	testing.TB
	errs []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// TestManifestWhysNameTheirSource fails when a row gt-plrm1y adds carries the
// wrong owner, a why that cites no source file, or one that cites a file the
// repository does not hold.
func TestManifestWhysNameTheirSource(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")

	t.Run("every new row cites source files the repository holds", func(t *testing.T) {
		checkRowWhys(t, Default(), root, newManifestRows)
	})

	t.Run("a bad citation in the row loop is reported", func(t *testing.T) {
		fixture := &Manifest{Entries: []Entry{
			{Path: ".runtime/fixture-missing", Owner: OwnerDaemon, Why: "the state internal/townstate/no-such-fixture.go writes"},
			{Path: ".runtime/fixture-typo", Owner: OwnerDaemon, Why: "the state internal/townstat/townstate.go writes"},
			{Path: ".runtime/fixture-dot", Owner: OwnerDaemon, Why: "the state .githubb/workflows/ci.yaml writes"},
			{Path: ".runtime/fixture-cited", Owner: OwnerDaemon, Why: "the state internal/townstate/townstate.go writes"},
			{Path: ".runtime/fixture-none", Owner: OwnerDaemon, Why: "the state the daemon writes"},
		}}
		rows := []manifestRow{
			{path: ".runtime/fixture-missing", owner: OwnerDaemon},
			{path: ".runtime/fixture-typo", owner: OwnerDaemon},
			{path: ".runtime/fixture-dot", owner: OwnerDaemon},
			{path: ".runtime/fixture-cited", owner: OwnerDaemon},
			{path: ".runtime/fixture-none", owner: OwnerDaemon},
		}
		rec := &recordingTB{TB: t}
		checkRowWhys(rec, fixture, root, rows)
		for _, want := range []struct{ row, what string }{
			{".runtime/fixture-missing", "internal/townstate/no-such-fixture.go"},
			{".runtime/fixture-typo", "internal/townstat/townstate.go"},
			{".runtime/fixture-dot", ".githubb/workflows/ci.yaml"},
			{".runtime/fixture-none", "no source file"},
		} {
			if !slices.ContainsFunc(rec.errs, func(e string) bool {
				return strings.Contains(e, want.row) && strings.Contains(e, want.what)
			}) {
				t.Errorf("the row loop did not report row %q with %q; it reported %q", want.row, want.what, rec.errs)
			}
		}
		if slices.ContainsFunc(rec.errs, func(e string) bool { return strings.Contains(e, "fixture-cited") }) {
			t.Errorf("the row loop refused a row that cites a file the repository holds: %q", rec.errs)
		}
	})
}

// TestManifestPrivateRowsKeepTheirModes names the daemon rows that can hold
// credential material or a token, which an import must copy with the archive's
// mode: a kubeconfig, a keychain, a 0600 password file, captured output and
// clone remote URLs. A row that loses private: true fails here, in the Go data
// or in the yaml mirror. The list is reviewed by hand, since the test cannot
// tell a credential path from any other: a new credential row must be added to
// it.
func TestManifestPrivateRowsKeepTheirModes(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "daemon", "town-state.yaml"))
	if err != nil {
		t.Fatalf("reading the mirror: %v", err)
	}
	mirror, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing the mirror: %v", err)
	}
	for _, src := range []struct {
		name string
		m    *Manifest
	}{{"the Go data", Default()}, {"the yaml mirror", mirror}} {
		for _, p := range []string{
			".runtime/container", ".runtime/sbx-k8s", ".runtime/signing",
			".runtime/gate-logs", ".runtime/openshell", ".runtime/om-src", ".runtime/hm-src",
		} {
			e, ok := entryNamed(src.m, p)
			if !ok {
				t.Errorf("%s has no row %q", src.name, p)
				continue
			}
			if e.Owner != OwnerDaemon {
				t.Errorf("%s: row %q is %q; only a daemon row is copied, so only it can be private", src.name, p, e.Owner)
			}
			if !e.Private {
				t.Errorf("%s: row %q can hold a credential but does not set private: true, so an import would mask its modes to 0755/0644", src.name, p)
			}
		}
	}
}

// TestManifestWhysAreNotPlaceholders is the half Validate and the parser's
// refusal table miss: an empty why is refused there, a why such as "TODO" is
// not. Every row's why must say what the path holds.
func TestManifestWhysAreNotPlaceholders(t *testing.T) {
	t.Parallel()
	for _, e := range Default().Entries {
		low := strings.ToLower(e.Why)
		for _, placeholder := range []string{"todo", "tbd", "fixme", "xxx", "...", "placeholder"} {
			if strings.Contains(low, placeholder) {
				t.Errorf("row %q has a placeholder why %q (holds %q); say what the path holds and who writes it", e.Path, e.Why, placeholder)
			}
		}
	}
}

// entryNamed returns the entry whose path is exactly p.
func entryNamed(m *Manifest, p string) (Entry, bool) {
	for _, e := range m.Entries {
		if e.Path == p {
			return e, true
		}
	}
	return Entry{}, false
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

// TestSessionOwnedPlaceholderInsideASegment holds a placeholder that is part of
// a segment to the text beside it: <rig>-scratch covers gastown-scratch, and
// nothing that shares only part of the name.
func TestSessionOwnedPlaceholderInsideASegment(t *testing.T) {
	t.Parallel()
	m := &Manifest{Entries: []Entry{{Path: "<rig>-scratch", Owner: OwnerSession, Why: "a fixture row"}}}
	cases := []struct {
		rel  string
		want bool
	}{
		{rel: "gastown-scratch", want: true},
		{rel: "gastown-scratch/notes", want: true},
		{rel: "gastown-scratchy/notes", want: false},
		{rel: "gastown/notes", want: false},
		{rel: "-scratch/notes", want: false},
	}
	for _, tc := range cases {
		if _, ok := m.SessionOwned(tc.rel); ok != tc.want {
			t.Errorf("SessionOwned(%q) covered = %v, want %v", tc.rel, ok, tc.want)
		}
	}
}

// TestDispatchHoldAndEstopRowsAreShared fails when the dispatch hold, the town
// ESTOP or a registered rig's ESTOP has no shared row. The paths are the names
// the code reads, so a renamed file moves this test with it.
func TestDispatchHoldAndEstopRowsAreShared(t *testing.T) {
	t.Parallel()
	entries := Default().Expand([]string{"gastown"}, nil)
	for _, p := range []string{dispatch.HoldFileName, estop.FileName, estop.RigFileName("gastown")} {
		e, ok := Lookup(entries, p)
		if !ok {
			t.Errorf("no row covers %s", p)
			continue
		}
		if e.Owner != OwnerShared {
			t.Errorf("row %q covers %s, want owner %q", e.Path, p, OwnerShared)
		}
	}
}

// TestEstopRigRowExpandsPerRegisteredRig holds ESTOP.<rig> to the rigs the town
// registers: one ESTOP.<name> per rig, beside the town ESTOP, and no row for a
// rig the town does not have.
func TestEstopRigRowExpandsPerRegisteredRig(t *testing.T) {
	t.Parallel()
	var got []string
	for _, e := range Default().Expand([]string{"gastown", "beads"}, nil) {
		if strings.HasPrefix(e.Path, estop.FileName) {
			got = append(got, e.Path)
		}
	}
	if want := []string{"ESTOP", "ESTOP.gastown", "ESTOP.beads"}; !slices.Equal(got, want) {
		t.Errorf("ESTOP rows expand to %v, want %v", got, want)
	}
}
