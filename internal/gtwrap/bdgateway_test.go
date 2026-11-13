package gtwrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// doerFunc is an HTTPDoer a test scripts: the fake gateway is the response the
// func returns, so no test opens a socket.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// gatewayResponse is a 200 response whose body is the frames, as the gateway
// streams them.
func gatewayResponse(t *testing.T, frames ...bdgateway.Frame) *http.Response {
	t.Helper()
	var body bytes.Buffer
	for _, frame := range frames {
		if err := bdgateway.WriteFrame(&body, frame); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(&body), Header: make(http.Header)}
}

// statusResponse is a refusal with status and body.
func statusResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// writeGatewayToken writes a token file and returns its path.
func writeGatewayToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

// gatewayTarget is a fully wired target whose request the row's doer answers.
func gatewayTarget(t *testing.T, town, tokenFile string, doer HTTPDoer) (BdGateway, *bytes.Buffer, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stdout, stderr, note := &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	return BdGateway{
		Town:      town,
		URL:       "http://gw:7440",
		TokenFile: tokenFile,
		Caller:    "daemon.test",
		TimeoutMS: 90000,
		HTTP:      doer,
		Stdin:     strings.NewReader("the stdin body"),
		Stdout:    stdout,
		Stderr:    stderr,
		Note:      note,
	}, stdout, stderr, note
}

// TestBdGatewayTarget drives one whole call through the target: the head names
// the bd arguments after bd, the screened env, the rig and the town-relative
// directory; the token travels as the bearer header; the frames reach the
// caller's streams; and the exit frame's status is returned unchanged.
func TestBdGatewayTarget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tokenFile := writeGatewayToken(t, "sekrit\n")
	var head bdgateway.Request
	var stdin string
	var headers http.Header
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", req.Method)
		}
		if got, want := req.URL.String(), "http://gw:7440/v1/exec"; got != want {
			t.Errorf("request URL = %q, want %q", got, want)
		}
		headers = req.Header.Clone()
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		line, rest, ok := bytes.Cut(body, []byte("\n"))
		if !ok {
			t.Fatalf("request body %q has no head line", body)
		}
		if err := json.Unmarshal(line, &head); err != nil {
			t.Fatalf("head %q is not JSON: %v", line, err)
		}
		stdin = string(rest)
		return gatewayResponse(t,
			bdgateway.Frame{Kind: bdgateway.FrameStdout, Payload: []byte("the bead\n")},
			bdgateway.Frame{Kind: bdgateway.FrameStderr, Payload: []byte("a warning\n")},
			bdgateway.ExitFrame(7),
		), nil
	})
	g, stdout, stderr, _ := gatewayTarget(t, town, tokenFile, doer)
	call := Call{
		Argv: []string{"bd", "--allow-stale", "show", "gt-abc", "--json"},
		Env: []string{
			"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads"),
			"BD_ACTOR=town/crew/dipper",
			"BD_READONLY=1",
			"GT_TOWN_ROOT=" + town,
			"BEADS_DOLT_SERVER_HOST=elsewhere",
			"BEADS_DOLT_SERVER_PORT=9999",
		},
		Dir: filepath.Join(town, "gastown", "crew", "dipper"),
	}
	exit, err := g.Launch(call)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if exit != 7 {
		t.Fatalf("exit = %d, want the exit frame's 7", exit)
	}
	if got, want := head.Argv, []string{"--allow-stale", "show", "gt-abc", "--json"}; !slices.Equal(got, want) {
		t.Errorf("head argv = %q, want %q (bd itself is never sent)", got, want)
	}
	wantEnv := map[string]string{"BD_ACTOR": "town/crew/dipper", "BD_READONLY": "1"}
	if len(head.Env) != len(wantEnv) || head.Env["BD_ACTOR"] != wantEnv["BD_ACTOR"] || head.Env["BD_READONLY"] != wantEnv["BD_READONLY"] {
		t.Errorf("head env = %v, want only the allowlisted %v", head.Env, wantEnv)
	}
	if head.Rig != "gastown" {
		t.Errorf("head rig = %q, want %q", head.Rig, "gastown")
	}
	if head.Cwd != "gastown/crew/dipper" {
		t.Errorf("head cwd = %q, want %q", head.Cwd, "gastown/crew/dipper")
	}
	if head.TimeoutMS != 90000 {
		t.Errorf("head timeout_ms = %d, want 90000", head.TimeoutMS)
	}
	if got, want := headers.Get("Authorization"), "Bearer sekrit"; got != want {
		t.Errorf("Authorization = %q, want %q (the file's trailing newline is trimmed)", got, want)
	}
	if got, want := headers.Get("Content-Type"), "application/vnd.gt.bdexec+json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := headers.Get("X-Gt-Caller"), "daemon.test"; got != want {
		t.Errorf("X-Gt-Caller = %q, want %q", got, want)
	}
	if stdin != "the stdin body" {
		t.Errorf("stdin after the head = %q, want the call's reader", stdin)
	}
	if got, want := stdout.String(), "the bead\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "a warning\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestBdGatewayTranslation: BEADS_DIR names the rig and the working directory is
