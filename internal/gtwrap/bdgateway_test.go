package gtwrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
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

// errWriter fails every write, so a test drives the output write failure.
type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

// errReader fails every read, so a test drives a broken response stream.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

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
// directory; the token travels as the bearer header; the caller's stdin, which
// the call reads, follows the head; the frames reach the caller's streams; and
// the exit frame's status is returned unchanged.
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
		Argv: []string{"bd", "--allow-stale", "comments", "add", "gt-abc", "-f", "-"},
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
	if got, want := head.Argv, []string{"--allow-stale", "comments", "add", "gt-abc", "-f", "-"}; !slices.Equal(got, want) {
		t.Errorf("head argv = %q, want %q (bd itself is never sent)", got, want)
	}
	wantEnv := map[string]string{"BD_ACTOR": "town/crew/dipper", "BD_READONLY": "1"}
	if len(head.Env) != len(wantEnv) || head.Env["BD_ACTOR"] != wantEnv["BD_ACTOR"] || head.Env["BD_READONLY"] != wantEnv["BD_READONLY"] {
		t.Errorf("head env = %d entries, want only the allowlisted %d", len(head.Env), len(wantEnv))
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

// recordingStdin is a caller's stdin a call must not read: a read is recorded
// and ends the stream at once, so a call that reads it still returns and the
// test reports the read, while a call that reads none never touches it (a pipe
// that never closes would hold a reading call to the gateway's read deadline).
type recordingStdin struct{ read bool }

func (r *recordingStdin) Read([]byte) (int, error) {
	r.read = true
	return 0, io.EOF
}

// TestBdGatewaySendsNoStdinWhenTheCallReadsNone: a call whose argv names no "-"
// value of a file flag sends the head line alone and never reads the caller's
// stdin, so it is not held to the gateway's read deadline (gt-vojyy8). That
// includes a "-" value of any other flag, a positional "-", and anything after
// "--".
func TestBdGatewaySendsNoStdinWhenTheCallReadsNone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name string
		argv []string
	}{
		{name: "no argument reads stdin", argv: []string{"bd", "show", "gt-x", "--json"}},
		{name: "a dash value after --", argv: []string{"bd", "show", "gt-x", "--", "--body-file", "-"}},
		{name: "a non-file flag's separate dash value", argv: []string{"bd", "list", "--assignee", "-"}},
		{name: "a non-file flag's attached dash value", argv: []string{"bd", "list", "--assignee=-"}},
		{name: "a positional dash", argv: []string{"bd", "show", "gt-x", "-"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stdin := &recordingStdin{}
			var body []byte
			g, _, _, _ := gatewayTarget(t, town, writeGatewayToken(t, "t"), doerFunc(func(req *http.Request) (*http.Response, error) {
				var err error
				body, err = io.ReadAll(req.Body)
				if err != nil {
					t.Errorf("read the request body: %v", err)
				}
				return gatewayResponse(t, bdgateway.ExitFrame(0)), nil
			}))
			g.Stdin = stdin
			call := Call{
				Argv: tt.argv,
				Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
				Dir:  town,
			}
			if _, err := g.Launch(call); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if stdin.read {
				t.Error("the call read the caller's stdin")
			}
			_, rest, ok := bytes.Cut(body, []byte("\n"))
			if !ok {
				t.Fatalf("request body %q has no head line", body)
			}
			if len(rest) != 0 {
				t.Errorf("body after the head = %q, want none", rest)
			}
		})
	}
}

