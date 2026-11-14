package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// doerFunc is an HTTPDoer a test scripts: the request runs against the response
// the func returns, so no test opens a socket (docs/testing.md, no-network).
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// clientTown writes the fixture town every builder test reads: the town's own
// beads, a rig whose .beads is a redirect into mayor/rig, and a rig that holds
// its files in its own .beads. Each directory keeps a file the workspace does
// not serve, so a test catches a document that copies the directory rather than
// the five files.
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
	// A bounded run of yields, no clock: a second push the slot lets through
	// reaches the pusher within them, and one the slot holds does not.
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	if n := len(entered); n != 0 {
		t.Errorf("the second push reached the pusher while the first held the slot")
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
