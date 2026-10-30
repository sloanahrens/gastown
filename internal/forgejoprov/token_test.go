package forgejoprov

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// memInfo is the fs.FileInfo of an in-memory file.
type memInfo struct {
	mode fs.FileMode
	dir  bool
}

func (memInfo) Name() string        { return "" }
func (memInfo) Size() int64         { return 0 }
func (i memInfo) Mode() fs.FileMode { return i.mode }
func (memInfo) ModTime() time.Time  { return time.Time{} }
func (i memInfo) IsDir() bool       { return i.dir }
func (memInfo) Sys() any            { return nil }

// memFiles is an in-memory Files that records every call and the mode it was
// given, so a test reads what the run asked of the filesystem.
type memFiles struct {
	files    map[string][]byte
	modes    map[string]fs.FileMode
	dirs     map[string]bool
	ops      []string
	writeErr error
	chmodErr error
	statErr  error
}

func newMemFiles() *memFiles {
	return &memFiles{files: map[string][]byte{}, modes: map[string]fs.FileMode{}, dirs: map[string]bool{}}
}

func (m *memFiles) WriteFileAtomic(path string, data []byte, mode fs.FileMode) error {
	m.ops = append(m.ops, "write "+path+" "+mode.String())
	if m.writeErr != nil {
		return m.writeErr
	}
	m.files[path] = append([]byte(nil), data...)
	m.modes[path] = mode
	return nil
}

func (m *memFiles) Chmod(path string, mode fs.FileMode) error {
	m.ops = append(m.ops, "chmod "+path+" "+mode.String())
	if m.chmodErr != nil {
		return m.chmodErr
	}
	m.modes[path] = mode
	return nil
}

func (m *memFiles) Stat(path string) (fs.FileInfo, error) {
	if m.statErr != nil {
		return nil, m.statErr
	}
	if m.dirs[path] {
		return memInfo{dir: true}, nil
	}
	if _, ok := m.files[path]; ok {
		return memInfo{mode: m.modes[path]}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

// seed puts an existing file at path, with the mode given.
func (m *memFiles) seed(path, content string, mode fs.FileMode) {
	m.files[path] = []byte(content)
	m.modes[path] = mode
}

// tokenDir is the config directory every token test uses; nothing is written there.
const tokenDir = "/cfg"

// tokenProvisioner returns a Provisioner over transport with the in-memory files,
// the default bot prefix and token name, and the log lines captured in logs.
func tokenProvisioner(transport http.RoundTripper, files Files, rotate, dryRun bool, logs *[]string) *Provisioner {
	p := userProvisioner(transport, dryRun, logs)
	p.TokenName = DefaultTokenName
	p.WebURL = "http://forgejo.fake.invalid"
	p.ConfigDir = tokenDir
	p.Rotate = rotate
	p.Files = files
	return p
}

// fakeTokenScopes returns the scopes of the token the fake holds for user.
func fakeTokenScopes(f *fakeForgejo, user, name string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[user]
	if !ok {
		return nil, false
	}
	tok, ok := u.tokens[name]
	return tok.scopes, ok
}

// withBot makes the fake hold the bot account of role, as a run that already
// created the user leaves it.
func withBot(t *testing.T, f *fakeForgejo, role string) {
	t.Helper()
	var logs []string
	if err := userProvisioner(f, false, &logs).EnsureUser(role); err != nil {
		t.Fatalf("seed bot %s: %v", role, err)
	}
}

// tokenFileText is the exact file the run writes for user and value.
func tokenFileText(user, value string) string {
	return "FORGEJO_URL=http://forgejo.fake.invalid\nFORGEJO_USER=" + user + "\nFORGEJO_TOKEN=" + value + "\n"
}

// TestEnsureTokenMintsWithTheRoleScopes mints for every role and checks the fake
// holds a token with that role's exact scopes and the file holds the three lines
// with the minted value.
func TestEnsureTokenMintsWithTheRoleScopes(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"polecat", "landing", "registry", "viewer"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			withBot(t, f, role)
			files := newMemFiles()
			var logs []string
			p := tokenProvisioner(f, files, false, false, &logs)
			if err := p.EnsureToken(role); err != nil {
				t.Fatalf("EnsureToken(%s): %v", role, err)
			}
			user := DefaultBotPrefix + role
			want, _ := RoleScopes(role)
			got, ok := fakeTokenScopes(f, user, DefaultTokenName)
			if !ok {
				t.Fatalf("the fake holds no %s token for %s", DefaultTokenName, user)
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s was minted with scopes %v, want %v", role, got, want)
			}
			path := filepath.Join(tokenDir, "forgejo-"+role+".env")
			value := f.token(user, DefaultTokenName)
			if string(files.files[path]) != tokenFileText(user, value) {
				t.Errorf("token file = %q, want the three lines with the minted value", redactToken(string(files.files[path])))
			}
		})
	}
}