// sent relative to the town; a directory outside the town runs in the town root
// with a note; a BEADS_DIR the workspace cannot hold is refused before any
// request.
func TestBdGatewayTranslation(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	inside := filepath.Join(town, "gastown", "crew", "dipper")
	outside := t.TempDir()
	tests := []struct {
		name     string
		env      []string
		dir      string
		wantRig  string
		wantCwd  string
		wantNote string
		wantErr  string
	}{
		{
			name: "rig and directory",
			env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
			dir:  inside, wantRig: "gastown", wantCwd: "gastown/crew/dipper",
		},
		{
			name: "town beads",
			env:  []string{"BEADS_DIR=" + filepath.Join(town, ".beads")},
			dir:  filepath.Join(town, "crew", "overseer"), wantRig: "", wantCwd: "crew/overseer",
		},
		{
			name: "directory outside the town",
			env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
			dir:  outside, wantRig: "gastown", wantCwd: ".", wantNote: "outside the town",
		},
		{
			name: "no BEADS_DIR",
			env:  nil, dir: inside, wantErr: "sets no BEADS_DIR",
		},
		{
			name: "relative BEADS_DIR",
			env:  []string{"BEADS_DIR=gastown/.beads"}, dir: inside, wantErr: "not an absolute path",
		},
		{
			name: "BEADS_DIR outside the town",
			env:  []string{"BEADS_DIR=" + filepath.Join(outside, "gastown", ".beads")},
			dir:  inside, wantErr: "outside the town",
		},
		{
			name: "BEADS_DIR that is not a .beads directory",
			env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", "beads")},
			dir:  inside, wantErr: "does not name a .beads directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var head bdgateway.Request
			called := false
			doer := doerFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				line, _, _ := bytes.Cut(body, []byte("\n"))
				if err := json.Unmarshal(line, &head); err != nil {
					t.Fatalf("head is not JSON: %v", err)
				}
				return gatewayResponse(t, bdgateway.ExitFrame(0)), nil
			})
			g, _, _, note := gatewayTarget(t, town, writeGatewayToken(t, "t"), doer)
			exit, err := g.Launch(Call{Argv: []string{"bd", "show", "gt-x"}, Env: tt.env, Dir: tt.dir})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Launch error = %v, want it to name %q", err, tt.wantErr)
				}
				if exit != exitDown {
					t.Errorf("exit = %d, want %d", exit, exitDown)
				}
				if called {
					t.Error("a refused call still reached the gateway")
				}
				return
			}
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if exit != 0 {
				t.Errorf("exit = %d, want 0", exit)
			}
			if head.Rig != tt.wantRig {
				t.Errorf("head rig = %q, want %q", head.Rig, tt.wantRig)
			}
			if head.Cwd != tt.wantCwd {
				t.Errorf("head cwd = %q, want %q", head.Cwd, tt.wantCwd)
			}
			if tt.wantNote == "" {
				if note.Len() != 0 {
					t.Errorf("note = %q, want none", note)
				}
			} else if !strings.Contains(note.String(), tt.wantNote) {
				t.Errorf("note = %q, want it to name %q", note, tt.wantNote)
			}
		})
	}
}

