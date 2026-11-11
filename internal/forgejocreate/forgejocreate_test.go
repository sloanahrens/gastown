package forgejocreate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// The repository, author and token every test uses. The token is distinctive so a test can
// look for it in an error.
const (
	testOwner  = "sloan"
	testName   = "notes"
	testAuthor = "Ada Lovelace"
	testToken  = "tok-admin-value"
)

// request is one call the fake Forgejo received.
type request struct {
	method string
	path   string
	auth   string
	body   []byte
}

// fakeForgejo is an httptest Forgejo for the three routes the command uses. Each status and the
// seed reply can be overridden before the test starts.
type fakeForgejo struct {
	repoStatus   int    // GET of the repository; 404 means it does not exist
	createStatus int    // POST /admin/users/{owner}/repos
	seedStatus   int    // POST /repos/{owner}/{name}/contents
	seedReply    string // the contents reply body
	down         bool   // every request fails at the transport, as an unreachable Forgejo does
	writesDown   bool   // every non-GET request fails at the transport

	mu   sync.Mutex
	reqs []request
}

// newFake starts a fake whose routes answer with the defaults a healthy Forgejo gives.
func newFake(t *testing.T, configure func(*fakeForgejo)) *fakeForgejo {
	t.Helper()
	f := &fakeForgejo{
		repoStatus:   http.StatusNotFound,
		createStatus: http.StatusCreated,
		seedStatus:   http.StatusCreated,
		seedReply:    `{"commit":{"sha":"abc123"}}`,
	}
	if configure != nil {
		configure(f)
	}
	return f
}

// fakeAPI is the API root the fake answers for. No socket is opened: RoundTrip hands each request
// to the fake's handler in-process.
const fakeAPI = "http://forgejo.test/api/v1"

// api is the API root the client is pointed at.
func (f *fakeForgejo) api() string { return fakeAPI }

// RoundTrip answers a request in-process, so the unit tier opens no listener.
func (f *fakeForgejo) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.down || (f.writesDown && r.Method != http.MethodGet) {
		return nil, errors.New("connection refused")
	}
	rec := httptest.NewRecorder()
	f.serve(rec, r)
	return rec.Result(), nil
}

// client is a forgejoprov.Client signed in with testToken, reaching the fake.
func (f *fakeForgejo) client(t *testing.T) *forgejoprov.Client {
	t.Helper()
	secret, err := forgejoprov.ResolveAdminToken(
		func(key string) string {
			if key == "FORGEJO_ADMIN_TOKEN" {
				return testToken
			}
			return ""
		},
		func(string) ([]byte, error) { return nil, os.ErrNotExist },
		"",
	)
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	return &forgejoprov.Client{API: f.api(), HTTP: &http.Client{Transport: f}, Token: secret, Ctx: context.Background()}
}

// requests returns a copy of every call received, in order.
func (f *fakeForgejo) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.reqs...)
}

// writes returns the calls that are not GET.
func (f *fakeForgejo) writes() []request {
	var out []request
	for _, r := range f.requests() {
		if r.method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeForgejo) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, request{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
	f.mu.Unlock()

	repoPath := "/api/v1/repos/" + testOwner + "/" + testName
	switch {
	case r.Method == http.MethodGet && r.URL.Path == repoPath:
		w.WriteHeader(f.repoStatus)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/users/"+testOwner+"/repos":
		w.WriteHeader(f.createStatus)
		_, _ = io.WriteString(w, `{"name":"`+testName+`"}`)
	case r.Method == http.MethodPost && r.URL.Path == repoPath+"/contents":
		w.WriteHeader(f.seedStatus)
		_, _ = io.WriteString(w, f.seedReply)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// seedDir writes files under a fresh temporary directory and returns its absolute path.
func seedDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for rel, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestValidateRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		repo      string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{"owner and name", "sloan/notes", "sloan", "notes", false},
		{"dots and dashes", "sloan/my.repo-2", "sloan", "my.repo-2", false},
		{"no slash", "sloan", "", "", true},
		{"extra slash", "sloan/notes/extra", "", "", true},
		{"empty owner", "/notes", "", "", true},
		{"traversal name", "sloan/../notes", "", "", true},
		{"space in name", "sloan/no tes", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner, name, err := ValidateRepo(tc.repo)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateRepo(%q) error = %v, want error %v", tc.repo, err, tc.wantErr)
			}
			if owner != tc.wantOwner || name != tc.wantName {
				t.Fatalf("ValidateRepo(%q) = %q, %q; want %q, %q", tc.repo, owner, name, tc.wantOwner, tc.wantName)
			}
		})
	}
}