// TestEnsureTokenAcceptsTheTokenField reads the value from a reply that names it
// "token" instead of "sha1".
func TestEnsureTokenAcceptsTheTokenField(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.tokenField = "token"
	withBot(t, f, "polecat")
	files := newMemFiles()
	var logs []string
	if err := tokenProvisioner(f, files, false, false, &logs).EnsureToken("polecat"); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	path := filepath.Join(tokenDir, "forgejo-polecat.env")
	if want := tokenFileText("bot-polecat", f.token("bot-polecat", DefaultTokenName)); string(files.files[path]) != want {
		t.Errorf("token file = %q, want %q", string(files.files[path]), want)
	}
}

// TestEnsureTokenRefusesWhatItCannotStore covers an unknown role and a directory
// where the token file belongs: neither mints anything.
func TestEnsureTokenRefusesWhatItCannotStore(t *testing.T) {
	t.Parallel()
	t.Run("unknown role", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		var logs []string
		err := tokenProvisioner(f, newMemFiles(), false, false, &logs).EnsureToken("../etc/passwd")
		if err == nil || !strings.Contains(err.Error(), "unknown role") {
			t.Errorf("err = %v, want an unknown role error", err)
		}
		if len(f.callLog()) != 0 {
			t.Errorf("an unknown role reached the server: %v", f.callLog())
		}
	})
	t.Run("directory at the path", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		withBot(t, f, "polecat")
		files := newMemFiles()
		files.dirs[filepath.Join(tokenDir, "forgejo-polecat.env")] = true
		before := len(f.writes())
		var logs []string
		err := tokenProvisioner(f, files, false, false, &logs).EnsureToken("polecat")
		if err == nil || !strings.Contains(err.Error(), "is a directory") {
			t.Errorf("err = %v, want a directory error", err)
		}
		if len(f.writes()) != before {
			t.Errorf("a token was minted though nothing could store it: %v", f.writes())
		}
	})
	t.Run("stat fails", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		withBot(t, f, "polecat")
		files := newMemFiles()
		files.statErr = fs.ErrPermission
		before := len(f.writes())
		var logs []string
		err := tokenProvisioner(f, files, false, false, &logs).EnsureToken("polecat")
		if err == nil || !errors.Is(err, fs.ErrPermission) {
			t.Errorf("err = %v, want the permission error", err)
		}
		if len(f.writes()) != before {
			t.Errorf("a token was minted though the file could not be examined: %v", f.writes())
		}
	})
}

// TestTokenFileIsMode0600ThroughTheSeam checks the run asks for mode 0600 on the
// write and again on the final path.
func TestTokenFileIsMode0600ThroughTheSeam(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	withBot(t, f, "polecat")
	files := newMemFiles()
	var logs []string
	if err := tokenProvisioner(f, files, false, false, &logs).EnsureToken("polecat"); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	path := filepath.Join(tokenDir, "forgejo-polecat.env")
	want := []string{"write " + path + " -rw-------", "chmod " + path + " -rw-------"}
	if strings.Join(files.ops, "|") != strings.Join(want, "|") {
		t.Errorf("file calls = %v, want %v", files.ops, want)
	}
}

