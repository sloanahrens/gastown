package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// pushDoerFunc is an HTTPDoer a test scripts, so a push runs against the
// response the func returns and opens no socket (docs/testing.md, no-network).
type pushDoerFunc func(*http.Request) (*http.Response, error)

func (f pushDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// writePushTown writes the fixture town a push reads: the town's own beads, and
// one rig whose .beads is a redirect into mayor/rig. It returns the root.
func writePushTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	write("mayor/town.json", `{"type":"town","version":1,"name":"fx"}`)
	write("mayor/rigs.json", `{"version":1,"rigs":{"gastown":{}}}`)
	write(".beads/metadata.json", `{"dolt_database":"hq"}`+"\n")
	write(".beads/config.yaml", "issue-prefix: hq\n")
	write(".beads/routes.jsonl", `{"path":".","prefix":"hq-"}`+"\n"+`{"path":"gastown/mayor/rig","prefix":"gt-"}`+"\n")
	write("gastown/.beads/redirect", "mayor/rig/.beads\n")
	write("gastown/mayor/rig/.beads/metadata.json", `{"dolt_database":"gt"}`+"\n")
	write("gastown/mayor/rig/.beads/config.yaml", "issue-prefix: gt\n")
	write("gastown/mayor/rig/.beads/dolt-server.port", "3307")
	return root
}

// writePushToken writes the pusher's token file and returns its path.
func writePushToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}
	return path
}

// pushDeps is the set of seams a command test drives: a town finder that names
// the fixture, no environment, the scripted doer and a buffer for the answer.
func pushDeps(root string, doer bdgateway.HTTPDoer) (workspacePushDeps, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return workspacePushDeps{
		findTown: func() (string, error) { return root, nil },
		getenv:   func(string) string { return "" },
		doer:     doer,
		out:      out,
	}, out
}

// TestWorkspacePushBuildsDocument: `gt bd workspace push` sends the document the
// gateway accepts — the town's routes, each rig's redirect and each rig's beads
// metadata files with the paths in workspace form, and nothing else — and prints
// the manifest hash the gateway answered with.
func TestWorkspacePushBuildsDocument(t *testing.T) {
	t.Parallel()
	root := writePushTown(t)
	var sent bdgateway.WorkspaceDocument
	deps, out := pushDeps(root, pushDoerFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("reading the request body: %v", err)
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("the request body is not a workspace document: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"manifest_sha":"` + sent.ManifestSHA + `"}`)),
			Header:     make(http.Header),
		}, nil
	}))
	cmd := newWorkspacePushCmd(deps)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"--town-root", root, "--gateway-url", "http://gw:7440", "--token-file", writePushToken(t, "tok")})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gt bd workspace push: %v", err)
	}

	want := map[string]string{
		".beads/metadata.json":                      `{"dolt_database":"hq"}` + "\n",
		".beads/config.yaml":                        "issue-prefix: hq\n",
		".beads/routes.jsonl":                       `{"prefix":"hq-","path":"."}` + "\n" + `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n",
		"gastown/.beads/redirect":                   "/ws/gastown/mayor/rig/.beads\n",
		"gastown/mayor/rig/.beads/metadata.json":    `{"dolt_database":"gt"}` + "\n",
		"gastown/mayor/rig/.beads/config.yaml":      "issue-prefix: gt\n",
		"gastown/mayor/rig/.beads/dolt-server.port": "3307",
	}
	for name, content := range want {
		if got, ok := sent.Files[name]; !ok {
			t.Errorf("the document holds no %s", name)
		} else if got != content {
			t.Errorf("%s = %q, want %q", name, got, content)
		}
	}
	for name := range sent.Files {
		if _, ok := want[name]; !ok {
			t.Errorf("the document holds %s, which the workspace does not serve", name)
		}
	}
	if sent.ManifestSHA != bdgateway.WorkspaceManifest(want) {
		t.Errorf("sent manifest = %s, want the hash of the expected files %s", sent.ManifestSHA, bdgateway.WorkspaceManifest(want))
	}
	if got := out.String(); got != "workspace "+sent.ManifestSHA+"\n" {
		t.Errorf("the command printed %q, want the manifest hash it stored", got)
	}
}

// TestWorkspacePushSends: the request is a PUT to the gateway's workspace
// endpoint, carrying the token from the token file, and the command prints the
// manifest hash the gateway returns.
func TestWorkspacePushSends(t *testing.T) {
	t.Parallel()
	root := writePushTown(t)
	var got *http.Request
	var sent bdgateway.WorkspaceDocument
	deps, out := pushDeps(root, pushDoerFunc(func(req *http.Request) (*http.Response, error) {
		got = req
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("reading the request body: %v", err)
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("the request body is not a workspace document: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"manifest_sha":"` + sent.ManifestSHA + `"}`)),
			Header:     make(http.Header),
		}, nil
	}))
	// The URL and token come from the environment when no flag names them, as
	// gt-wrap's bdgateway target reads them.
	deps.getenv = func(key string) string {
		switch key {
		case bdgateway.WorkspaceURLEnv:
			return "https://gw.test"
		case bdgateway.WorkspaceTokenEnv:
			return writePushToken(t, "env-token")
		}
		return ""
	}
	if err := runWorkspacePush(context.Background(), deps, root, "", ""); err != nil {
		t.Fatalf("runWorkspacePush: %v", err)
	}
	if got == nil {
		t.Fatal("the command sent no request")
	}
	if got.Method != http.MethodPut {
		t.Errorf("method = %s, want PUT", got.Method)
	}
	if want := "https://gw.test" + bdgateway.WorkspacePath; got.URL.String() != want {
		t.Errorf("url = %s, want %s", got.URL, want)
	}
	if want := "Bearer env-token"; got.Header.Get("Authorization") != want {
		t.Errorf("Authorization = %q, want %q", got.Header.Get("Authorization"), want)
	}
	if want := "workspace " + sent.ManifestSHA + "\n"; out.String() != want {
		t.Errorf("printed %q, want the manifest hash the gateway returned %q", out.String(), want)
	}
}