// TestBdGatewayErrors: every way a call cannot be made gives the caller one line
// that names what failed, and a gateway error frame's exit status still passes
// through.
func TestBdGatewayErrors(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	beadsDir := filepath.Join(town, "gastown", ".beads")
	baseCall := func() Call {
		return Call{Argv: []string{"bd", "show", "gt-x"}, Env: []string{"BEADS_DIR=" + beadsDir}, Dir: town}
	}
	tests := []struct {
		name     string
		mutate   func(*BdGateway, *Call)
		wantErr  string
		wantExit int
	}{
		{
			name: "missing token file",
			mutate: func(g *BdGateway, _ *Call) {
				g.TokenFile = filepath.Join(t.TempDir(), "gone.token")
			},
			wantErr: "read the bd gateway token file", wantExit: exitDown,
		},
		{
			name: "empty token file",
			mutate: func(g *BdGateway, _ *Call) {
				g.TokenFile = writeGatewayToken(t, "\n")
			},
			wantErr: "is empty", wantExit: exitDown,
		},
		{
			name: "unreachable URL",
			mutate: func(g *BdGateway, _ *Call) {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("dial tcp: connection refused")
				})
			},
			wantErr: "did not answer", wantExit: exitDown,
		},
		{
			name: "401",
			mutate: func(g *BdGateway, _ *Call) {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return statusResponse(http.StatusUnauthorized, `{"code":"unauthorized","message":"the token is not known"}`), nil
				})
			},
			wantErr: "HTTP 401: unauthorized: the token is not known", wantExit: exitDown,
		},
		{
			name: "403 with no JSON",
			mutate: func(g *BdGateway, _ *Call) {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return statusResponse(http.StatusForbidden, "no"), nil
				})
			},
			wantErr: "HTTP 403", wantExit: exitDown,
		},
		{
			name: "error frame then exit 125",
			mutate: func(g *BdGateway, _ *Call) {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return gatewayResponse(t,
						bdgateway.Frame{Kind: bdgateway.FrameError, Payload: []byte(`{"code":"policy","message":"verb not allowed"}`)},
						bdgateway.ExitFrame(125),
					), nil
				})
			},
			wantErr: "policy: verb not allowed", wantExit: 125,
		},
		{
			name: "stream ends before an exit frame",
			mutate: func(g *BdGateway, _ *Call) {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return gatewayResponse(t, bdgateway.Frame{Kind: bdgateway.FrameStdout, Payload: []byte("partial")}), nil
				})
			},
			wantErr: "ended before an exit status", wantExit: exitDown,
		},
		{
			name: "BD_ALLOW_NEW_DATABASE",
			mutate: func(_ *BdGateway, c *Call) {
				c.Env = append(c.Env, "BD_ALLOW_NEW_DATABASE=1")
			},
			wantErr: "BD_ALLOW_NEW_DATABASE", wantExit: exitDown,
		},
		{
			name: "a call that is not bd",
			mutate: func(_ *BdGateway, c *Call) {
				c.Argv = []string{"gt", "status"}
			},
			wantErr: "the bdgateway target runs bd", wantExit: exitDown,
		},
		{
			name: "no token file configured",
			mutate: func(g *BdGateway, _ *Call) {
				g.TokenFile = ""
			},
			wantErr: "names no token file", wantExit: exitDown,
		},
		{
			name: "no output writer",
			mutate: func(g *BdGateway, _ *Call) {
				g.Stdout = nil
			},
			wantErr: "no output writer", wantExit: exitDown,
		},
		{
			name: "no note writer",
			mutate: func(g *BdGateway, _ *Call) {
				g.Note = nil
			},
			wantErr: "no note writer", wantExit: exitDown,
		},
		{
			name: "relative town root",
			mutate: func(g *BdGateway, _ *Call) {
				g.Town = "gt"
			},
			wantErr: "must be an absolute path", wantExit: exitDown,
		},
		{
			name: "URL that is not http",
			mutate: func(g *BdGateway, _ *Call) {
				g.URL = "ftp://gw:7440"
			},
			wantErr: "not an http or https URL", wantExit: exitDown,
		},
		{
			name: "URL with a query",
			mutate: func(g *BdGateway, _ *Call) {
				g.URL = "http://gw:7440?x=1"
			},
			wantErr: "query or fragment", wantExit: exitDown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g, _, _, _ := gatewayTarget(t, town, writeGatewayToken(t, "t"), doerFunc(func(*http.Request) (*http.Response, error) {
				return gatewayResponse(t, bdgateway.ExitFrame(0)), nil
			}))
			call := baseCall()
			tt.mutate(&g, &call)
			exit, err := g.Launch(call)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Launch error = %v, want it to name %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("the error is not one line: %q", err)
			}
			if exit != tt.wantExit {
				t.Errorf("exit = %d, want %d", exit, tt.wantExit)
			}
		})
	}
}