// TestTokenFileIsMode0600AndAtomic runs the real filesystem in a temp directory:
// the file is replaced by a rename with mode 0600, no temp file is left behind,
// and a write that fails leaves the old file and no partial one.
func TestTokenFileIsMode0600AndAtomic(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cfg")
	path := filepath.Join(dir, "forgejo-polecat.env")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := (osFiles{}).WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("content = %q, want new", got)
	}
	if after.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", after.Mode().Perm())
	}
	if os.SameFile(before, after) {
		t.Errorf("the file was rewritten in place; an atomic write replaces it by a rename")
	}
	assertOnlyFile(t, dir, "forgejo-polecat.env")

	// A directory where the file belongs makes the rename fail: the error comes
	// back, and the temp file is gone.
	blocked := filepath.Join(dir, "forgejo-viewer.env")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (osFiles{}).WriteFileAtomic(blocked, []byte("new"), 0o600); err == nil {
		t.Error("WriteFileAtomic over a directory succeeded")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".forgejo-token.") {
			t.Errorf("a failed write left %s behind", e.Name())
		}
	}
	// The mode argument is what the file gets, not a constant inside the writer.
	odd := filepath.Join(dir, "forgejo-odd.env")
	if err := (osFiles{}).WriteFileAtomic(odd, []byte("x"), 0o640); err != nil {
		t.Fatalf("WriteFileAtomic with mode 0640: %v", err)
	}
	if info, err := os.Stat(odd); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("a write asked for mode 0640 produced %v %v", info, err)
	}
	// A missing directory is created, 0700.
	nested := filepath.Join(t.TempDir(), "a", "b", "f.env")
	if err := (osFiles{}).WriteFileAtomic(nested, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic into a new directory: %v", err)
	}
	if info, err := os.Stat(filepath.Dir(nested)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("new directory = %v %v, want mode 0700", info, err)
	}
}

// assertOnlyFile fails unless dir holds exactly the one file name, so a temp file
// left behind by the write shows up.
func assertOnlyFile(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only %s", names, name)
	}
}

// TestEnsureTokenLeavesAnExistingFile checks that without Rotate an existing file
// is chmod'ed 0600 and nothing else is written or minted.
func TestEnsureTokenLeavesAnExistingFile(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	withBot(t, f, "polecat")
	files := newMemFiles()
	path := filepath.Join(tokenDir, "forgejo-polecat.env")
	files.seed(path, "kept", 0o644)
	writesBefore := len(f.writes())
	var logs []string
	if err := tokenProvisioner(f, files, false, false, &logs).EnsureToken("polecat"); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if string(files.files[path]) != "kept" {
		t.Errorf("the existing file changed: %q", files.files[path])
	}
	if files.modes[path] != 0o600 {
		t.Errorf("mode = %v, want 0600", files.modes[path])
	}
	if want := []string{"chmod " + path + " -rw-------"}; strings.Join(files.ops, "|") != strings.Join(want, "|") {
		t.Errorf("file calls = %v, want only the chmod", files.ops)
	}
	if len(f.writes()) != writesBefore {
		t.Errorf("the server was written to: %v", f.writes())
	}
	if len(logs) != 1 || logs[0] != "token "+path+" exists; leaving it (--rotate mints a new one)" {
		t.Errorf("logs = %v", logs)
	}
}

// TestRotateRevokesThenMints checks the old token is deleted before the new one is
// minted, and the file is replaced.
func TestRotateRevokesThenMints(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	withBot(t, f, "polecat")
	files := newMemFiles()
	var logs []string
	p := tokenProvisioner(f, files, false, false, &logs)
	if err := p.EnsureToken("polecat"); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	first := f.token("bot-polecat", DefaultTokenName)
	f.calls = nil
	p.Rotate = true
	logs = nil
	if err := p.EnsureToken("polecat"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	second := f.token("bot-polecat", DefaultTokenName)
	if first == second || second == "" {
		t.Fatalf("rotation kept the token: %q then %q", redactToken(first), redactToken(second))
	}
	var order []string
	for _, c := range f.callLog() {
		order = append(order, c.method+" "+c.path)
	}
	want := []string{
		"GET /api/v1/admin/users/bot-polecat/tokens",
		"DELETE /api/v1/admin/users/bot-polecat/tokens/gt-provision",
		"POST /api/v1/admin/users/bot-polecat/tokens",
	}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v, want %v", order, want)
	}
	path := filepath.Join(tokenDir, "forgejo-polecat.env")
	if string(files.files[path]) != tokenFileText("bot-polecat", second) {
		t.Errorf("the file does not hold the new token")
	}
	joined := strings.Join(logs, "\n")
	for _, line := range []string{"rotating the token in " + path, "revoked the previous 'gt-provision' token"} {
		if !strings.Contains(joined, line) {
			t.Errorf("logs lack %q: %v", line, logs)
		}
	}
}

