package forgejocreate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejotest"
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

// client is a forgejoprov.Client signed in with testToken, reaching the fake.
func client(t *testing.T, f *forgejotest.RepoFake) *forgejoprov.Client {
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
	return &forgejoprov.Client{API: f.API(), HTTP: &http.Client{Transport: f}, Token: secret, Ctx: context.Background()}
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

// seedOfSize returns a seed directory of n files of size bytes each, named so they sort in
// creation order.
func seedOfSize(t *testing.T, n, size int) string {
	t.Helper()
	files := make(map[string][]byte, n)
	for i := range n {
		files[fmt.Sprintf("f%04d", i)] = make([]byte, size)
	}
	return seedDir(t, files)
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

// TestLoadSeedRefusesASeedOverTheTotalLimit: a seed over MaxSeedBytes is refused, and the file
// that crossed the limit is named.
func TestLoadSeedRefusesASeedOverTheTotalLimit(t *testing.T) {
	t.Parallel()
	files, err := LoadSeed(seedOfSize(t, MaxSeedBytes/MaxFileBytes+1, MaxFileBytes))
	if err == nil {
		t.Fatalf("LoadSeed succeeded with %d files; want a refusal", len(files))
	}
	if !strings.Contains(err.Error(), "total limit") {
		t.Fatalf("LoadSeed error %q does not name the total limit", err)
	}
	if want := fmt.Sprintf("f%04d", MaxSeedBytes/MaxFileBytes); !strings.Contains(err.Error(), want) {
		t.Fatalf("LoadSeed error %q does not name %s", err, want)
	}
}

// TestLoadSeedAcceptsExactlyTheTotalLimit pins the boundary: files totalling MaxSeedBytes are
// sent.
func TestLoadSeedAcceptsExactlyTheTotalLimit(t *testing.T) {
	t.Parallel()
	files, err := LoadSeed(seedOfSize(t, MaxSeedBytes/MaxFileBytes, MaxFileBytes))
	if err != nil {
		t.Fatalf("LoadSeed refused files of exactly the total limit: %v", err)
	}
	if len(files) != MaxSeedBytes/MaxFileBytes {
		t.Fatalf("LoadSeed = %d files, want %d", len(files), MaxSeedBytes/MaxFileBytes)
	}
}

func TestCreateRepoSendsPrivateEmptyMainRepository(t *testing.T) {
	t.Parallel()
	f := forgejotest.NewRepoFake(testOwner, testName)
	if err := CreateRepo(client(t, f), testOwner, testName); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("CreateRepo sent %d requests, want a read then a create: %+v", len(reqs), reqs)
	}
	if reqs[0].Method != http.MethodGet || reqs[0].Path != "/api/v1/repos/sloan/notes" {
		t.Errorf("first request = %s %s, want the read of the repository", reqs[0].Method, reqs[0].Path)
	}
	create := reqs[1]
	if create.Method != http.MethodPost || create.Path != "/api/v1/admin/users/sloan/repos" {
		t.Fatalf("second request = %s %s, want POST to the admin create route", create.Method, create.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(create.Body, &body); err != nil {
		t.Fatalf("create body is not JSON: %v", err)
	}
	if body["name"] != testName || body["private"] != true || body["auto_init"] != false || body["default_branch"] != "main" {
		t.Errorf("create body = %v, want name notes, private true, auto_init false, default_branch main", body)
	}
	if create.Auth != "token "+testToken {
		t.Errorf("Authorization = %q, want the admin token in the header", create.Auth)
	}
}

func TestCreateRepoRefusesExistingRepositoryBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	f := forgejotest.NewRepoFake(testOwner, testName)
	f.RepoStatus = http.StatusOK
	err := CreateRepo(client(t, f), testOwner, testName)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateRepo error = %v, want an already-exists refusal", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("CreateRepo sent %d writes for an existing repository: %+v", len(w), w)
	}
}

func TestCreateRepoRefusesUnexpectedReadBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	f := forgejotest.NewRepoFake(testOwner, testName)
	f.RepoStatus = http.StatusInternalServerError
	err := CreateRepo(client(t, f), testOwner, testName)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("CreateRepo error = %v, want the unexpected HTTP 500 named", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("CreateRepo sent %d writes after a failed read: %+v", len(w), w)
	}
}