// TestWorkspacePushRefused: a gateway that refuses the document fails the
// command with the gateway's own line, and nothing is printed as if the push
// had landed.
func TestWorkspacePushRefused(t *testing.T) {
	t.Parallel()
	root := writePushTown(t)
	deps, out := pushDeps(root, pushDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"code":"workspace_manifest_mismatch","message":"sent x, files hash to y"}`)),
			Header:     make(http.Header),
		}, nil
	}))
	cmd := newWorkspacePushCmd(deps)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"--town-root", root, "--gateway-url", "http://gw:7440", "--token-file", writePushToken(t, "tok")})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("gt bd workspace push exited 0 on a refused push")
	}
	if !errors.Is(err, bdgateway.ErrGatewayRefused) {
		t.Errorf("error = %v, want it to wrap %v", err, bdgateway.ErrGatewayRefused)
	}
	for _, want := range []string{"workspace_manifest_mismatch", "sent x, files hash to y"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want the gateway's line %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("the command printed %q after a refused push", out.String())
	}
}

// TestWorkspacePushNeedsItsSettings: a push with no gateway URL, no token file,
// no town or no output sink is refused by name rather than sent somewhere.
func TestWorkspacePushNeedsItsSettings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// deps builds the seams; root and flags are the run's other inputs.
		deps     func(t *testing.T, root string) (workspacePushDeps, string, string, string)
		wantErr  error
		wantText string
	}{
		{
			name: "no gateway url",
			deps: func(t *testing.T, root string) (workspacePushDeps, string, string, string) {
				deps, _ := pushDeps(root, nil)
				return deps, root, "", writePushToken(t, "tok")
			},
			wantErr:  bdgateway.ErrNoGatewayURL,
			wantText: bdgateway.WorkspaceURLEnv,
		},
		{
			name: "no token file",
			deps: func(t *testing.T, root string) (workspacePushDeps, string, string, string) {
				deps, _ := pushDeps(root, nil)
				return deps, root, "http://gw:7440", ""
			},
			wantErr:  bdgateway.ErrNoTokenFile,
			wantText: bdgateway.WorkspaceTokenEnv,
		},
		{
			name: "a town that cannot be found",
			deps: func(t *testing.T, root string) (workspacePushDeps, string, string, string) {
				deps, _ := pushDeps(root, nil)
				deps.findTown = func() (string, error) { return "", errors.New("no town above this directory") }
				return deps, "", "http://gw:7440", writePushToken(t, "tok")
			},
			wantText: "finding the town",
		},
		{
			name: "no town finder wired",
			deps: func(t *testing.T, root string) (workspacePushDeps, string, string, string) {
				deps, _ := pushDeps(root, nil)
				deps.findTown = nil
				return deps, "", "http://gw:7440", writePushToken(t, "tok")
			},
			wantText: "pass --town-root",
		},
		{
			name: "no output writer wired",
			deps: func(t *testing.T, root string) (workspacePushDeps, string, string, string) {
				deps, _ := pushDeps(root, nil)
				deps.out = nil
				return deps, root, "http://gw:7440", writePushToken(t, "tok")
			},
			wantText: "no output writer is wired",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writePushTown(t)
			deps, townRoot, url, token := tc.deps(t, root)
			err := runWorkspacePush(context.Background(), deps, townRoot, url, token)
			if err == nil {
				t.Fatal("the push ran")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantText)
			}
		})
	}
}

// TestWorkspacePusherNilNoop: with no gateway configured the pusher every
// writer calls is nil, and a push through it is a no-op that succeeds with no
// manifest — the state that leaves every existing test and Mac-only path alone.
func TestWorkspacePusherNilNoop(t *testing.T) {
	t.Parallel()
	manifest, err := bdgateway.PushWorkspace(context.Background(), writePushTown(t))
	if err != nil {
		t.Fatalf("PushWorkspace with no pusher installed: %v", err)
	}
	if manifest != "" {
		t.Errorf("manifest = %q, want none: no gateway is configured", manifest)
	}
}