// failingRevokeTransport passes everything to next except a DELETE or a token
// list, which it answers with the status given, so a rotation meets a failed
// revoke.
type failingRevokeTransport struct {
	next       http.RoundTripper
	listStatus int
	delStatus  int
	// body is the reply to the failed call; empty means echoReply.
	body string
}

func (f failingRevokeTransport) reply() string {
	if f.body != "" {
		return f.body
	}
	return echoReply
}

func (f failingRevokeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete && f.delStatus != 0 {
		return respond(req, f.delStatus, f.reply())
	}
	if req.Method == http.MethodGet && f.listStatus != 0 && strings.HasSuffix(req.URL.Path, "/tokens") {
		return respond(req, f.listStatus, f.reply())
	}
	return f.next.RoundTrip(req)
}

// TestRotateRevokeFailuresAreWarnings checks a failed list or delete only warns
// and the mint is still attempted, and that no warning carries a token. A list
// that fails leaves nothing to collide with, so the mint succeeds. A delete that
// fails leaves the old token in place, so the server refuses the same name as a
// duplicate: the run reports that refusal after the warning, and the file keeps
// the token it had.
func TestRotateRevokeFailuresAreWarnings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		tr        func(next http.RoundTripper) http.RoundTripper
		seedToken bool
		want      string
		wantErr   string
	}{
		{name: "list fails", tr: func(n http.RoundTripper) http.RoundTripper { return failingRevokeTransport{next: n, listStatus: 500} },
			want: "warning: cannot list bot-polecat's tokens (HTTP 500)"},
		{name: "delete fails", tr: func(n http.RoundTripper) http.RoundTripper { return failingRevokeTransport{next: n, delStatus: 500} },
			seedToken: true, want: "warning: could not revoke the previous 'gt-provision' token (HTTP 500)", wantErr: "unexpected HTTP 422"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			withBot(t, f, "polecat")
			files := newMemFiles()
			path := filepath.Join(tokenDir, "forgejo-polecat.env")
			var logs []string
			if tc.seedToken {
				if err := tokenProvisioner(f, newMemFiles(), false, false, &logs).EnsureToken("polecat"); err != nil {
					t.Fatalf("seed token: %v", err)
				}
				logs = nil
			}
			files.seed(path, "old", 0o600)
			f.calls = nil
			err := tokenProvisioner(tc.tr(f), files, true, false, &logs).EnsureToken("polecat")
			if !strings.Contains(strings.Join(logs, "\n"), tc.want) {
				t.Errorf("logs lack %q: %v", tc.want, logs)
			}
			posted := false
			for _, c := range f.callLog() {
				if c.method == http.MethodPost {
					posted = true
				}
			}
			if !posted {
				t.Errorf("the mint was not attempted after the warning: %v", f.callLog())
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("EnsureToken: %v", err)
				}
				if string(files.files[path]) == "old" {
					t.Errorf("the file was not replaced after the warning")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if string(files.files[path]) != "old" {
					t.Errorf("the file changed though the mint failed: %q", files.files[path])
				}
			}
			for _, line := range append(logs, errString(err)) {
				if strings.Contains(line, fixtureToken) || strings.Contains(line, mintedSecret) {
					t.Errorf("a log line or error carries a token: %q", redactToken(line))
				}
			}
		})
	}
}

// errString is err's text, or "" for nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestRotateNoTokenNamed checks the log when the user holds no token of that name.
func TestRotateNoTokenNamed(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	withBot(t, f, "polecat")
	files := newMemFiles()
	files.seed(filepath.Join(tokenDir, "forgejo-polecat.env"), "old", 0o600)
	var logs []string
	if err := tokenProvisioner(f, files, true, false, &logs).EnsureToken("polecat"); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	want := "no token named 'gt-provision' to revoke; if the file held a token minted under another name, revoke it in the web UI"
	if !strings.Contains(strings.Join(logs, "\n"), want) {
		t.Errorf("logs lack %q: %v", want, logs)
	}
	for _, c := range f.callLog() {
		if c.method == http.MethodDelete {
			t.Errorf("a DELETE was sent though no token had that name: %v", c)
		}
	}
}