// TestBdGatewayDoctor: the doctor names the URL, the token file and the first
// step that fails, and asks /readyz with the token, never /v1/exec.
func TestBdGatewayDoctor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name      string
		mutate    func(*BdGateway)
		wantRung  Rung
		wantParts []string
		wantErr   string
	}{
		{
			name:      "ready",
			wantRung:  RungExec,
			wantParts: []string{"answers /readyz with the token in"},
		},
		{
			name: "token refused",
			mutate: func(g *BdGateway) {
				g.HTTP = doerFunc(func(req *http.Request) (*http.Response, error) {
					return statusResponse(http.StatusUnauthorized, ""), nil
				})
			},
			wantRung:  RungDown,
			wantParts: []string{"refused the token in", "HTTP 401"},
		},
		{
			name: "not ready",
			mutate: func(g *BdGateway) {
				g.HTTP = doerFunc(func(req *http.Request) (*http.Response, error) {
					return statusResponse(http.StatusServiceUnavailable, ""), nil
				})
			},
			wantRung:  RungDown,
			wantParts: []string{"not ready to run bd", "HTTP 503"},
		},
		{
			name: "unreachable",
			mutate: func(g *BdGateway) {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("dial tcp: connection refused")
				})
			},
			wantRung:  RungDown,
			wantParts: []string{"did not answer"},
		},
		{
			name: "missing token file",
			mutate: func(g *BdGateway) {
				g.TokenFile = filepath.Join(t.TempDir(), "gone.token")
			},
			wantRung:  RungDown,
			wantParts: []string{"read the bd gateway token file"},
		},
		{
			name: "invalid URL",
			mutate: func(g *BdGateway) {
				g.URL = "http://"
			},
			wantErr: "not an http or https URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ready := false
			g, _, _, _ := gatewayTarget(t, town, writeGatewayToken(t, "t"), doerFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == gatewayExecPath {
					t.Errorf("the doctor ran a call at %s", req.URL.Path)
				}
				if req.URL.Path != gatewayReadyPath {
					t.Errorf("doctor path = %s, want %s", req.URL.Path, gatewayReadyPath)
				}
				if got, want := req.Header.Get("Authorization"), "Bearer t"; got != want {
					t.Errorf("Authorization = %q, want %q", got, want)
				}
				ready = true
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
			}))
			if tt.mutate != nil {
				tt.mutate(&g)
			}
			diag, err := g.Doctor()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Doctor error = %v, want it to name %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Doctor: %v", err)
			}
			if diag.Rung != tt.wantRung {
				t.Errorf("rung = %s, want %s (reason: %s)", diag.Rung, tt.wantRung, diag.Reason)
			}
			if diag.URL != g.URL {
				t.Errorf("diagnosis url = %q, want %q", diag.URL, g.URL)
			}
			if diag.TokenFile != g.TokenFile {
				t.Errorf("diagnosis token file = %q, want %q", diag.TokenFile, g.TokenFile)
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(diag.Reason, part) {
					t.Errorf("reason %q does not name %q", diag.Reason, part)
				}
			}
			if tt.wantRung == RungExec && !ready {
				t.Error("the exec diagnosis did not ask /readyz")
			}
		})
	}
}