// TestBdGatewaySendsStdinWhenTheCallReadsIt: every form bd takes its stdin in
// carries the caller's stdin bytes after the head line, and a stdin the call
// itself carries is sent whatever the arguments say.
func TestBdGatewaySendsStdinWhenTheCallReadsIt(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tests := []struct {
		name string
		argv []string
		// callStdin is the stdin the call struct carries; empty uses the target's.
		callStdin string
		want      string
	}{
		{
			name: "--body-file -",
			argv: []string{"bd", "update", "gt-x", "--body-file", "-"},
			want: "the caller's stdin",
		},
		{
			name: "-f - under comments add",
			argv: []string{"bd", "comments", "add", "gt-x", "-f", "-"},
			want: "the caller's stdin",
		},
		{
			name: "-f- attached to the shorthand",
			argv: []string{"bd", "comments", "add", "gt-x", "-f-"},
			want: "the caller's stdin",
		},
		{
			name: "--description-file=-",
			argv: []string{"bd", "update", "gt-x", "--description-file=-"},
			want: "the caller's stdin",
		},
		{
			name: "--stdin",
			argv: []string{"bd", "create", "--stdin", "--title", "t"},
			want: "the caller's stdin",
		},
		{
			name:      "the call's own stdin",
			argv:      []string{"bd", "update", "gt-x"},
			callStdin: "the call's stdin",
			want:      "the call's stdin",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got string
			g, _, _, _ := gatewayTarget(t, town, writeGatewayToken(t, "t"), doerFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read the request body: %v", err)
				}
				_, rest, ok := bytes.Cut(body, []byte("\n"))
				if !ok {
					t.Fatalf("request body %q has no head line", body)
				}
				got = string(rest)
				return gatewayResponse(t, bdgateway.ExitFrame(0)), nil
			}))
			g.Stdin = strings.NewReader("the caller's stdin")
			call := Call{
				Argv: tt.argv,
				Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
				Dir:  town,
			}
			if tt.callStdin != "" {
				call.Stdin = strings.NewReader(tt.callStdin)
			}
			if _, err := g.Launch(call); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if got != tt.want {
				t.Errorf("stdin after the head = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBdGatewayTranslation: BEADS_DIR names the rig and the working directory is
// sent relative to the town; a directory outside the town runs in the town root
// with a note; a BEADS_DIR the workspace cannot hold is refused before any
// request; a call that sets none and has no .beads above its working directory
// runs in the town workspace only for bd version and the root help probe, and is
// refused otherwise; the walk that settles a missing BEADS_DIR has its own test,
// TestBdGatewaySettlesBeadsDir.
func TestBdGatewayTranslation(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	inside := filepath.Join(town, "gastown", "crew", "dipper")
	outside := t.TempDir()
	tests := []struct {
		name     string
		argv     []string // nil means "bd show gt-x"
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
			name: "version without BEADS_DIR",
			argv: []string{"bd", "version"}, env: nil, dir: inside, wantRig: "", wantCwd: "gastown/crew/dipper",
		},
		{
			name: "version with --json without BEADS_DIR",
			argv: []string{"bd", "version", "--json"}, env: nil, dir: inside, wantRig: "", wantCwd: "gastown/crew/dipper",
		},
		{
			name: "--help without BEADS_DIR",
			argv: []string{"bd", "--help"}, env: nil, dir: inside, wantRig: "", wantCwd: "gastown/crew/dipper",
		},
		{
			name: "help without BEADS_DIR",
			argv: []string{"bd", "help"}, env: nil, dir: inside, wantRig: "", wantCwd: "gastown/crew/dipper",
		},
		{
			name: "list without BEADS_DIR",
			argv: []string{"bd", "list", "--json"}, env: nil, dir: inside, wantErr: "sets no BEADS_DIR",
		},
		{
			name: "version with another flag without BEADS_DIR",
			argv: []string{"bd", "version", "--verbose"}, env: nil, dir: inside, wantErr: "sets no BEADS_DIR",
		},
		{
			name: "--help with another word without BEADS_DIR",
			argv: []string{"bd", "--help", "show"}, env: nil, dir: inside, wantErr: "sets no BEADS_DIR",
		},
		{
			name: "version with a BEADS_DIR outside the town",
			argv: []string{"bd", "version"},
			env:  []string{"BEADS_DIR=" + filepath.Join(outside, "gastown", ".beads")},
			dir:  inside, wantErr: "outside the town",
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
			argv := tt.argv
			if argv == nil {
				argv = []string{"bd", "show", "gt-x"}
			}
			exit, err := g.Launch(Call{Argv: argv, Env: tt.env, Dir: tt.dir})
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

// mkdirBeads makes the .beads directory under dir, standing in for a rig's or
// the town's beads directory.
func mkdirBeads(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, beadsDirName), 0o755); err != nil {
		t.Fatalf("make the .beads directory under %s: %v", dir, err)
	}
}

// TestBdGatewaySettlesBeadsDir: a bd call that sets no BEADS_DIR runs against
// the .beads directory nearest at or above its working directory inside the
// town, the walk bd itself makes; a working directory outside the town, or one
// with no .beads above it up to the town root, still refuses with the same
// message; an explicit BEADS_DIR is never overridden; and a .beads entry the
// stat cannot read refuses rather than being read as absent.
func TestBdGatewaySettlesBeadsDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		setup     func(t *testing.T, town string) (Call, func(*BdGateway))
		wantRig   string
		wantErr   string
		wantErrIs error
	}{
		{
			name: "a rig's mayor/rig gets that rig's .beads",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway)) {
				mkdirBeads(t, filepath.Join(town, "gastown"))
				return Call{Argv: []string{"bd", "list", "--json"}, Dir: filepath.Join(town, "gastown", "mayor", "rig")}, nil
			},
			wantRig: "gastown",
		},
		{
			name: "the town root gets the town's .beads",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway)) {
				mkdirBeads(t, town)
				return Call{Argv: []string{"bd", "list", "--json"}, Dir: town}, nil
			},
			wantRig: "",
		},
		{
			name: "a working directory outside the town is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway)) {
				mkdirBeads(t, filepath.Join(town, "gastown"))
				return Call{Argv: []string{"bd", "list"}, Dir: t.TempDir()}, nil
			},
			wantErr: "sets no BEADS_DIR",
		},
		{
			name: "no .beads on the way up is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway)) {
				return Call{Argv: []string{"bd", "list"}, Dir: filepath.Join(town, "gastown", "crew", "dipper")}, nil
			},
			wantErr: "sets no BEADS_DIR",
		},
		{
			name: "an explicit BEADS_DIR is used as it is, with no walk",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway)) {
				mkdirBeads(t, filepath.Join(town, "crew"))
				return Call{
					Argv: []string{"bd", "list"},
					Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
					Dir:  filepath.Join(town, "crew"),
				}, func(g *BdGateway) {
					// The walk from the working directory would read this rig's
					// entry; an explicit BEADS_DIR means no walk runs at all, so
					// an entry the stat cannot read cannot refuse the call.
					g.Stat = func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
				}
			},
			wantRig: "gastown",
		},
		{
			name: "the version probe runs with none",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway)) {
				mkdirBeads(t, filepath.Join(town, "gastown"))
				return Call{Argv: []string{"bd", "version"}, Dir: filepath.Join(town, "gastown", "crew")}, nil
			},
			wantRig: "",
		},
		{
			name: "an unreadable .beads entry refuses",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway)) {
				return Call{Argv: []string{"bd", "list"}, Dir: filepath.Join(town, "gastown", "crew")}, func(g *BdGateway) {
					g.Stat = func(name string) (fs.FileInfo, error) {
						if name == filepath.Join(town, "gastown", ".beads") {
							return nil, fs.ErrPermission
						}
						return nil, fs.ErrNotExist
					}
				}
			},
			wantErr: "read the .beads entry", wantErrIs: fs.ErrPermission,
		},
		{
			name: "a working directory that cannot be compared refuses",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway)) {
				return Call{Argv: []string{"bd", "list"}, Dir: "relative/dir"}, nil
			},
			wantErr: "compare the working directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			call, mutate := tt.setup(t, town)
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
			g, _, _, _ := gatewayTarget(t, town, writeGatewayToken(t, "t"), doer)
			if mutate != nil {
				mutate(&g)
			}
			exit, err := g.Launch(call)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Launch error = %v, want it to name %q", err, tt.wantErr)
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Errorf("error %v does not carry %v", err, tt.wantErrIs)
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
			if head.Rig != tt.wantRig {
				t.Errorf("head rig = %q, want %q", head.Rig, tt.wantRig)
			}
		})
	}
}