// TestEnsureTokenDryRun writes nothing: no server write, no file call, and the
// would-mint line names the user and the path.
func TestEnsureTokenDryRun(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		name := "no file"
		if existing {
			name = "existing file with rotate"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			withBot(t, f, "polecat")
			files := newMemFiles()
			path := filepath.Join(tokenDir, "forgejo-polecat.env")
			if existing {
				files.seed(path, "old", 0o644)
			}
			writesBefore := len(f.writes())
			var logs []string
			if err := tokenProvisioner(f, files, existing, true, &logs).EnsureToken("polecat"); err != nil {
				t.Fatalf("EnsureToken: %v", err)
			}
			if len(files.ops) != 0 {
				t.Errorf("a dry run touched the files: %v", files.ops)
			}
			if len(f.writes()) != writesBefore {
				t.Errorf("a dry run wrote to the server: %v", f.writes())
			}
			want := "would mint a 'gt-provision' token for bot-polecat and write " + path
			if !strings.Contains(strings.Join(logs, "\n"), want) {
				t.Errorf("logs lack %q: %v", want, logs)
			}
		})
	}
}

// noUsableTransport answers a create-token call with the status and body given.
type noUsableTransport struct {
	status int
	body   string
}

func (n noUsableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return respond(req, n.status, n.body)
}

// TestMintFailures covers the refusals of the mint: a 403 names the missing
// rights, a reply with no usable token fails without echoing the body's token
// fields, and any other status goes through the scrubbing Unexpected.
func TestMintFailures(t *testing.T) {
	t.Parallel()
	t.Run("403", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		f.adminless = true
		withBot(t, f, "polecat")
		files := newMemFiles()
		var logs []string
		err := tokenProvisioner(f, files, false, false, &logs).EnsureToken("polecat")
		if err == nil || !strings.Contains(err.Error(), "needs site-administrator rights, and a token limit of 'all'") {
			t.Fatalf("err = %v, want the site-administrator message", err)
		}
		if len(files.ops) != 0 {
			t.Errorf("a file was written after the refusal: %v", files.ops)
		}
	})
	for _, tc := range []struct{ name, body string }{
		{name: "empty value", body: `{"name":"gt-provision","sha1":"","token":""}`},
		{name: "characters outside the set", body: `{"sha1":"abc def!` + mintedSecret + `"}`},
		{name: "no token field", body: `{"name":"gt-provision","note":"` + mintedSecret + `"}`},
		{name: "not JSON", body: `not json ` + mintedSecret},
	} {
		t.Run("no usable token: "+tc.name, func(t *testing.T) {
			t.Parallel()
			files := newMemFiles()
			var logs []string
			p := tokenProvisioner(noUsableTransport{status: http.StatusCreated, body: tc.body}, files, false, false, &logs)
			err := p.EnsureToken("polecat")
			if err == nil || !strings.Contains(err.Error(), "returned no usable token") {
				t.Fatalf("err = %v, want the no usable token error", err)
			}
			if strings.Contains(err.Error(), mintedSecret) || strings.Contains(err.Error(), "abc def") {
				t.Errorf("the error echoes the reply: %q", err.Error())
			}
			if len(files.ops) != 0 {
				t.Errorf("a file was written for an unusable token: %v", files.ops)
			}
		})
	}
	t.Run("other status", func(t *testing.T) {
		t.Parallel()
		files := newMemFiles()
		var logs []string
		p := tokenProvisioner(noUsableTransport{status: http.StatusInternalServerError, body: echoReply}, files, false, false, &logs)
		err := p.EnsureToken("polecat")
		if err == nil || !strings.Contains(err.Error(), "unexpected HTTP 500") {
			t.Fatalf("err = %v, want an unexpected HTTP 500 error", err)
		}
		if strings.Contains(err.Error(), fixtureToken) || strings.Contains(err.Error(), mintedSecret) {
			t.Errorf("the error carries a token: %q", redactToken(err.Error()))
		}
	})
	t.Run("write fails", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		withBot(t, f, "polecat")
		files := newMemFiles()
		files.writeErr = errors.New("disk full")
		var logs []string
		err := tokenProvisioner(f, files, false, false, &logs).EnsureToken("polecat")
		path := filepath.Join(tokenDir, "forgejo-polecat.env")
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "--rotate") {
			t.Fatalf("err = %v, want the path, the cause and the --rotate hint", err)
		}
		if v := f.token("bot-polecat", DefaultTokenName); v != "" && strings.Contains(err.Error(), v) {
			t.Errorf("the error carries the minted token")
		}
	})
}