func TestLoadSeedReadsEveryFileAsASlashPathInOrder(t *testing.T) {
	t.Parallel()
	binary := []byte{0, 255, 10, 13}
	dir := seedDir(t, map[string][]byte{
		"README.md":     []byte("# notes\n"),
		"bin/tool.bin":  binary,
		"sub/deep/a.md": []byte("deep"),
	})
	files, err := LoadSeed(dir)
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	want := []struct {
		path string
		data []byte
	}{
		{"README.md", []byte("# notes\n")},
		{"bin/tool.bin", binary},
		{"sub/deep/a.md", []byte("deep")},
	}
	if len(files) != len(want) {
		t.Fatalf("LoadSeed returned %d files, want %d: %+v", len(files), len(want), files)
	}
	for i, w := range want {
		if files[i].Path != w.path || string(files[i].Data) != string(w.data) {
			t.Errorf("file %d = {%q %q}, want {%q %q}", i, files[i].Path, files[i].Data, w.path, w.data)
		}
	}
}

// TestLoadSeedRefusals gives each refusal a row. Each row names the cause the refusal text must
// carry, so a refusal that says nothing useful fails.
func TestLoadSeedRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		dir      func(t *testing.T) string
		wantText string
	}{
		{
			name:     "relative path",
			dir:      func(*testing.T) string { return "seed" },
			wantText: "relative",
		},
		{
			name:     "missing directory",
			dir:      func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			wantText: "absent",
		},
		{
			name: "a file, not a directory",
			dir: func(t *testing.T) string {
				p := filepath.Join(seedDir(t, map[string][]byte{"f": []byte("x")}), "f")
				return p
			},
			wantText: "not a directory",
		},
		{
			name:     "directory with no files",
			dir:      func(t *testing.T) string { return t.TempDir() },
			wantText: "holds no files",
		},
		{
			name: "file over 1 MiB, named",
			dir: func(t *testing.T) string {
				return seedDir(t, map[string][]byte{
					"small.txt": []byte("ok"),
					"big.bin":   make([]byte, MaxFileBytes+1),
				})
			},
			wantText: "big.bin",
		},
		{
			name: "symlink is refused, not followed",
			dir: func(t *testing.T) string {
				dir := seedDir(t, map[string][]byte{"target.txt": []byte("t")})
				if err := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "link.txt")); err != nil {
					t.Fatalf("create symlink: %v", err)
				}
				return dir
			},
			wantText: "link.txt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files, err := LoadSeed(tc.dir(t))
			if err == nil {
				t.Fatalf("LoadSeed succeeded with %d files; want a refusal", len(files))
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("LoadSeed error %q does not name %q", err, tc.wantText)
			}
		})
	}
}

// TestLoadSeedAcceptsExactlyTheLimit pins the boundary: a file of exactly MaxFileBytes is sent.
func TestLoadSeedAcceptsExactlyTheLimit(t *testing.T) {
	t.Parallel()
	dir := seedDir(t, map[string][]byte{"edge.bin": make([]byte, MaxFileBytes)})
	files, err := LoadSeed(dir)
	if err != nil {
		t.Fatalf("LoadSeed refused a file of exactly the limit: %v", err)
	}
	if len(files) != 1 || len(files[0].Data) != MaxFileBytes {
		t.Fatalf("LoadSeed = %d files, want one of %d bytes", len(files), MaxFileBytes)
	}
}

func TestCreateRepoSendsPrivateEmptyMainRepository(t *testing.T) {
	t.Parallel()
	f := newFake(t, nil)
	if err := CreateRepo(f.client(t), testOwner, testName); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("CreateRepo sent %d requests, want a read then a create: %+v", len(reqs), reqs)
	}
	if reqs[0].method != http.MethodGet || reqs[0].path != "/api/v1/repos/sloan/notes" {
		t.Errorf("first request = %s %s, want the read of the repository", reqs[0].method, reqs[0].path)
	}
	create := reqs[1]
	if create.method != http.MethodPost || create.path != "/api/v1/admin/users/sloan/repos" {
		t.Fatalf("second request = %s %s, want POST to the admin create route", create.method, create.path)
	}
	var body map[string]any
	if err := json.Unmarshal(create.body, &body); err != nil {
		t.Fatalf("create body is not JSON: %v", err)
	}
	if body["name"] != testName || body["private"] != true || body["auto_init"] != false || body["default_branch"] != "main" {
		t.Errorf("create body = %v, want name notes, private true, auto_init false, default_branch main", body)
	}
	if create.auth != "token "+testToken {
		t.Errorf("Authorization = %q, want the admin token in the header", create.auth)
	}
}

func TestCreateRepoRefusesExistingRepositoryBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	f := newFake(t, func(f *fakeForgejo) { f.repoStatus = http.StatusOK })
	err := CreateRepo(f.client(t), testOwner, testName)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateRepo error = %v, want an already-exists refusal", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("CreateRepo sent %d writes for an existing repository: %+v", len(w), w)
	}
}