func TestCreateRepoRefusesRejectedCreate(t *testing.T) {
	t.Parallel()
	f := forgejotest.NewRepoFake(testOwner, testName)
	f.CreateStatus = http.StatusForbidden
	err := CreateRepo(client(t, f), testOwner, testName)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("CreateRepo error = %v, want the rejected create's HTTP 403 named", err)
	}
}

func TestSeedSendsEveryFileInOneCommit(t *testing.T) {
	t.Parallel()
	f := forgejotest.NewRepoFake(testOwner, testName)
	files := []File{
		{Path: "README.md", Data: []byte("# notes\n")},
		{Path: "bin/tool.bin", Data: []byte{0, 255, 10}},
	}
	sha, err := Seed(client(t, f), testOwner, testName, testAuthor, files)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if sha != "abc123" {
		t.Errorf("Seed returned %q, want the commit SHA abc123", sha)
	}
	w := f.Writes()
	if len(w) != 1 || w[0].Path != "/api/v1/repos/sloan/notes/contents" {
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
	if err := json.Unmarshal(w[0].Body, &body); err != nil {
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
	f := forgejotest.NewRepoFake(testOwner, testName)
	f.SeedStatus = http.StatusInternalServerError
	_, err := Seed(client(t, f), testOwner, testName, testAuthor, []File{{Path: "a", Data: []byte("a")}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("Seed error = %v, want the rejected commit's HTTP 500 named", err)
	}
}

func TestSeedRefusesReplyWithoutACommit(t *testing.T) {
	t.Parallel()
	f := forgejotest.NewRepoFake(testOwner, testName)
	f.SeedReply = `{}`
	_, err := Seed(client(t, f), testOwner, testName, testAuthor, []File{{Path: "a", Data: []byte("a")}})
	if err == nil || !strings.Contains(err.Error(), "no commit") {
		t.Fatalf("Seed error = %v, want a refusal naming the missing commit", err)
	}
}

// TestSeedRefusesASeedOverTheTotalLimitBeforeAnyRequest: the body is never built for a seed the
// total cap refuses.
func TestSeedRefusesASeedOverTheTotalLimitBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	f := forgejotest.NewRepoFake(testOwner, testName)
	buf := make([]byte, MaxFileBytes)
	files := make([]File, 0, MaxSeedBytes/MaxFileBytes+1)
	for i := 0; i <= MaxSeedBytes/MaxFileBytes; i++ {
		files = append(files, File{Path: fmt.Sprintf("f%04d", i), Data: buf})
	}
	_, err := Seed(client(t, f), testOwner, testName, testAuthor, files)
	if err == nil || !strings.Contains(err.Error(), "total limit") {
		t.Fatalf("Seed error = %v, want a refusal naming the total limit", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("Seed sent %d writes for a seed over the total limit: %+v", len(w), w)
	}
}

// TestErrorsNeverCarryTheToken: a refusal whose reply echoes the token must not print it.
func TestErrorsNeverCarryTheToken(t *testing.T) {
	t.Parallel()
	echo := `{"message":"rejected token ` + testToken + `"}`
	f := forgejotest.NewRepoFake(testOwner, testName)
	f.CreateStatus = http.StatusForbidden
	f.SeedStatus = http.StatusForbidden
	f.SeedReply = echo
	c := client(t, f)
	errs := []error{CreateRepo(c, testOwner, testName)}
	_, err := Seed(c, testOwner, testName, testAuthor, []File{{Path: "a", Data: []byte("a")}})
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