// TestNoTokenInOutput runs a full mint and a rotate and checks neither the admin
// token nor a minted token appears in any log line, any error, any recorded call
// field or any file call; the minted token is only in the token file's content.
func TestNoTokenInOutput(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	withBot(t, f, "polecat")
	files := newMemFiles()
	var logs []string
	p := tokenProvisioner(f, files, false, false, &logs)
	var errs []error
	errs = append(errs, p.EnsureToken("polecat"))
	first := f.token("bot-polecat", DefaultTokenName)
	p.Rotate = true
	errs = append(errs, p.EnsureToken("polecat"))
	second := f.token("bot-polecat", DefaultTokenName)
	secrets := []string{fixtureToken, first, second}
	for _, s := range secrets {
		if s == "" {
			t.Fatal("a token was not minted")
		}
	}
	var surfaces []string
	surfaces = append(surfaces, logs...)
	for _, e := range errs {
		if e != nil {
			surfaces = append(surfaces, e.Error())
		}
	}
	for _, c := range f.callLog() {
		surfaces = append(surfaces, c.method+" "+c.path)
	}
	surfaces = append(surfaces, files.ops...)
	for _, v := range f.violations() {
		surfaces = append(surfaces, v.method+" "+v.path)
	}
	for _, line := range surfaces {
		for _, s := range secrets {
			if strings.Contains(line, s) {
				t.Errorf("a token appears in %q", strings.ReplaceAll(line, s, redacted))
			}
		}
	}
	path := filepath.Join(tokenDir, "forgejo-polecat.env")
	if !strings.Contains(string(files.files[path]), second) {
		t.Errorf("the token file does not hold the minted token")
	}
}

// failingTemp is a temp file whose steps can be made to fail, and which records
// whether it was written, closed and removed.
type failingTemp struct {
	name       string
	chmodErr   error
	writeErr   error
	closeErr   error
	wrote      bool
	closeCalls int
}

func (f *failingTemp) Name() string            { return f.name }
func (f *failingTemp) Chmod(fs.FileMode) error { return f.chmodErr }
func (f *failingTemp) Close() error            { f.closeCalls++; return f.closeErr }
func (f *failingTemp) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.wrote = true
	return len(p), nil
}

// TestTokenFileWriteFailureLeavesNoTempFile makes each step of the atomic write fail
// in turn and checks the error comes back, nothing is renamed over the target
// after a failure, and the temp file is removed once it exists.
func TestTokenFileWriteFailureLeavesNoTempFile(t *testing.T) {
	t.Parallel()
	boom := errors.New("injected failure")
	cases := []struct {
		name       string
		mkdirErr   error
		createErr  error
		temp       *failingTemp
		renameErr  error
		wantRemove bool
		wantRename bool
	}{
		{name: "mkdir", mkdirErr: boom},
		{name: "create temp", createErr: boom},
		{name: "chmod temp", temp: &failingTemp{name: "/d/.forgejo-token.1", chmodErr: boom}, wantRemove: true},
		{name: "write temp", temp: &failingTemp{name: "/d/.forgejo-token.1", writeErr: boom}, wantRemove: true},
		{name: "close temp", temp: &failingTemp{name: "/d/.forgejo-token.1", closeErr: boom}, wantRemove: true},
		{name: "rename", temp: &failingTemp{name: "/d/.forgejo-token.1"}, renameErr: boom, wantRemove: true, wantRename: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var removed []string
			renamed := false
			o := osFiles{
				mkdirAll: func(string, fs.FileMode) error { return tc.mkdirErr },
				createTemp: func(string, string) (tempFile, error) {
					if tc.createErr != nil {
						return nil, tc.createErr
					}
					return tc.temp, nil
				},
				rename: func(string, string) error { renamed = true; return tc.renameErr },
				remove: func(p string) error { removed = append(removed, p); return nil },
			}
			err := o.WriteFileAtomic("/d/forgejo-polecat.env", []byte("x"), 0o600)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if tc.wantRemove != (len(removed) == 1) {
				t.Errorf("removed = %v, want the temp file removed: %v", removed, tc.wantRemove)
			}
			if tc.wantRemove && removed[0] != tc.temp.name {
				t.Errorf("removed %q, want %q", removed[0], tc.temp.name)
			}
			if renamed != tc.wantRename {
				t.Errorf("renamed = %v, want %v", renamed, tc.wantRename)
			}
			if tc.temp != nil && tc.temp.closeCalls == 0 && tc.wantRemove {
				t.Errorf("the temp file was never closed")
			}
		})
	}
}

