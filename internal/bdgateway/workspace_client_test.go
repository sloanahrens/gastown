package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// doerFunc is an HTTPDoer a test scripts: the request runs against the response
// the func returns, so no test opens a socket (docs/testing.md, no-network).
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// clientTown writes the fixture town every builder test reads: the town's own
// beads, a rig whose .beads is a redirect into mayor/rig, and a rig that holds
// its files in its own .beads. Each directory keeps a file the workspace does
// not serve, so a test catches a document that copies the directory rather than
// the files the workspace takes.
func clientTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	write("mayor/town.json", `{"type":"town","version":1,"name":"fx"}`)
	write("mayor/rigs.json", `{"version":1,"rigs":{"gastown":{},"toolbox":{}}}`)
	write(".beads/metadata.json", `{"dolt_database":"hq"}`+"\n")
	write(".beads/config.yaml", "issue-prefix: hq\n")
	write(".beads/dolt-server.port", "3307")
	write(".beads/routes.jsonl", strings.Join([]string{
		`{"path":".","prefix":"hq-"}`,
		`{"path":"gastown/mayor/rig","prefix":"gt-"}`,
		`{"path":"toolbox","prefix":"tb-"}`,
		// An absolute path bd resolves but the container never sees: the push
		// rewrites it to the town-relative form, and the town root's own path to
		// the "." its route uses.
		`{"path":` + quoteJSON(filepath.Join(root, "toolbox")) + `,"prefix":"ab-"}`,
		`{"path":` + quoteJSON(root) + `,"prefix":"rt-"}`,
		"",
	}, "\n"))
	write(".beads/.gt-types-configured", "x")
	write("gastown/.beads/redirect", "mayor/rig/.beads\n")
	write("gastown/.beads/.local_version", "6")
	write("gastown/mayor/rig/.beads/metadata.json", `{"dolt_database":"gt"}`+"\n")
	write("gastown/mayor/rig/.beads/config.yaml", "issue-prefix: gt\n")
	write("gastown/mayor/rig/.beads/dolt-server.port", "3307")
	write("gastown/mayor/rig/.beads/.gt-types-configured", "y")
	write("toolbox/.beads/metadata.json", `{"dolt_database":"toolbox"}`+"\n")
	write("toolbox/.beads/config.yaml", "issue-prefix: tb\n")
	write("toolbox/.beads/dolt/town.marker", "z")
	return root
}

// quoteJSON encodes s as a JSON string for a hand-built route line.
func quoteJSON(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// formulaTown writes the smallest town that carries a formula set: the town's
// own beads metadata and routes, the given number of registered rigs, each with
// its beads metadata, and the given number of formulas of size bytes. The
// builder then carries every formula once under the town and once under each
// rig.
func formulaTown(t *testing.T, rigs, formulas, size int) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	write("mayor/town.json", `{"type":"town","version":1,"name":"fx"}`)
	entries := make([]string, rigs)
	names := make([]string, rigs)
	for i := range names {
		names[i] = fmt.Sprintf("rig%02d", i)
		entries[i] = quoteJSON(names[i]) + ":{}"
	}
	write("mayor/rigs.json", `{"version":1,"rigs":{`+strings.Join(entries, ",")+`}}`)
	write(".beads/metadata.json", `{"dolt_database":"hq"}`+"\n")
	write(".beads/routes.jsonl", `{"path":".","prefix":"hq-"}`+"\n")
	formula := strings.Repeat("f", size)
	for i := 0; i < formulas; i++ {
		write(fmt.Sprintf(".beads/formulas/mol-%02d%s", i, formulaSuffix), formula)
	}
	for _, name := range names {
		write(name+"/.beads/metadata.json", `{"dolt_database":`+quoteJSON(name)+`}`+"\n")
	}
	return root
}

// clientTownDocument is the document writeClientTown's town builds, keyed by
// path. The redirect is the absolute /ws form and the absolute route path is
// the town-relative one; nothing else from the town is in it.
func clientTownDocument(t *testing.T, root string) map[string]string {
	t.Helper()
	return map[string]string{
		".beads/metadata.json":                      `{"dolt_database":"hq"}` + "\n",
		".beads/config.yaml":                        "issue-prefix: hq\n",
		".beads/dolt-server.port":                   "3307",
		".beads/routes.jsonl":                       clientTownRoutes,
		"gastown/.beads/redirect":                   "/ws/gastown/mayor/rig/.beads\n",
		"gastown/mayor/rig/.beads/metadata.json":    `{"dolt_database":"gt"}` + "\n",
		"gastown/mayor/rig/.beads/config.yaml":      "issue-prefix: gt\n",
		"gastown/mayor/rig/.beads/dolt-server.port": "3307",
		"toolbox/.beads/metadata.json":              `{"dolt_database":"toolbox"}` + "\n",
		"toolbox/.beads/config.yaml":                "issue-prefix: tb\n",
	}
}

// clientTownRoutes is routes.jsonl in the workspace form: every route kept in
// the file's order, the absolute path made town-relative, and the blank line
// gone.
const clientTownRoutes = `{"prefix":"hq-","path":"."}
{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"tb-","path":"toolbox"}
{"prefix":"ab-","path":"toolbox"}
{"prefix":"rt-","path":"."}
`

// TestBuildWorkspaceDocument: the document holds the town's routes, each rig's
// redirect and each rig's beads metadata files with the paths in workspace form,
// and nothing else from the town, and its manifest is the gateway's hash of it.
func TestBuildWorkspaceDocument(t *testing.T) {
	t.Parallel()
	root := clientTown(t)
	doc, err := BuildWorkspaceDocument(root)
	if err != nil {
		t.Fatalf("BuildWorkspaceDocument: %v", err)
	}
	want := clientTownDocument(t, root)
	for name, content := range want {
		if got, ok := doc.Files[name]; !ok {
			t.Errorf("document holds no %s", name)
		} else if got != content {
			t.Errorf("%s = %q, want %q", name, got, content)
		}
	}
	for name, content := range doc.Files {
		if _, ok := want[name]; !ok {
			t.Errorf("document holds %s, which the workspace does not serve: %q", name, content)
		}
	}
	if got, want := doc.ManifestSHA, WorkspaceManifest(want); got != want {
		t.Errorf("manifest = %s, want the hash of the expected files %s", got, want)
	}
	if _, err := validatedManifest(doc); err != nil {
		t.Errorf("the gateway would refuse the document the builder returned: %v", err)
	}
}