func TestCreateRepoRefusesUnexpectedReadBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	f := newFake(t, func(f *fakeForgejo) { f.repoStatus = http.StatusInternalServerError })
	err := CreateRepo(f.client(t), testOwner, testName)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("CreateRepo error = %v, want the unexpected HTTP 500 named", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("CreateRepo sent %d writes after a failed read: %+v", len(w), w)
	}
}

func TestCreateRepoRefusesRejectedCreate(t *testing.T) {
	t.Parallel()
	f := newFake(t, func(f *fakeForgejo) { f.createStatus = http.StatusForbidden })
	err := CreateRepo(f.client(t), testOwner, testName)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("CreateRepo error = %v, want the rejected create's HTTP 403 named", err)
	}
}

func TestSeedSendsEveryFileInOneCommit(t *testing.T) {
	t.Parallel()
	f := newFake(t, nil)
	files := []File{
		{Path: "README.md", Data: []byte("# notes\n")},
		{Path: "bin/tool.bin", Data: []byte{0, 255, 10}},
	}
	sha, err := Seed(f.client(t), testOwner, testName, testAuthor, files)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if sha != "abc123" {
		t.Errorf("Seed returned %q, want the commit SHA abc123", sha)
	}
	w := f.writes()
	if len(w) != 1 || w[0].path != "/api/v1/repos/sloan/notes/contents" {
		t.Fatalf("Seed sent writes %+v, want one POST to the contents route", w)
	}
	var body struct {
		Branch    string            `json:"branch"`
		Message   string            `json:"message"`
		Author    map[string]string `json:"author"`
		Committer map[string]string `json:"committer"`
		Files     []struct {
			Operation string `json:"operation"`
			Path      string `json:"path"`
			Content   string `json:"content"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w[0].body, &body); err != nil {
		t.Fatalf("contents body is not JSON: %v", err)
	}
	if body.Branch != "main" || body.Message != "Seed sloan/notes" {
		t.Errorf("branch=%q message=%q, want main and Seed sloan/notes", body.Branch, body.Message)
	}
	if body.Author["name"] != testAuthor || body.Committer["name"] != testAuthor {
		t.Errorf("author=%v committer=%v, want %q on both", body.Author, body.Committer, testAuthor)
	}
	if len(body.Files) != len(files) {
		t.Fatalf("contents carried %d files, want %d", len(body.Files), len(files))
	}
	for i, got := range body.Files {
		raw, err := base64.StdEncoding.DecodeString(got.Content)
		if err != nil {
			t.Fatalf("file %d content is not base64: %v", i, err)
		}
		if got.Operation != "create" || got.Path != files[i].Path || string(raw) != string(files[i].Data) {
			t.Errorf("file %d = {%s %s %q}, want create %s %q", i, got.Operation, got.Path, raw, files[i].Path, files[i].Data)
		}
	}
}

func TestSeedRefusesRejectedCommit(t *testing.T) {
	t.Parallel()
	f := newFake(t, func(f *fakeForgejo) { f.seedStatus = http.StatusInternalServerError })
	_, err := Seed(f.client(t), testOwner, testName, testAuthor, []File{{Path: "a", Data: []byte("a")}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("Seed error = %v, want the rejected commit's HTTP 500 named", err)
	}
}

func TestSeedRefusesReplyWithoutACommit(t *testing.T) {
	t.Parallel()
	f := newFake(t, func(f *fakeForgejo) { f.seedReply = `{}` })
	_, err := Seed(f.client(t), testOwner, testName, testAuthor, []File{{Path: "a", Data: []byte("a")}})
	if err == nil || !strings.Contains(err.Error(), "no commit") {
		t.Fatalf("Seed error = %v, want a refusal naming the missing commit", err)
	}
}

// TestErrorsNeverCarryTheToken: a refusal whose reply echoes the token must not print it.
func TestErrorsNeverCarryTheToken(t *testing.T) {
	t.Parallel()
	echo := `{"message":"rejected token ` + testToken + `"}`
	f := newFake(t, func(f *fakeForgejo) {
		f.createStatus = http.StatusForbidden
		f.seedStatus = http.StatusForbidden
		f.seedReply = echo
	})
	client := f.client(t)
	errs := []error{CreateRepo(client, testOwner, testName)}
	_, err := Seed(client, testOwner, testName, testAuthor, []File{{Path: "a", Data: []byte("a")}})
	errs = append(errs, err)
	for _, e := range errs {
		if e == nil {
			t.Fatal("expected a refusal from the echoing fake")
		}
		if strings.Contains(e.Error(), testToken) {
			t.Fatalf("error carries the token: %v", e)
		}
	}
}