// TestEnsureTokenChmodFailures checks a chmod that fails on an existing file stops
// the run before anything is minted, and one that fails on the new file is an
// error naming the path.
func TestEnsureTokenChmodFailures(t *testing.T) {
	t.Parallel()
	path := filepath.Join(tokenDir, "forgejo-polecat.env")
	t.Run("existing file", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		withBot(t, f, "polecat")
		files := newMemFiles()
		files.seed(path, "old", 0o644)
		files.chmodErr = errors.New("operation not permitted")
		before := len(f.writes())
		var logs []string
		err := tokenProvisioner(f, files, true, false, &logs).EnsureToken("polecat")
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "mode 600") {
			t.Fatalf("err = %v, want an error naming %s and mode 600", err, path)
		}
		if len(f.writes()) != before {
			t.Errorf("a token was minted though the file could not be secured: %v", f.writes())
		}
	})
	t.Run("new file", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		withBot(t, f, "polecat")
		files := newMemFiles()
		files.chmodErr = errors.New("operation not permitted")
		var logs []string
		err := tokenProvisioner(f, files, false, false, &logs).EnsureToken("polecat")
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "IS stored") || !strings.Contains(err.Error(), "not lost") {
			t.Fatalf("err = %v, want an error saying the token IS stored in %s", err, path)
		}
		if _, ok := files.files[path]; !ok {
			t.Errorf("the token file is not there though the error says it is stored")
		}
		if v := f.token("bot-polecat", DefaultTokenName); v != "" && strings.Contains(err.Error(), v) {
			t.Errorf("the error carries the minted token")
		}
	})
}

// methodFailsTransport fails every request of one method as a broken connection
// does, and passes the rest to next.
type methodFailsTransport struct {
	next   http.RoundTripper
	method string
}

func (m methodFailsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == m.method {
		return nil, errors.New("connection reset by peer")
	}
	return m.next.RoundTrip(req)
}

// TestEnsureTokenTransportFailures checks a request that never gets an answer fails
// the run at each of its three calls, with no file written and no token in the
// error.
func TestEnsureTokenTransportFailures(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			withBot(t, f, "polecat")
			var logs []string
			// A first mint makes the rotation list, delete and mint all happen.
			if method != http.MethodPost {
				if err := tokenProvisioner(f, newMemFiles(), false, false, &logs).EnsureToken("polecat"); err != nil {
					t.Fatalf("seed token: %v", err)
				}
			}
			files := newMemFiles()
			files.seed(filepath.Join(tokenDir, "forgejo-polecat.env"), "old", 0o600)
			err := tokenProvisioner(methodFailsTransport{next: f, method: method}, files, true, false, &logs).EnsureToken("polecat")
			if err == nil || !strings.Contains(err.Error(), "request failed") {
				t.Fatalf("err = %v, want a request failed error", err)
			}
			if string(files.files[filepath.Join(tokenDir, "forgejo-polecat.env")]) != "old" {
				t.Errorf("the file changed though the %s failed", method)
			}
			if strings.Contains(err.Error(), fixtureToken) {
				t.Errorf("the error carries the admin token")
			}
		})
	}
}

// echoShapes are the ways a server or proxy might echo the admin token back in a
// reply that is not JSON: as plain text, as an Authorization line, as a Bearer
// credential.
func echoShapes() map[string]string {
	return map[string]string{
		"plain text":         "invalid credential " + fixtureToken + " for user",
		"authorization line": "request headers:\nAuthorization: token " + fixtureToken + "\nAccept: application/json",
		"bearer":             "rejected Bearer " + fixtureToken + " as expired",
	}
}