// TestBuildWorkspaceDocumentCarriesTownFormulas: the document holds each
// regular .formula.toml of the town's formulas directory under
// .beads/formulas, and a copy under each registered rig's beads directory at
// <rig>/.beads/formulas, and nothing else that directory holds — not the
// installed record, not a README, not a file nested in a subdirectory.
func TestBuildWorkspaceDocumentCarriesTownFormulas(t *testing.T) {
	t.Parallel()
	root := clientTown(t)
	writePath(t, root, ".beads/formulas/alpha.formula.toml", "formula = \"alpha\"\n")
	writePath(t, root, ".beads/formulas/mol-beta.formula.toml", "formula = \"mol-beta\"\ntype = \"workflow\"\n")
	writePath(t, root, ".beads/formulas/.installed.json", `{"formulas":{}}`)
	writePath(t, root, ".beads/formulas/README.md", "hand-written notes\n")
	writePath(t, root, ".beads/formulas/archive/gamma.formula.toml", "formula = \"gamma\"\n")

	doc, err := BuildWorkspaceDocument(root)
	if err != nil {
		t.Fatalf("BuildWorkspaceDocument: %v", err)
	}
	want := clientTownDocument(t, root)
	want[".beads/formulas/alpha.formula.toml"] = "formula = \"alpha\"\n"
	want[".beads/formulas/mol-beta.formula.toml"] = "formula = \"mol-beta\"\ntype = \"workflow\"\n"
	// A rig-scoped bd cook reads the rig's own .beads/formulas, so the same two
	// files are carried under each registered rig as well.
	for _, rig := range []string{"gastown", "toolbox"} {
		want[rig+"/.beads/formulas/alpha.formula.toml"] = "formula = \"alpha\"\n"
		want[rig+"/.beads/formulas/mol-beta.formula.toml"] = "formula = \"mol-beta\"\ntype = \"workflow\"\n"
	}
	for name, content := range want {
		if got, ok := doc.Files[name]; !ok {
			t.Errorf("document holds no %s", name)
		} else if got != content {
			t.Errorf("%s = %q, want %q", name, got, content)
		}
	}
	for name, content := range doc.Files {
		if _, ok := want[name]; ok {
			continue
		}
		t.Errorf("document holds %s, which the workspace does not take: %q", name, content)
	}
	// The manifest the pusher sends must cover the formulas, or the gateway's
	// hash of the same files would differ and refuse the push.
	if got, want := doc.ManifestSHA, WorkspaceManifest(want); got != want {
		t.Errorf("manifest = %s, want the hash of the expected files %s", got, want)
	}
	if _, err := validatedManifest(doc); err != nil {
		t.Errorf("the gateway would refuse the document the builder returned: %v", err)
	}
}

// appendRoute adds one route line to the fixture town's routes.jsonl.
func appendRoute(t *testing.T, root, line string) {
	t.Helper()
	path := filepath.Join(root, ".beads", "routes.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writePath(t, root, ".beads/routes.jsonl", string(data)+line+"\n")
}

// TestBuildWorkspaceDocumentCarriesRouteTargets: a route to a folder that is
// not a registered rig is in the document's routes, and that folder's .beads
// files are pushed with it. bd resolves a route's path to the .beads directory
// inside the folder, so a workspace holding the route but not the database
// answers "no dolt_database" for every bead the prefix names.
func TestBuildWorkspaceDocumentCarriesRouteTargets(t *testing.T) {
	t.Parallel()
	root := clientTown(t)
	appendRoute(t, root, `{"path":"claude-beads","prefix":"claude-"}`)
	writePath(t, root, "claude-beads/.beads/metadata.json", `{"dolt_database":"claude"}`+"\n")
	writePath(t, root, "claude-beads/.beads/config.yaml", "issue-prefix: claude\n")
	writePath(t, root, "claude-beads/.beads/dolt-server.port", "3307")
	writePath(t, root, "claude-beads/.beads/.gt-types-configured", "x")
	writePath(t, root, "claude-beads/notes.md", "not a beads file\n")

	doc, err := BuildWorkspaceDocument(root)
	if err != nil {
		t.Fatalf("BuildWorkspaceDocument: %v", err)
	}
	want := clientTownDocument(t, root)
	want[".beads/routes.jsonl"] = clientTownRoutes + `{"prefix":"claude-","path":"claude-beads"}` + "\n"
	want["claude-beads/.beads/metadata.json"] = `{"dolt_database":"claude"}` + "\n"
	want["claude-beads/.beads/config.yaml"] = "issue-prefix: claude\n"
	want["claude-beads/.beads/dolt-server.port"] = "3307"
	for name, content := range want {
		if got, ok := doc.Files[name]; !ok {
			t.Errorf("document holds no %s", name)
		} else if got != content {
			t.Errorf("%s = %q, want %q", name, got, content)
		}
	}
	for name, content := range doc.Files {
		if _, ok := want[name]; !ok {
			t.Errorf("document holds %s, which the workspace does not serve: %q", name, content)
		}
	}
	if got, wantHash := doc.ManifestSHA, WorkspaceManifest(want); got != wantHash {
		t.Errorf("manifest = %s, want the hash of the expected files %s", got, wantHash)
	}
	if _, err := validatedManifest(doc); err != nil {
		t.Errorf("the gateway would refuse the document the builder returned: %v", err)
	}
}

// TestBuildWorkspaceDocumentSkipsRouteTargetsWithoutADatabase: a route whose
// target the town does not hold — no folder, no beads metadata, or something
// that is not a folder — is kept in routes.jsonl and pushes no file. bd on the
// host reports the same missing database for that prefix, and a route written
// before its folder exists must not fail every other prefix's push.
func TestBuildWorkspaceDocumentSkipsRouteTargetsWithoutADatabase(t *testing.T) {
	t.Parallel()
	const route = `{"path":"claude-beads","prefix":"claude-"}`
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name:  "the target is missing on disk",
			setup: func(*testing.T, string) {},
		},
		{
			name: "the target holds no beads metadata",
			setup: func(t *testing.T, root string) {
				writePath(t, root, "claude-beads/.beads/config.yaml", "issue-prefix: claude\n")
			},
		},
		{
			name: "the target is a regular file",
			setup: func(t *testing.T, root string) {
				writePath(t, root, "claude-beads", "not a folder\n")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := clientTown(t)
			appendRoute(t, root, route)
			tc.setup(t, root)
			doc, err := BuildWorkspaceDocument(root)
			if err != nil {
				t.Fatalf("BuildWorkspaceDocument: %v", err)
			}
			if got, want := doc.Files[".beads/routes.jsonl"], clientTownRoutes+`{"prefix":"claude-","path":"claude-beads"}`+"\n"; got != want {
				t.Errorf("routes.jsonl = %q, want the route kept as %q", got, want)
			}
			for name := range doc.Files {
				if strings.HasPrefix(name, "claude-beads/") {
					t.Errorf("document holds %s, want no file for a target with no database", name)
				}
			}
		})
	}
}