// writeRedirect puts a redirect file in the .beads directory under dir, the way
// a polecat worktree shares its rig's beads.
func writeRedirect(t *testing.T, dir, target string) {
	t.Helper()
	beads := filepath.Join(dir, beadsDirName)
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatalf("make the .beads directory under %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(beads, beadsRedirectName), []byte(target), 0o644); err != nil {
		t.Fatalf("write the redirect under %s: %v", dir, err)
	}
}

// TestBdGatewayFollowsTheWorktreeRedirect: a polecat worktree's .beads holds a
// redirect to its rig's beads directory, and the gateway workspace holds the
// rig's directory, never the worktree's, so a call with no BEADS_DIR from the
// worktree must name the target (gt-8y5mcn). A redirect the walk cannot follow
// keeps the worktree's .beads, and the gateway's existing refusal names that
// directory; a .beads with no redirect is named as it was; and an explicit
// BEADS_DIR wins with no walk at all.
func TestBdGatewayFollowsTheWorktreeRedirect(t *testing.T) {
	t.Parallel()
	// newTown builds a town with the town's beads and the gastown rig's
	// mayor/rig beads, and returns its root and the polecat worktree under it.
	newTown := func(t *testing.T) (town, worktree string) {
		t.Helper()
		town = t.TempDir()
		mkdirBeads(t, town)
		mkdirBeads(t, filepath.Join(town, "gastown", "mayor", "rig"))
		return town, filepath.Join(town, "gastown", "polecats", "nux", "gastown")
	}
	tests := []struct {
		name      string
		setup     func(t *testing.T) (town string, call Call)
		wantRig   string
		wantErr   string
		wantErrIn string
	}{
		{
			name: "a worktree redirect is followed to the rig's beads",
			setup: func(t *testing.T) (string, Call) {
				town, worktree := newTown(t)
				writeRedirect(t, worktree, "../../../mayor/rig/.beads\n")
				return town, Call{Argv: []string{"bd", "list", "--json"}, Dir: worktree}
			},
			wantRig: "gastown/mayor/rig",
		},
		{
			name: "a redirect that leaves the town keeps the worktree's .beads",
			setup: func(t *testing.T) (string, Call) {
				town, worktree := newTown(t)
				outside := filepath.Join(t.TempDir(), ".beads")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					t.Fatalf("make a beads directory outside the town: %v", err)
				}
				writeRedirect(t, worktree, outside+"\n")
				return town, Call{Argv: []string{"bd", "list"}, Dir: worktree}
			},
			wantErr: "workspace is not present", wantErrIn: "gastown/polecats/nux/gastown/.beads",
		},
		{
			name: "a redirect that does not resolve keeps the worktree's .beads",
			setup: func(t *testing.T) (string, Call) {
				town, worktree := newTown(t)
				writeRedirect(t, worktree, "../missing/.beads\n")
				return town, Call{Argv: []string{"bd", "list"}, Dir: worktree}
			},
			wantErr: "workspace is not present", wantErrIn: "gastown/polecats/nux/gastown/.beads",
		},
		{
			name: "a redirect to an entry that is not a directory is not followed",
			setup: func(t *testing.T) (string, Call) {
				town, worktree := newTown(t)
				entry := filepath.Join(town, "gastown", "other", beadsDirName)
				if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
					t.Fatalf("make the entry's directory: %v", err)
				}
				if err := os.WriteFile(entry, []byte("not a directory"), 0o644); err != nil {
					t.Fatalf("write the .beads entry: %v", err)
				}
				writeRedirect(t, worktree, "../../../other/.beads\n")
				return town, Call{Argv: []string{"bd", "list"}, Dir: worktree}
			},
			wantErr: "workspace is not present", wantErrIn: "gastown/polecats/nux/gastown/.beads",
		},
		{
			name: "a redirect that does not name a .beads directory is not followed",
			setup: func(t *testing.T) (string, Call) {
				town, worktree := newTown(t)
				writeRedirect(t, worktree, "../../../mayor/rig\n")
				return town, Call{Argv: []string{"bd", "list"}, Dir: worktree}
			},
			wantErr: "workspace is not present", wantErrIn: "gastown/polecats/nux/gastown/.beads",
		},
		{
			name: "a redirect that names nothing is not followed",
			setup: func(t *testing.T) (string, Call) {
				town, worktree := newTown(t)
				writeRedirect(t, worktree, " \n")
				return town, Call{Argv: []string{"bd", "list"}, Dir: worktree}
			},
			wantErr: "workspace is not present", wantErrIn: "gastown/polecats/nux/gastown/.beads",
		},
		{
			name: "a redirect that cannot be read is not followed",
			setup: func(t *testing.T) (string, Call) {
				town, worktree := newTown(t)
				// A directory in the redirect's place makes the read fail with
				// something other than a missing file.
				if err := os.MkdirAll(filepath.Join(worktree, beadsDirName, beadsRedirectName), 0o755); err != nil {
					t.Fatalf("make a directory in the redirect's place: %v", err)
				}
				return town, Call{Argv: []string{"bd", "list"}, Dir: worktree}
			},
			wantErr: "workspace is not present", wantErrIn: "gastown/polecats/nux/gastown/.beads",
		},
		{
			name: "a .beads with no redirect is named as it was",
			setup: func(t *testing.T) (string, Call) {
				town := t.TempDir()
				mkdirBeads(t, town)
				mkdirBeads(t, filepath.Join(town, "gastown"))
				return town, Call{Argv: []string{"bd", "list"}, Dir: filepath.Join(town, "gastown", "crew", "nux")}
			},
			wantRig: "gastown",
		},
		{
			name: "an explicit BEADS_DIR wins and the redirect is not read",
			setup: func(t *testing.T) (string, Call) {
				town, worktree := newTown(t)
				writeRedirect(t, worktree, "../../../mayor/rig/.beads\n")
				return town, Call{
					Argv: []string{"bd", "list"},
					Env:  []string{"BEADS_DIR=" + filepath.Join(town, ".beads")},
					Dir:  worktree,
				}
			},
			wantRig: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town, call := tt.setup(t)
			var head bdgateway.Request
			// The fake gateway holds the town's beads and the rig's, in the
			// workspace document's layout, and refuses every other rig with the
			// real gateway's message, naming the workspace it lacks.
			held := map[string]bool{"": true, "gastown": true, "gastown/mayor/rig": true}
			doer := doerFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				line, _, _ := bytes.Cut(body, []byte("\n"))
				if err := json.Unmarshal(line, &head); err != nil {
					t.Fatalf("head is not JSON: %v", err)
				}
				if !held[head.Rig] {
					payload, err := json.Marshal(bdgateway.ErrorPayload{
						Code:    "bad_request",
						Message: "bdgateway: the workspace is not present: stat /ws/" + head.Rig + "/.beads: no such file or directory",
					})
					if err != nil {
						t.Fatalf("marshal the refusal: %v", err)
					}
					return statusResponse(http.StatusBadRequest, string(payload)), nil
				}
				return gatewayResponse(t, bdgateway.ExitFrame(0)), nil
			})
			g, _, _, _ := gatewayTarget(t, town, writeGatewayToken(t, "t"), doer)
			exit, err := g.Launch(call)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Launch error = %v, want it to name %q", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErrIn) {
					t.Errorf("error %v does not name %q", err, tt.wantErrIn)
				}
				if exit != exitDown {
					t.Errorf("exit = %d, want %d", exit, exitDown)
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
		})
	}
}