// TestMintAndRevokeRepliesAreScrubbed drives the 403 path, the unexpected-status
// path and both revoke warnings with a reply that echoes the admin token in text
// that is not JSON, and checks neither the error nor the warning carries it. The
// replies go through the scrubbing Client methods from gt-3r55b (62531b83).
func TestMintAndRevokeRepliesAreScrubbed(t *testing.T) {
	t.Parallel()
	for shape, body := range echoShapes() {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			check := func(t *testing.T, where, text string) {
				t.Helper()
				if strings.Contains(text, fixtureToken) {
					t.Errorf("%s carries the admin token: %q", where, redactToken(text))
				}
				if !strings.Contains(text, redacted) {
					t.Errorf("%s lacks the masked marker, so the reply was not quoted through the scrubber: %q", where, text)
				}
			}
			for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
				var logs []string
				err := tokenProvisioner(noUsableTransport{status: status, body: body}, newMemFiles(), false, false, &logs).EnsureToken("polecat")
				if err == nil {
					t.Fatalf("status %d: no error", status)
				}
				check(t, "the mint error", err.Error())
			}
			for name, tr := range map[string]func(http.RoundTripper) http.RoundTripper{
				"list": func(n http.RoundTripper) http.RoundTripper {
					return failingRevokeTransport{next: n, listStatus: 500, body: body}
				},
				"delete": func(n http.RoundTripper) http.RoundTripper {
					return failingRevokeTransport{next: n, delStatus: 500, body: body}
				},
			} {
				f := newFakeForgejo()
				withBot(t, f, "polecat")
				var logs []string
				if name == "delete" {
					if err := tokenProvisioner(f, newMemFiles(), false, false, &logs).EnsureToken("polecat"); err != nil {
						t.Fatalf("seed token: %v", err)
					}
					logs = nil
				}
				files := newMemFiles()
				files.seed(filepath.Join(tokenDir, "forgejo-polecat.env"), "old", 0o600)
				_ = tokenProvisioner(tr(f), files, true, false, &logs).EnsureToken("polecat")
				warning := ""
				for _, l := range logs {
					if strings.HasPrefix(l, "warning:") {
						warning = l
					}
				}
				if warning == "" {
					t.Fatalf("%s: no warning was logged: %v", name, logs)
				}
				check(t, name+" warning", warning)
			}
		})
	}
}

// TestRotateMintFailureSaysTheOldTokenIsRevoked checks a mint that fails after the
// old token was revoked says so, with the file path and what to do, and that a
// mint failing with nothing revoked does not claim it.
func TestRotateMintFailureSaysTheOldTokenIsRevoked(t *testing.T) {
	t.Parallel()
	path := filepath.Join(tokenDir, "forgejo-polecat.env")
	t.Run("revoked then mint fails", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		withBot(t, f, "polecat")
		var logs []string
		if err := tokenProvisioner(f, newMemFiles(), false, false, &logs).EnsureToken("polecat"); err != nil {
			t.Fatalf("seed token: %v", err)
		}
		files := newMemFiles()
		files.seed(path, "old", 0o600)
		err := tokenProvisioner(methodFailsTransport{next: f, method: http.MethodPost}, files, true, false, &logs).EnsureToken("polecat")
		if err == nil || !strings.Contains(err.Error(), "already revoked") || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "run again with --rotate") {
			t.Fatalf("err = %v, want the revoked-token explanation naming %s", err, path)
		}
		if string(files.files[path]) != "old" {
			t.Errorf("the file changed though the mint failed")
		}
	})
	t.Run("nothing revoked", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		withBot(t, f, "polecat")
		files := newMemFiles()
		files.seed(path, "old", 0o600)
		var logs []string
		err := tokenProvisioner(methodFailsTransport{next: f, method: http.MethodPost}, files, true, false, &logs).EnsureToken("polecat")
		if err == nil {
			t.Fatal("no error")
		}
		if strings.Contains(err.Error(), "already revoked") {
			t.Errorf("the error claims a revoke that did not happen: %v", err)
		}
	})
}