// TestBuildWorkspaceDocumentRefusesAnEscapingRouteTarget: a route whose target
// leaves the town — the folder itself, the .beads directory inside it, or the
// metadata file — is refused rather than read into the pushed tree.
func TestBuildWorkspaceDocumentRefusesAnEscapingRouteTarget(t *testing.T) {
	t.Parallel()
	const route = `{"path":"claude-beads","prefix":"claude-"}`
	cases := []struct {
		name    string
		setup   func(t *testing.T, root string)
		wantErr error
		want    string
	}{
		{
			// The route path itself resolves out, so rewrittenRoutes refuses it
			// before any target file is read.
			name: "the target folder is a symlink out of the town",
			setup: func(t *testing.T, root string) {
				if err := os.Symlink(writeOutsideBeads(t), filepath.Join(root, "claude-beads")); err != nil {
					t.Fatal(err)
				}
			},
			want: "outside the town",
		},
		{
			// A link that names nothing is refused, never read as a folder the
			// town has not made yet.
			name: "the target folder is a dangling symlink",
			setup: func(t *testing.T, root string) {
				if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(root, "claude-beads")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrLinkOutsideTown,
			want:    "names nothing",
		},
		{
			name: "the target's .beads is a symlink out of the town",
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "claude-beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(writeOutsideBeads(t), filepath.Join(root, "claude-beads", ".beads")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrLinkOutsideTown,
			want:    "resolves outside the town",
		},
		{
			name: "the target's metadata.json is a symlink out of the town",
			setup: func(t *testing.T, root string) {
				writePath(t, root, "claude-beads/.beads/metadata.json", "placeholder\n")
				linkFile(t, root, "claude-beads/.beads/metadata.json", filepath.Join(writeOutsideBeads(t), "metadata.json"))
			},
			wantErr: ErrLinkOutsideTown,
			want:    "resolves outside the town",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := clientTown(t)
			appendRoute(t, root, route)
			tc.setup(t, root)
			_, err := BuildWorkspaceDocument(root)
			if err == nil {
				t.Fatal("BuildWorkspaceDocument accepted a route target that leaves the town")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestBuildWorkspaceDocumentPushesARigRouteTargetOnce: the fixture's routes name
// both rigs — toolbox directly, and gastown through its redirect's target — so
// each is also a route target. The document is exactly the rig-only set: the
// route loop adds no second copy, and the manifest is the gateway's hash of the
// files it carries.
func TestBuildWorkspaceDocumentPushesARigRouteTargetOnce(t *testing.T) {
	t.Parallel()
	root := clientTown(t)
	doc, err := BuildWorkspaceDocument(root)
	if err != nil {
		t.Fatalf("BuildWorkspaceDocument: %v", err)
	}
	want := clientTownDocument(t, root)
	if len(doc.Files) != len(want) {
		t.Fatalf("document holds %d files, want the %d rig-only files: %v", len(doc.Files), len(want), mapKeys(doc.Files))
	}
	for name := range doc.Files {
		if _, ok := want[name]; !ok {
			t.Errorf("document holds %s, which the rig and town files do not", name)
		}
	}
	if got, wantHash := doc.ManifestSHA, WorkspaceManifest(want); got != wantHash {
		t.Errorf("manifest = %s, want the hash of the rig-only files %s", got, wantHash)
	}
}

// mapKeys is a document's file paths, in any order.
func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// failingBody is an answer body that fails every read and closes cleanly, so a
// test drives a push whose answer breaks mid-stream.
type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (failingBody) Close() error               { return nil }

// TestBuildWorkspaceDocumentRefusals drives every way the town copy can be
// refused: each leaves a document no gateway would accept, so the builder stops
// rather than pushing a tree bd cannot route.
func TestBuildWorkspaceDocumentRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// spoil edits the fixture town before the build.
		spoil   func(t *testing.T, root string)
		wantErr error
		want    string
		// root replaces the fixture's root, for a row that must refuse before
		// any file is read.
		root string
	}{
		{
			name: "a town root that is not absolute",
			// The path never reaches the fixture: the check is first.
			spoil: func(*testing.T, string) {},
			want:  "must be an absolute path",
			root:  "relative/town",
		},
		{
			// An absolute root that names nothing: the symlink resolution fails
			// before any file is read, and the error keeps the missing-file cause.
			name:    "a town root that does not exist",
			spoil:   func(*testing.T, string) {},
			wantErr: os.ErrNotExist,
			want:    "resolving the town root",
			root:    "/gt-bdgateway-test-no-such-town",
		},
		{
			name:  "a root that is not a town",
			spoil: func(t *testing.T, root string) { removePath(t, root, "mayor/town.json") },
			want:  "not a Gas Town root",
		},
		{
			name:  "no routes file",
			spoil: func(t *testing.T, root string) { removePath(t, root, ".beads/routes.jsonl") },
			want:  "routes.jsonl",
		},
		{
			name: "a routes line that is not a route",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/routes.jsonl", "not json\n")
			},
			want: "holds a line that is not a route",
		},
		{
			name: "a route with no path",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/routes.jsonl", `{"prefix":"gt-"}`+"\n")
			},
			want: "no prefix or path",
		},
		{
			name: "a route whose absolute path is outside the town",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/routes.jsonl", `{"path":"/etc/passwd","prefix":"gt-"}`+"\n")
			},
			want: "outside the town",
		},
		{
			name: "a route path that climbs out of the town",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/routes.jsonl", `{"path":"../other","prefix":"gt-"}`+"\n")
			},
			want: "leaves the town",
		},
		{
			name: "a rig name that is not one path element",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, "mayor/rigs.json", `{"version":1,"rigs":{"../evil":{}}}`)
			},
			want: "is not one path element",
		},
		{
			name:  "a rig with neither a redirect nor beads metadata",
			spoil: func(t *testing.T, root string) { removePath(t, root, "gastown/.beads/redirect") },
			want:  "neither a redirect nor a beads metadata.json",
		},
		{
			name: "an empty redirect",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, "gastown/.beads/redirect", "\n")
			},
			want: "redirect is empty",
		},
		{
			name: "a redirect that leaves the town",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, "gastown/.beads/redirect", "../../../elsewhere/.beads\n")
			},
			want: "outside the town",
		},
		{
			name: "a redirect that does not name a .beads directory",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, "gastown/.beads/redirect", "mayor\n")
			},
			want: "does not name a .beads directory",
		},
		{
			name: "a redirect target whose metadata.json cannot be read",
			spoil: func(t *testing.T, root string) {
				dir := filepath.Join(root, "gastown", "mayor", "rig", ".beads", "metadata.json")
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "reading",
		},
		{
			name: "a redirect file that is a directory",
			spoil: func(t *testing.T, root string) {
				path := filepath.Join(root, "gastown", ".beads", "redirect")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "reading rig \"gastown\"'s redirect",
		},
		{
			name:  "a town with no beads metadata",
			spoil: func(t *testing.T, root string) { removePath(t, root, ".beads/metadata.json") },
			want:  "holds no metadata.json",
		},
		{
			name: "a beads metadata.json that is not a file",
			spoil: func(t *testing.T, root string) {
				path := filepath.Join(root, "toolbox", ".beads", "metadata.json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "neither a redirect nor a beads metadata.json",
		},
		{
			name: "a beads config.yaml that cannot be read",
			spoil: func(t *testing.T, root string) {
				path := filepath.Join(root, "toolbox", ".beads", "config.yaml")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "reading",
		},
		{
			name: "a metadata.json over the file cap",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, "toolbox/.beads/metadata.json", strings.Repeat("m", MaxWorkspaceFileBytes+1))
			},
			wantErr: ErrWorkspaceFileTooLarge,
		},
		{
			name:  "a redirect target with no beads metadata",
			spoil: func(t *testing.T, root string) { removePath(t, root, "gastown/mayor/rig/.beads/metadata.json") },
			want:  "holds no metadata.json",
		},
		{
			name: "a town beads directory that is a symlink out of the town",
			spoil: func(t *testing.T, root string) {
				outside := writeOutsideBeads(t)
				beads := filepath.Join(root, ".beads")
				if err := os.RemoveAll(beads); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, beads); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolves outside the town",
		},
		{
			name: "a rig directory that is a symlink out of the town",
			spoil: func(t *testing.T, root string) {
				// The link's tree is a working rig: a redirect and the
				// metadata it names, so only the location can be what refuses.
				outside := t.TempDir()
				redirect := filepath.Join(outside, ".beads", "redirect")
				if err := os.MkdirAll(filepath.Dir(redirect), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(redirect, []byte("mayor/rig/.beads\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				metadata := filepath.Join(outside, "mayor", "rig", ".beads", "metadata.json")
				if err := os.MkdirAll(filepath.Dir(metadata), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(metadata, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				rigDir := filepath.Join(root, "toolbox")
				if err := os.RemoveAll(rigDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, rigDir); err != nil {
					t.Fatal(err)
				}
				// The routes reach the rig first, so the row drops them: the
				// rig loop is what this row drives.
				writePath(t, root, ".beads/routes.jsonl", `{"path":".","prefix":"hq-"}`+"\n")
			},
			want: "resolves outside the town",
		},
		{
			name: "a rig beads directory that is a symlink out of the town",
			spoil: func(t *testing.T, root string) {
				outside := writeOutsideBeads(t)
				beads := filepath.Join(root, "toolbox", ".beads")
				if err := os.RemoveAll(beads); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, beads); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolves outside the town",
		},
		{
			name: "a redirect target that is a symlink out of the town",
			spoil: func(t *testing.T, root string) {
				outside := writeOutsideBeads(t)
				target := filepath.Join(root, "gastown", "mayor", "rig", ".beads")
				if err := os.RemoveAll(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			},
			want: "outside the town",
		},
		{
			name: "a route path that is a symlink out of the town",
			spoil: func(t *testing.T, root string) {
				outside := writeOutsideBeads(t)
				if err := os.Symlink(outside, filepath.Join(root, "elsewhere")); err != nil {
					t.Fatal(err)
				}
				writePath(t, root, ".beads/routes.jsonl", `{"path":"elsewhere","prefix":"xx-"}`+"\n")
			},
			want: "outside the town",
		},
		{
			name: "a metadata.json that is a symlink to a file outside the town",
			spoil: func(t *testing.T, root string) {
				linkFile(t, root, "toolbox/.beads/metadata.json", filepath.Join(writeOutsideBeads(t), "metadata.json"))
			},
			wantErr: ErrLinkOutsideTown,
			want:    "resolves outside the town",
		},
		{
			name: "a config.yaml that is a symlink to a file outside the town",
			spoil: func(t *testing.T, root string) {
				outside := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(outside, []byte("issue-prefix: x\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				linkFile(t, root, "toolbox/.beads/config.yaml", outside)
			},
			wantErr: ErrLinkOutsideTown,
			want:    "resolves outside the town",
		},
		{
			name: "a dangling config.yaml symlink is refused, not read as absent",
			spoil: func(t *testing.T, root string) {
				linkFile(t, root, "toolbox/.beads/config.yaml", filepath.Join(t.TempDir(), "gone.yaml"))
			},
			wantErr: ErrLinkOutsideTown,
			want:    "names nothing",
		},
		{
			name: "a routes.jsonl that is a symlink to a file outside the town",
			spoil: func(t *testing.T, root string) {
				linkFile(t, root, ".beads/routes.jsonl", filepath.Join(writeOutsideBeads(t), "metadata.json"))
			},
			wantErr: ErrLinkOutsideTown,
			want:    "resolves outside the town",
		},
		{
			name: "a redirect that is a symlink to a file outside the town",
			spoil: func(t *testing.T, root string) {
				outside := filepath.Join(t.TempDir(), "redirect")
				if err := os.WriteFile(outside, []byte("mayor/rig/.beads\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				linkFile(t, root, "gastown/.beads/redirect", outside)
			},
			wantErr: ErrLinkOutsideTown,
			want:    "resolves outside the town",
		},
		{
			name: "a dangling redirect is refused, not read as a rig with no redirect",
			spoil: func(t *testing.T, root string) {
				linkFile(t, root, "gastown/.beads/redirect", filepath.Join(t.TempDir(), "gone"))
			},
			wantErr: ErrLinkOutsideTown,
			want:    "names nothing",
		},
		{
			name: "a rigs.json that is a symlink to a file outside the town",
			spoil: func(t *testing.T, root string) {
				outside := filepath.Join(t.TempDir(), "rigs.json")
				if err := os.WriteFile(outside, []byte(`{"version":1,"rigs":{"evil":{}}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				linkFile(t, root, "mayor/rigs.json", outside)
			},
			wantErr: ErrLinkOutsideTown,
			want:    "mayor/rigs.json",
		},
		{
			// A formula link is refused even when its target is in the town: the
			// workspace carries formulas as regular files only.
			name: "a formula that is a symlink to a file in the town",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, "shared/alpha.formula.toml", "formula = \"alpha\"\n")
				writePath(t, root, ".beads/formulas/alpha.formula.toml", "placeholder\n")
				linkFile(t, root, ".beads/formulas/alpha.formula.toml", filepath.Join(root, "shared", "alpha.formula.toml"))
			},
			wantErr: ErrWorkspaceFileName,
			want:    "is not a regular file",
		},
		{
			name: "a formula that is a symlink to a file outside the town",
			spoil: func(t *testing.T, root string) {
				outside := filepath.Join(t.TempDir(), "alpha.formula.toml")
				if err := os.WriteFile(outside, []byte("formula = \"alpha\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				writePath(t, root, ".beads/formulas/alpha.formula.toml", "placeholder\n")
				linkFile(t, root, ".beads/formulas/alpha.formula.toml", outside)
			},
			wantErr: ErrWorkspaceFileName,
			want:    "is not a regular file",
		},
		{
			name: "a subdirectory named like a formula",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/formulas/alpha.formula.toml/keep", "x")
			},
			wantErr: ErrWorkspaceFileName,
			want:    "is not a regular file",
		},
		{
			name: "a formulas directory that is a symlink out of the town",
			spoil: func(t *testing.T, root string) {
				outside := t.TempDir()
				if err := os.WriteFile(filepath.Join(outside, "alpha.formula.toml"), []byte("formula = \"alpha\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, ".beads", "formulas")); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolves outside the town",
		},
		{
			name: "a formulas directory that is a dangling link is refused, not read as absent",
			spoil: func(t *testing.T, root string) {
				if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(root, ".beads", "formulas")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrLinkOutsideTown,
			want:    "names nothing",
		},
		{
			name: "a formulas path that is a regular file",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/formulas", "not a directory\n")
			},
			want: "the town's formulas directory",
		},
		{
			name: "a formula over the per-file cap",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/formulas/alpha.formula.toml", strings.Repeat("f", MaxWorkspaceFileBytes+1))
			},
			wantErr: ErrWorkspaceFileTooLarge,
		},
		{
			// One more formula at the per-file cap than the total holds: the set
			// is over the total while each file is under its own, so it is
			// refused, never truncated.
			name: "town formulas over the total cap",
			spoil: func(t *testing.T, root string) {
				for i := 0; i < MaxWorkspaceTotalBytes/MaxWorkspaceFileBytes+1; i++ {
					writePath(t, root, fmt.Sprintf(".beads/formulas/bulk-%02d.formula.toml", i), strings.Repeat("f", MaxWorkspaceFileBytes))
				}
			},
			wantErr: ErrWorkspaceTooLarge,
		},
		{
			name: "a route target whose metadata.json is a directory",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/routes.jsonl", `{"path":"claude-beads","prefix":"claude-"}`+"\n")
				if err := os.MkdirAll(filepath.Join(root, "claude-beads", ".beads", "metadata.json"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrWorkspaceFileName,
			want:    "is not a regular file",
		},
		{
			name: "a route target's metadata.json over the file cap",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/routes.jsonl", `{"path":"claude-beads","prefix":"claude-"}`+"\n")
				writePath(t, root, "claude-beads/.beads/metadata.json", strings.Repeat("m", MaxWorkspaceFileBytes+1))
			},
			wantErr: ErrWorkspaceFileTooLarge,
		},
		{
			// The metadata can be neither read nor shown absent: the route target
			// is refused rather than skipped as one holding no database.
			name: "a route target whose .beads is a regular file",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/routes.jsonl", `{"path":"claude-beads","prefix":"claude-"}`+"\n")
				writePath(t, root, "claude-beads/.beads", "not a directory\n")
			},
			want: "reading route target",
		},
		{
			// A route path whose parent is a regular file cannot be stat'd, so
			// the target is refused rather than read as absent.
			name: "a route target under a regular file",
			spoil: func(t *testing.T, root string) {
				writePath(t, root, ".beads/routes.jsonl", `{"path":"claude-beads/sub","prefix":"claude-"}`+"\n")
				writePath(t, root, "claude-beads", "not a directory\n")
			},
			want: "reading route target",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := clientTown(t)
			tc.spoil(t, root)
			if tc.root != "" {
				root = tc.root
			}
			_, err := BuildWorkspaceDocument(root)
			if err == nil {
				t.Fatal("BuildWorkspaceDocument accepted the town")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestBuildWorkspaceDocumentRigFormulasCountTowardTheTotalCap: the same formula
// bytes are carried under the town's beads directory and under each registered
// rig's, so a set that fits as the town's copies alone is over the total once
// the rig copies are added, and the document is refused whole rather than
// truncated. A town with no registered rigs carries the town copies alone and
// pushes the same set.
func TestBuildWorkspaceDocumentRigFormulasCountTowardTheTotalCap(t *testing.T) {
	t.Parallel()
	// clientTown registers two rigs, so each formula is carried three times: at
	// the per-file cap, one more formula than the total holds with the rig
	// copies, while the town's copies alone fit under it.
	const copies = 3
	names := make([]string, MaxWorkspaceTotalBytes/(copies*MaxWorkspaceFileBytes)+1)
	for i := range names {
		names[i] = fmt.Sprintf("bulk-%02d", i)
	}
	spoil := func(t *testing.T, root string) {
		t.Helper()
		for _, name := range names {
			writePath(t, root, ".beads/formulas/"+name+".formula.toml", strings.Repeat("f", MaxWorkspaceFileBytes))
		}
	}

	t.Run("with the registered rigs the set is refused", func(t *testing.T) {
		t.Parallel()
		root := clientTown(t)
		spoil(t, root)
		if _, err := BuildWorkspaceDocument(root); !errors.Is(err, ErrWorkspaceTooLarge) {
			t.Fatalf("BuildWorkspaceDocument error = %v, want %v", err, ErrWorkspaceTooLarge)
		}
	})

	t.Run("as the town's copies alone the same set is pushed", func(t *testing.T) {
		t.Parallel()
		root := clientTown(t)
		spoil(t, root)
		writePath(t, root, "mayor/rigs.json", `{"version":1,"rigs":{}}`)
		doc, err := BuildWorkspaceDocument(root)
		if err != nil {
			t.Fatalf("BuildWorkspaceDocument: %v", err)
		}
		for _, name := range names {
			if _, ok := doc.Files[".beads/formulas/"+name+".formula.toml"]; !ok {
				t.Errorf("document holds no town formula %q", name)
			}
		}
	})
}

// TestBuildWorkspaceDocumentNamesTheFormulaCopiesWhenTheyFillTheCap: a formula
// set that fits as the town's own copy is refused once the copies under the
// registered rigs are added, and the refusal names the arithmetic — the formula
// count, the copy count and the cap — so a town that grew past the cap (more
// formulas, more rigs, or both) reports the growth instead of one formula path
// the running total happened to reach.
func TestBuildWorkspaceDocumentNamesTheFormulaCopiesWhenTheyFillTheCap(t *testing.T) {
	t.Parallel()
	const (
		rigCount     = 2
		formulaCount = 6
		copies       = rigCount + 1
	)
	// Six formulas at the per-file cap are 1.5 MiB: the town's copy fits under
	// the 4 MiB cap, and the three copies the workspace carries do not.
	if townBytes := formulaCount * MaxWorkspaceFileBytes; townBytes*copies <= MaxWorkspaceTotalBytes {
		t.Fatalf("the fixture is %d town formula bytes, not over the cap with %d copies", townBytes, copies)
	}
	root := formulaTown(t, rigCount, formulaCount, MaxWorkspaceFileBytes)
	_, err := BuildWorkspaceDocument(root)
	if !errors.Is(err, ErrWorkspaceTooLarge) {
		t.Fatalf("BuildWorkspaceDocument error = %v, want %v", err, ErrWorkspaceTooLarge)
	}
	for _, want := range []string{"6 formulas", "3 copies", fmt.Sprint(MaxWorkspaceTotalBytes)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}

// TestBuildWorkspaceDocumentCarriesTheTownFormulasUnderEveryRig: the town's
// formula set is carried under the town's beads directory and under each
// registered rig's, so a town with ten rigs pushes eleven copies, about 1.74 MB
// for the town's 21 formulas, and the gateway accepts the document whole. The
// per-file cap still bounds each file; the total cap admits the copies the
// workspace now carries.
func TestBuildWorkspaceDocumentCarriesTheTownFormulasUnderEveryRig(t *testing.T) {
	t.Parallel()
	const (
		formulaCount = 21
		formulaSize  = 7555 // 21 of these are the 158,644 bytes the town's formulas take, to within 11
		rigCount     = 10
	)
	root := formulaTown(t, rigCount, formulaCount, formulaSize)
	doc, err := BuildWorkspaceDocument(root)
	if err != nil {
		t.Fatalf("BuildWorkspaceDocument: %v", err)
	}
	if _, err := validatedManifest(doc); err != nil {
		t.Fatalf("the gateway refuses the document: %v", err)
	}
	var formulaBytes int
	for name, content := range doc.Files {
		if strings.HasSuffix(name, formulaSuffix) {
			formulaBytes += len(content)
		}
	}
	wantCopies := rigCount + 1
	if want := wantCopies * formulaCount * formulaSize; formulaBytes != want {
		t.Errorf("the document carries %d formula bytes, want %d (%d copies of %d)", formulaBytes, want, wantCopies, formulaCount)
	}
	if formulaBytes <= 1<<20 {
		t.Fatalf("the fixture carries %d formula bytes, not over the 1 MiB the total cap used to be", formulaBytes)
	}
	// The store is what a push lands in, so the document must push whole and
	// read back to the same tree.
	s, landing := openTestStore(t)
	if _, err := s.Put(doc); err != nil {
		t.Fatalf("the gateway refused the town's formulas under every rig: %v", err)
	}
	reopened, err := OpenWorkspaceStore(landing)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	if got := reopened.Manifest(); got != doc.ManifestSHA {
		t.Errorf("the tree the gateway stored hashes to %s, want the document's %s", got, doc.ManifestSHA)
	}
}

// TestBuildWorkspaceDocumentRefusesAnUnreadableFormula: a formula the builder
// cannot read refuses the push rather than pushing a workspace without it, which
// would fail only when a sling cooks that formula.
func TestBuildWorkspaceDocumentRefusesAnUnreadableFormula(t *testing.T) {
	t.Parallel()
	root := clientTown(t)
	formula := filepath.Join(root, ".beads", "formulas", "alpha.formula.toml")
	writePath(t, root, ".beads/formulas/alpha.formula.toml", "formula = \"alpha\"\n")
	if err := os.Chmod(formula, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(formula, 0o600) })
	_, err := BuildWorkspaceDocument(root)
	if err == nil || !strings.Contains(err.Error(), "alpha.formula.toml") {
		t.Fatalf("BuildWorkspaceDocument error = %v, want a refusal naming the formula", err)
	}
}

// removePath deletes a fixture file.
func removePath(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

// linkFile replaces a fixture file with a symlink to target.
func linkFile(t *testing.T, root, rel, target string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// TestBuildWorkspaceDocumentFollowsTownLinks: a symlink whose target is another
// file inside the town is read as itself, so the document carries the target's
// content under the link's own path.
func TestBuildWorkspaceDocumentFollowsTownLinks(t *testing.T) {
	t.Parallel()
	root := clientTown(t)
	writePath(t, root, "shared/issue-prefix.yaml", "issue-prefix: shared\n")
	linkFile(t, root, "toolbox/.beads/config.yaml", filepath.Join(root, "shared", "issue-prefix.yaml"))
	doc, err := BuildWorkspaceDocument(root)
	if err != nil {
		t.Fatalf("BuildWorkspaceDocument: %v", err)
	}
	if got, want := doc.Files["toolbox/.beads/config.yaml"], "issue-prefix: shared\n"; got != want {
		t.Errorf("toolbox/.beads/config.yaml = %q, want the linked file's content %q", got, want)
	}
}

// writeOutsideBeads makes a beads directory outside the town, holding the
// metadata the builder would otherwise accept, so a refusal it drives can only
// be about where the directory is.
func writeOutsideBeads(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"dolt_database":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writePath replaces a fixture file's content.
func writePath(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// clientToken writes a token file and returns its path.
func clientToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// jsonResponse is a gateway answer with status and body.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// TestGatewayPusherPush drives one whole push: the request's method, endpoint,
// bearer token and document, and the manifest the answer names.
func TestGatewayPusherPush(t *testing.T) {
	t.Parallel()
	root := clientTown(t)
	var got *http.Request
	var sent WorkspaceDocument
	pusher := GatewayPusher{
		URL:       "http://127.0.0.1:7440",
		TokenFile: clientToken(t, "  s3cret \n"),
		HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
			got = req
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("reading the request body: %v", err)
			}
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatalf("the request body is not a workspace document: %v", err)
			}
			return jsonResponse(http.StatusOK, `{"manifest_sha":"`+sent.ManifestSHA+`"}`), nil
		}),
	}
	manifest, err := pusher.Push(context.Background(), root)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got == nil {
		t.Fatal("the pusher sent no request")
	}
	if got.Method != http.MethodPut {
		t.Errorf("method = %s, want PUT", got.Method)
	}
	if want := "http://127.0.0.1:7440" + WorkspacePath; got.URL.String() != want {
		t.Errorf("url = %s, want %s", got.URL, want)
	}
	if want := "Bearer s3cret"; got.Header.Get("Authorization") != want {
		t.Errorf("Authorization = %q, want %q", got.Header.Get("Authorization"), want)
	}
	want := clientTownDocument(t, root)
	if !equalFiles(sent.Files, want) {
		t.Errorf("the request carried %v, want %v", mapKeys(sent.Files), mapKeys(want))
	}
	if sent.ManifestSHA != WorkspaceManifest(want) {
		t.Errorf("sent manifest = %s, want %s", sent.ManifestSHA, WorkspaceManifest(want))
	}
	if manifest != sent.ManifestSHA {
		t.Errorf("Push returned %s, want the stored manifest %s", manifest, sent.ManifestSHA)
	}
}

// equalFiles reports whether two documents hold the same paths and contents.
func equalFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, content := range b {
		if a[name] != content {
			return false
		}
	}
	return true
}

// TestGatewayPusherRefusals drives every way a push stops before the gateway
// stores anything, and every answer that is not a stored workspace: a caller
// must never read a refusal as a landed push.
func TestGatewayPusherRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// pusher builds the pusher under test from a good one.
		pusher func(t *testing.T, good GatewayPusher) GatewayPusher
		// town is the town root to push; "" is the fixture.
		town    func(t *testing.T) string
		wantErr error
		want    string
		// notContain must be absent from the error, for text that names a
		// secret the test planted.
		notContain string
	}{
		{
			name:    "no url",
			pusher:  func(_ *testing.T, g GatewayPusher) GatewayPusher { g.URL = ""; return g },
			wantErr: ErrNoGatewayURL,
			want:    WorkspaceURLEnv,
		},
		{
			name:   "a url that does not parse",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher { g.URL = "http://127.0.0.1:7440/%zz"; return g },
			want:   "invalid gateway URL",
		},
		{
			// url.Parse's own error quotes the whole URL, so the credentials in it
			// must not reach the message.
			name:       "an unparseable url carrying credentials",
			pusher:     func(_ *testing.T, g GatewayPusher) GatewayPusher { g.URL = "http://pusher:hunter2@[::1"; return g },
			want:       "invalid gateway URL",
			notContain: "hunter2",
		},
		{
			name:   "a url that is not http",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher { g.URL = "ftp://gw:7440"; return g },
			want:   "not an http or https URL",
		},
		{
			name:   "a url adding no host",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher { g.URL = "http://"; return g },
			want:   "not an http or https URL",
		},
		{
			name:   "a url carrying a query",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher { g.URL = "http://127.0.0.1:7440?x=1"; return g },
			want:   "carries a query or fragment",
		},
		{
			name: "a url carrying credentials",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher {
				g.URL = "http://pusher:hunter2@127.0.0.1:7440"
				return g
			},
			want:       "carries credentials",
			notContain: "hunter2",
		},
		{
			name:   "plain http to another machine",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher { g.URL = "http://gw.example.com:7440"; return g },
			want:   "plain http",
		},
		{
			name:    "no token file",
			pusher:  func(_ *testing.T, g GatewayPusher) GatewayPusher { g.TokenFile = ""; return g },
			wantErr: ErrNoTokenFile,
			want:    WorkspaceTokenEnv,
		},
		{
			name: "a token file that is not there",
			pusher: func(t *testing.T, g GatewayPusher) GatewayPusher {
				g.TokenFile = filepath.Join(t.TempDir(), "absent")
				return g
			},
			want: "opening the token file",
		},
		{
			name: "a token file that is a directory",
			pusher: func(t *testing.T, g GatewayPusher) GatewayPusher {
				g.TokenFile = t.TempDir()
				return g
			},
			want: "reading the token file",
		},
		{
			name:    "an empty token file",
			pusher:  func(t *testing.T, g GatewayPusher) GatewayPusher { g.TokenFile = clientToken(t, " \n"); return g },
			wantErr: ErrEmptyToken,
			want:    "token file",
		},
		{
			name: "a token file over the cap",
			pusher: func(t *testing.T, g GatewayPusher) GatewayPusher {
				g.TokenFile = clientToken(t, strings.Repeat("t", tokenFileLimit+1))
				return g
			},
			want: "does not hold one token",
		},
		{
			name: "a gateway that does not answer",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("connection refused")
				})
				return g
			},
			want: "did not answer",
		},
		{
			name: "a gateway refusal naming its code",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return jsonResponse(http.StatusBadRequest, `{"code":"workspace_path_refused","message":"bad path"}`), nil
				})
				return g
			},
			wantErr: ErrGatewayRefused,
			want:    "workspace_path_refused: bad path",
		},
		{
			name: "a gateway refusal with a body that is not one",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return jsonResponse(http.StatusInternalServerError, "boom"), nil
				})
				return g
			},
			wantErr: ErrGatewayRefused,
			want:    "HTTP 500",
		},
		{
			name: "an answer body that cannot be read",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: failingBody{errors.New("reset")}, Header: make(http.Header)}, nil
				})
				return g
			},
			want: "reading the gateway's answer",
		},
		{
			name: "an answer that is not a workspace answer",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return jsonResponse(http.StatusOK, "stored"), nil
				})
				return g
			},
			wantErr: ErrNoManifest,
			want:    "not a workspace answer",
		},
		{
			name: "an answer with no manifest",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return jsonResponse(http.StatusOK, `{"manifest_sha":""}`), nil
				})
				return g
			},
			wantErr: ErrNoManifest,
			want:    "no manifest_sha",
		},
		{
			name: "an answer naming another manifest",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return jsonResponse(http.StatusOK, `{"manifest_sha":"`+strings.Repeat("a", 64)+`"}`), nil
				})
				return g
			},
			wantErr: ErrManifestDiffers,
			want:    "stored a different manifest",
		},
		{
			name:   "a town the builder refuses",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher { return g },
			town:   func(t *testing.T) string { return "relative/town" },
			want:   "must be an absolute path",
		},
		{
			// The builder refuses the link before any request is made: the fixture
			// doer fails the test if a request reaches it.
			name:   "a town whose metadata.json links outside the town",
			pusher: func(_ *testing.T, g GatewayPusher) GatewayPusher { return g },
			town: func(t *testing.T) string {
				root := clientTown(t)
				linkFile(t, root, "toolbox/.beads/metadata.json", filepath.Join(writeOutsideBeads(t), "metadata.json"))
				return root
			},
			wantErr: ErrLinkOutsideTown,
			want:    "resolves outside the town",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := ""
			if tc.town == nil {
				root = clientTown(t)
			} else {
				root = tc.town(t)
			}
			good := GatewayPusher{
				URL:       "http://127.0.0.1:7440",
				TokenFile: clientToken(t, "tok"),
				HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("the pusher sent a request it should have refused")
					return nil, nil
				}),
			}
			if _, err := tc.pusher(t, good).Push(context.Background(), root); err == nil {
				t.Fatal("Push accepted the push")
			} else {
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Errorf("error = %v, want it to wrap %v", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error = %v, want it to name %q", err, tc.want)
				}
				if tc.notContain != "" && strings.Contains(err.Error(), tc.notContain) {
					t.Errorf("error = %v, want it not to echo the URL's credentials", err)
				}
			}
		})
	}
}

// fakePusher is a WorkspacePusher a test installs: it records the town root it
// was asked to push and answers with the manifest or the error the test set.
type fakePusher struct {
	towns    []string
	manifest string
	err      error
}

func (f *fakePusher) Push(_ context.Context, townRoot string) (string, error) {
	f.towns = append(f.towns, townRoot)
	return f.manifest, f.err
}

// TestPusherSlotUsesTheInstalledPusher: the pusher a slot holds is the one its
// push reaches, with the town root the writer handed it, and its manifest or
// failure is the push's own; setting nil removes it again. The test drives its
// own slot, so it moves no package state.
func TestPusherSlotUsesTheInstalledPusher(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	slot := &pusherSlot{}

	good := &fakePusher{manifest: "abc123"}
	slot.set(good)
	manifest, err := slot.push(context.Background(), townRoot)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if manifest != "abc123" {
		t.Errorf("manifest = %q, want the installed pusher's %q", manifest, "abc123")
	}
	if len(good.towns) != 1 || good.towns[0] != townRoot {
		t.Errorf("pushed towns = %v, want the writer's town root %s once", good.towns, townRoot)
	}

	bad := &fakePusher{err: errors.New("the gateway at gw:7440 did not answer")}
	slot.set(bad)
	if manifest, err := slot.push(context.Background(), townRoot); err == nil {
		t.Fatalf("push returned %q for an installed pusher that failed", manifest)
	} else if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("error = %v, want the installed pusher's own failure", err)
	}

	slot.set(nil)
	if manifest, err := slot.push(context.Background(), townRoot); err != nil || manifest != "" {
		t.Errorf("push after removing the pusher = %q, %v; want no manifest and no error", manifest, err)
	}
}

// TestPusherSlotOrdersItsPushes: the slot runs one push to completion before it
// starts the next, so a writer's document is built in the order it is sent and
// an older document cannot land last. The first push is held inside the pusher
// behind a gate the test controls. The second push is started, and the test
// yields a bounded number of times; it must not reach the pusher while the first
// holds the slot. Then the gate opens and the pushes must not overlap.
func TestPusherSlotOrdersItsPushes(t *testing.T) {
	t.Parallel()
	slot := &pusherSlot{}
	gate := make(chan struct{})
	entered := make(chan struct{}, 2)
	var inside, overlapped int32
	slot.set(pusherFunc(func(context.Context, string) (string, error) {
		if atomic.AddInt32(&inside, 1) != 1 {
			atomic.StoreInt32(&overlapped, 1)
		}
		entered <- struct{}{}
		<-gate
		atomic.AddInt32(&inside, -1)
		return "manifest", nil
	}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := slot.push(context.Background(), "first"); err != nil {
			t.Errorf("push first: %v", err)
		}
	}()
	<-entered // the first push is inside the pusher and holds the slot

	started := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		if _, err := slot.push(context.Background(), "second"); err != nil {
			t.Errorf("push second: %v", err)
		}
	}()
	<-started
	// The second push either waits on the slot or reaches the pusher; the pusher
	// signals entry on a channel, so the wait ends the moment it is reached, and
	// a slot that holds the second push is proven by the bound passing with none.
	select {
	case <-entered:
		t.Errorf("the second push reached the pusher while the first held the slot")
	case <-time.After(100 * time.Millisecond): //testpolicy:allow no-sleep — a negative proof: nothing reaches the pusher while the slot is held; the bound is the settle for an asynchronous entry, and the wait ends the moment one arrives
	}
	close(gate)
	wg.Wait()
	if atomic.LoadInt32(&overlapped) != 0 {
		t.Error("two pushes ran at the same time, so an older document can be stored after a newer one")
	}
}

// pusherFunc is a WorkspacePusher a test scripts.
type pusherFunc func(ctx context.Context, townRoot string) (string, error)

func (f pusherFunc) Push(ctx context.Context, townRoot string) (string, error) {
	return f(ctx, townRoot)
}

// TestPushWorkspaceNoPusher: with no pusher installed — every test and every
// town without a gateway — a push is a no-op that succeeds with no hash, so the
// writers that call it behave exactly as they did before the gateway existed.
func TestPushWorkspaceNoPusher(t *testing.T) {
	t.Parallel()
	manifest, err := PushWorkspace(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("PushWorkspace with no pusher: %v", err)
	}
	if manifest != "" {
		t.Errorf("manifest = %q, want none: no gateway was pushed to", manifest)
	}
}

// TestReadTownFileChecksTheFileBeforeReadingIt: a town file is read only when it
// is a regular file within the gateway's per-file cap, so a link to a directory
// or a huge file is refused before the read and before the push slot is taken.
func TestReadTownFileChecksTheFileBeforeReadingIt(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	good := filepath.Join(town, "routes.jsonl")
	if err := os.WriteFile(good, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readTownFile(town, good); err != nil || string(got) != "{}\n" {
		t.Fatalf("a regular small file: got %q, %v", got, err)
	}

	dir := filepath.Join(town, "adir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(town, "link-to-dir")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readTownFile(town, link); !errors.Is(err, ErrWorkspaceFileName) {
		t.Errorf("a link to a directory inside the town: got %v, want ErrWorkspaceFileName", err)
	}

	big := filepath.Join(town, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxWorkspaceFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readTownFile(town, big); !errors.Is(err, ErrWorkspaceFileTooLarge) {
		t.Errorf("a file over the cap: got %v, want ErrWorkspaceFileTooLarge", err)
	}
}

// reportedHashGateway is a fake gateway for the reported-hash read, answering by
// route as the real one does. /readyz needs no token and carries readyzHash on its
// answer. /v1/stats needs the bearer token: without it, or with a token the gateway
// does not hold, the answer is 401. Every /v1/stats answer carries statsHash on the
// workspace header, which the real gateway sets on every response. statsStatus, when
// set, replaces the status /v1/stats answers with.
type reportedHashGateway struct {
	token       string
	readyzHash  string
	statsHash   string
	statsStatus int
}

// Do answers req as the gateway's route does. A request to any other path is 404.
func (g reportedHashGateway) Do(req *http.Request) (*http.Response, error) {
	h := http.Header{}
	status := http.StatusOK
	switch req.URL.Path {
	case readyPath:
		if g.readyzHash != "" {
			h.Set(WorkspaceHashHeader, g.readyzHash)
		}
	case statsPath:
		if g.statsHash != "" {
			h.Set(WorkspaceHashHeader, g.statsHash)
		}
		if req.Header.Get("Authorization") != "Bearer "+g.token {
			status = http.StatusUnauthorized
		}
		if g.statsStatus != 0 {
			status = g.statsStatus
		}
	default:
		status = http.StatusNotFound
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// TestGatewayPusherReportedHash drives the reported-hash read: one GET to /v1/stats
// with the bearer token from the token file, the hash taken from WorkspaceHashHeader
// of a 200 answer, and a refusal for each way the read can fail. A header on the
// unauthenticated /readyz route is never the reported hash.
func TestGatewayPusherReportedHash(t *testing.T) {
	t.Parallel()
	const url = "http://127.0.0.1:7440"
	const token = "s3cret"
	tests := []struct {
		name     string
		url      string
		noToken  bool
		down     bool
		gateway  reportedHashGateway
		want     string
		wantErr  error
		wantText string
	}{
		{name: "the stats header carries the reported hash", url: url, gateway: reportedHashGateway{token: token, statsHash: "manifest-1"}, want: "manifest-1"},
		{name: "no gateway URL is refused", gateway: reportedHashGateway{token: token, statsHash: "manifest-1"}, wantErr: ErrNoGatewayURL},
		{name: "no token file is refused", url: url, noToken: true, gateway: reportedHashGateway{token: token, statsHash: "manifest-1"}, wantErr: ErrNoTokenFile},
		{name: "a 200 with no header is ErrNoReportedHash", url: url, gateway: reportedHashGateway{token: token}, wantErr: ErrNoReportedHash},
		{name: "a header on /readyz alone does not count", url: url, gateway: reportedHashGateway{token: token, readyzHash: "manifest-1"}, wantErr: ErrNoReportedHash},
		{name: "a 401 carrying the header is refused", url: url, gateway: reportedHashGateway{token: "other", statsHash: "manifest-1"}, wantErr: ErrReportedHashRefused, wantText: "refused the doctor's token"},
		{name: "a 403 carrying the header is refused", url: url, gateway: reportedHashGateway{token: token, statsHash: "manifest-1", statsStatus: http.StatusForbidden}, wantErr: ErrReportedHashRefused, wantText: "refused the doctor's token"},
		{name: "a 401 with the header only on /readyz is refused", url: url, gateway: reportedHashGateway{token: "other", readyzHash: "manifest-1"}, wantErr: ErrReportedHashRefused},
		{name: "a 503 is an error that names the status", url: url, gateway: reportedHashGateway{token: token, statsHash: "manifest-1", statsStatus: http.StatusServiceUnavailable}, wantText: "answered 503"},
		{name: "an unreachable gateway is an error", url: url, down: true, gateway: reportedHashGateway{token: token, statsHash: "manifest-1"}, wantText: "did not answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got *http.Request
			pusher := GatewayPusher{
				URL: tt.url,
				HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
					got = req
					if tt.down {
						return nil, errors.New("connection refused")
					}
					return tt.gateway.Do(req)
				}),
			}
			if !tt.noToken {
				pusher.TokenFile = clientToken(t, "  "+token+" \n")
			}
			hash, err := pusher.ReportedHash(context.Background())
			if tt.want == "" {
				if err == nil {
					t.Fatalf("ReportedHash = %q, nil error; want a refusal", hash)
				}
				if hash != "" {
					t.Errorf("hash = %q with an error, want empty", hash)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("error = %v, want %v", err, tt.wantErr)
				}
				if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantText)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReportedHash: %v", err)
			}
			if hash != tt.want {
				t.Errorf("hash = %q, want %q", hash, tt.want)
			}
			if got.Method != http.MethodGet || got.URL.String() != url+"/v1/stats" {
				t.Errorf("request = %s %s, want GET %s/v1/stats", got.Method, got.URL, url)
			}
			if auth := got.Header.Get("Authorization"); auth != "Bearer "+token {
				t.Errorf("Authorization = %q, want the bearer token from the token file", auth)
			}
		})
	}
}