// TestBdGatewaySettlesDirectory: a call that begins with bd's root flag
// "-C <dir>" runs in that directory. The two tokens leave the argv, the
// directory (resolved against the call's own working directory when relative)
// becomes the call's working directory, and the rig follows from it; an explicit
// BEADS_DIR still decides the rig; a -C that names no directory, names one
// outside the town, missing, not a directory or unreadable refuses before the
// gateway sees the call; and every other -C form is sent unchanged for the
// gateway's flag refusal (gt-dnaau7).
func TestBdGatewaySettlesDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		setup     func(t *testing.T, town string) (Call, func(*BdGateway), []string)
		wantRig   string
		wantCwd   string
		wantErr   string
		wantErrIs error
	}{
		{
			name: "a leading -C names the rig directory",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				mkdirBeads(t, filepath.Join(town, "gastown"))
				return Call{
					Argv: []string{"bd", "-C", filepath.Join(town, "gastown"), "list", "--json"},
					Dir:  town,
				}, nil, []string{"list", "--json"}
			},
			wantRig: "gastown", wantCwd: "gastown",
		},
		{
			name: "a relative -C resolves against the call's working directory",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				mkdirBeads(t, filepath.Join(town, "gastown"))
				return Call{
					Argv: []string{"bd", "-C", "../../gastown", "list", "--json"},
					Dir:  filepath.Join(town, "crew", "dipper"),
				}, nil, []string{"list", "--json"}
			},
			wantRig: "gastown", wantCwd: "gastown",
		},
		{
			name: "a relative -C that leaves the town is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "-C", "../..", "list"},
					Dir:  filepath.Join(town, "crew"),
				}, nil, nil
			},
			wantErr: "outside the town",
		},
		{
			name: "an absolute -C outside the town is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "-C", t.TempDir(), "list"},
					Dir:  town,
				}, nil, nil
			},
			wantErr: "outside the town",
		},
		{
			name: "a -C in a call whose working directory is relative is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "-C", "gastown", "list"},
					Dir:  "relative/dir",
				}, nil, nil
			},
			wantErr: "compare the bd call's -C directory",
		},
		{
			name: "a -C directory that does not exist is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "-C", filepath.Join(town, "gastown", "gone"), "list"},
					Dir:  town,
				}, nil, nil
			},
			wantErr: "does not exist",
		},
		{
			name: "a -C path that is a file is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				file := filepath.Join(town, "notes.txt")
				if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
					t.Fatalf("write the file: %v", err)
				}
				return Call{Argv: []string{"bd", "-C", file, "list"}, Dir: town}, nil, nil
			},
			wantErr: "is not a directory",
		},
		{
			name: "a -C with no directory is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{Argv: []string{"bd", "-C", "", "list"}, Dir: town}, nil, nil
			},
			wantErr: "names no directory",
		},
		{
			name: "a -C directory the stat cannot read is refused",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "-C", filepath.Join(town, "gastown"), "list"},
					Dir:  town,
				}, func(g *BdGateway) {
					g.Stat = func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
				}, nil
			},
			wantErr: "read the bd call's -C directory", wantErrIs: fs.ErrPermission,
		},
		{
			name: "an explicit BEADS_DIR is kept",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				mkdirBeads(t, filepath.Join(town, "gastown"))
				other := filepath.Join(town, "other-rig")
				if err := os.MkdirAll(other, 0o755); err != nil {
					t.Fatalf("make the other rig directory: %v", err)
				}
				return Call{
					Argv: []string{"bd", "-C", other, "list"},
					Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
					Dir:  town,
				}, nil, []string{"list"}
			},
			wantRig: "gastown", wantCwd: "other-rig",
		},
		{
			name: "-C after the verb is sent unchanged",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "list", "-C", "gastown", "--json"},
					Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
					Dir:  town,
				}, nil, []string{"list", "-C", "gastown", "--json"}
			},
			wantRig: "gastown", wantCwd: ".",
		},
		{
			name: "-C=dir is sent unchanged",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "-C=gastown", "list"},
					Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
					Dir:  town,
				}, nil, []string{"-C=gastown", "list"}
			},
			wantRig: "gastown", wantCwd: ".",
		},
		{
			name: "-Cdir is sent unchanged",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "-Cgastown", "list"},
					Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
					Dir:  town,
				}, nil, []string{"-Cgastown", "list"}
			},
			wantRig: "gastown", wantCwd: ".",
		},
		{
			name: "a second -C is sent unchanged",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "list", "-C", "one", "-C", "two"},
					Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
					Dir:  town,
				}, nil, []string{"list", "-C", "one", "-C", "two"}
			},
			wantRig: "gastown", wantCwd: ".",
		},
		{
			name: "a later -C after a leading one is left for the gateway",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				mkdirBeads(t, filepath.Join(town, "gastown"))
				return Call{
					Argv: []string{"bd", "-C", filepath.Join(town, "gastown"), "-C", "two", "list"},
					Dir:  town,
				}, nil, []string{"-C", "two", "list"}
			},
			wantRig: "gastown", wantCwd: "gastown",
		},
		{
			name: "a leading -C with no value is sent unchanged",
			setup: func(t *testing.T, town string) (Call, func(*BdGateway), []string) {
				return Call{
					Argv: []string{"bd", "-C"},
					Env:  []string{"BEADS_DIR=" + filepath.Join(town, "gastown", ".beads")},
					Dir:  town,
				}, nil, []string{"-C"}
			},
			wantRig: "gastown", wantCwd: ".",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			call, mutate, wantArgv := tt.setup(t, town)
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
			g, _, _, _ := gatewayTarget(t, town, writeGatewayToken(t, "t"), doer)
			if mutate != nil {
				mutate(&g)
			}
			exit, err := g.Launch(call)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Launch error = %v, want it to name %q", err, tt.wantErr)
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Errorf("error %v does not carry %v", err, tt.wantErrIs)
				}
				if strings.Contains(err.Error(), "\n") {
					t.Errorf("the error is not one line: %q", err)
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
			if !slices.Equal(head.Argv, wantArgv) {
				t.Errorf("head argv = %q, want %q", head.Argv, wantArgv)
			}
			if head.Rig != tt.wantRig {
				t.Errorf("head rig = %q, want %q", head.Rig, tt.wantRig)
			}
			if head.Cwd != tt.wantCwd {
				t.Errorf("head cwd = %q, want %q", head.Cwd, tt.wantCwd)
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
			name: "response stream breaks",
			mutate: func(g *BdGateway, _ *Call) {
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(errReader{errors.New("broken pipe")}), Header: make(http.Header)}, nil
				})
			},
			wantErr: "read the bd gateway's answer", wantExit: exitDown,
		},
		{
			name: "stdout write fails",
			mutate: func(g *BdGateway, _ *Call) {
				g.Stdout = errWriter{errors.New("closed pipe")}
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return gatewayResponse(t,
						bdgateway.Frame{Kind: bdgateway.FrameStdout, Payload: []byte("x")},
						bdgateway.ExitFrame(0),
					), nil
				})
			},
			wantErr: "write bd's stdout", wantExit: exitDown,
		},
		{
			name: "stderr write fails",
			mutate: func(g *BdGateway, _ *Call) {
				g.Stderr = errWriter{errors.New("closed pipe")}
				g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
					return gatewayResponse(t,
						bdgateway.Frame{Kind: bdgateway.FrameStderr, Payload: []byte("x")},
						bdgateway.ExitFrame(0),
					), nil
				})
			},
			wantErr: "write bd's stderr", wantExit: exitDown,
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
			name: "a call that names no tool",
			mutate: func(_ *BdGateway, c *Call) {
				c.Argv = nil
			},
			wantErr: "the call names no tool to run", wantExit: exitDown,
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
			name: "URL that does not parse",
			mutate: func(g *BdGateway, _ *Call) {
				g.URL = "http://gw:7440/%zz"
			},
			wantErr: "does not parse", wantExit: exitDown,
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
