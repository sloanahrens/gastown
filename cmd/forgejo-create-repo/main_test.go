package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

// The token every run is signed in with, and the repository the runs create. The token is
// distinctive so a test can look for it in the output.
const (
	testToken = "tok-broker-value"
	fileToken = "tok-file-value"
	testRepo  = "sloan/notes"
	testOwner = "sloan"
	testName  = "notes"
)

// request is one call the fake Forgejo received.
type request struct {
	method string
	path   string
	auth   string
	body   []byte
}

// fakeForgejo is an httptest Forgejo for the three routes the command uses.
type fakeForgejo struct {
	repoStatus   int    // GET of the repository; 404 means it does not exist
	createStatus int    // POST /admin/users/{owner}/repos
	seedStatus   int    // POST /repos/{owner}/{name}/contents
	seedReply    string // the contents reply body

	mu   sync.Mutex
	reqs []request
}

// newFake starts a fake that answers the way a healthy Forgejo does, with the overrides set by
// configure.
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

// fakeAPI is the API root the command is pointed at with --api-url. No socket is opened: RoundTrip
// hands each request to the fake's handler in-process.
const fakeAPI = "http://forgejo.test/api/v1"

// apiURL is the API root the command is pointed at with --api-url.
func (f *fakeForgejo) apiURL() string { return fakeAPI }

// RoundTrip answers a request in-process, so the unit tier opens no listener.
func (f *fakeForgejo) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.serve(rec, r)
	return rec.Result(), nil
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

	repoPath := "/api/v1/repos/" + testRepo
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

// seedDir writes a two-file seed under a fresh temporary directory and returns its absolute
// path: a text README and a binary file.
func seedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "tool.bin"), []byte{0, 255, 10}, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// environ is a Getenv over a fixed map: nothing is read from the host environment.
func environ(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// testEnv is the env seam a run reads through, reaching the fake through its HTTP client.
func testEnv(vars map[string]string, f *fakeForgejo) env {
	e := env{Getenv: environ(vars), ReadFile: os.ReadFile}
	if f != nil {
		e.HTTP = &http.Client{Transport: f}
	}
	return e
}

// result is what one run printed and the code it returned.
type result struct {
	code   int
	stdout string
	stderr string
}

// runCmd drives run with args, the env seam and the streams, and returns what came back.
func runCmd(t *testing.T, e env, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, e, &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// realArgs is the command line of a real run against the fake.
func realArgs(f *fakeForgejo, seed string) []string {
	return []string{"--api-url", f.apiURL(), "--repo", testRepo, "--seed-dir", seed, "--author", "Ada Lovelace"}
}

func TestHelpDocumentsFlagsAndTokenSource(t *testing.T) {
	t.Parallel()
	got := runCmd(t, testEnv(nil, nil), "--help")
	if got.code != forgejoprov.ExitOK {
		t.Fatalf("--help exit = %d, want %d", got.code, forgejoprov.ExitOK)
	}
	for _, want := range []string{"--repo", "--seed-dir", "--author", "--dry-run", "--admin-token-file", "GT_AUTH_FORGEJO_TOKEN", "FORGEJO_ADMIN_TOKEN"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("--help does not document %s:\n%s", want, got.stdout)
		}
	}
}

// TestUsageRefusalsExitTwo: a refused command line exits ExitUsage and sends nothing.
func TestUsageRefusalsExitTwo(t *testing.T) {
	t.Parallel()
	seed := seedDir(t)
	cases := []struct {
		name     string
		args     []string
		wantText string
	}{
		{"missing repo", []string{"--seed-dir", seed, "--author", "a"}, "--repo is required"},
		{"repo not OWNER/NAME", []string{"--repo", "sloan", "--seed-dir", seed, "--author", "a"}, "OWNER/NAME"},
		{"missing seed dir", []string{"--repo", testRepo, "--author", "a"}, "--seed-dir is required"},
		{"relative seed dir", []string{"--repo", testRepo, "--seed-dir", "seed", "--author", "a"}, "relative"},
		{"missing author", []string{"--repo", testRepo, "--seed-dir", seed}, "--author is required"},
		{"unknown flag", []string{"--repo", testRepo, "--bogus"}, "bogus"},
		{"stray argument", []string{"--repo", testRepo, "--seed-dir", seed, "--author", "a", "extra"}, "extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t, nil)
			got := runCmd(t, testEnv(map[string]string{"GT_AUTH_FORGEJO_TOKEN": testToken}, f), append([]string{"--api-url", f.apiURL()}, tc.args...)...)
			if got.code != forgejoprov.ExitUsage {
				t.Fatalf("exit = %d, want ExitUsage %d; stderr %q", got.code, forgejoprov.ExitUsage, got.stderr)
			}
			if !strings.Contains(got.stderr, tc.wantText) {
				t.Fatalf("stderr %q does not name %q", got.stderr, tc.wantText)
			}
			if n := len(f.requests()); n != 0 {
				t.Fatalf("refused command line sent %d requests", n)
			}
		})
	}
}

// TestSeedAndRepoRefusalsSendNoWrite: each refusal of the seed or the repository exits ExitFailure,
// names its cause, and sends no write.
func TestSeedAndRepoRefusalsSendNoWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		seed     func(t *testing.T) string
		fake     func(*fakeForgejo)
		wantText string
	}{
		{
			name: "file over 1 MiB",
			seed: func(t *testing.T) string {
				dir := seedDir(t)
				if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 1<<20+1), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantText: "big.bin",
		},
		{
			name:     "existing repository",
			seed:     func(t *testing.T) string { return seedDir(t) },
			fake:     func(f *fakeForgejo) { f.repoStatus = http.StatusOK },
			wantText: "already exists",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t, tc.fake)
			got := runCmd(t, testEnv(map[string]string{"GT_AUTH_FORGEJO_TOKEN": testToken}, f), realArgs(f, tc.seed(t))...)
			if got.code != forgejoprov.ExitFailure {
				t.Fatalf("exit = %d, want ExitFailure %d; stderr %q", got.code, forgejoprov.ExitFailure, got.stderr)
			}
			if !strings.Contains(got.stderr, tc.wantText) {
				t.Fatalf("stderr %q does not name %q", got.stderr, tc.wantText)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("refused run sent %d writes: %+v", len(w), w)
			}
		})
	}
}

// TestDryRunPrintsPlanAndSendsNothing: the plan lists the files and the message, and no request
// is sent. The dry run needs no token, so the environment is empty.
func TestDryRunPrintsPlanAndSendsNothing(t *testing.T) {
	t.Parallel()
	f := newFake(t, nil)
	args := append(realArgs(f, seedDir(t)), "--dry-run")
	got := runCmd(t, testEnv(nil, f), args...)
	if got.code != forgejoprov.ExitOK {
		t.Fatalf("dry-run exit = %d, want ExitOK; stderr %q", got.code, got.stderr)
	}
	for _, want := range []string{"dry run", "repository: sloan/notes (private, default branch main", "message: Seed sloan/notes", "README.md (8 bytes)", "bin/tool.bin (3 bytes)", "author: Ada Lovelace"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("dry-run plan does not contain %q:\n%s", want, got.stdout)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("dry run sent %d requests: %+v", n, f.requests())
	}
}

// TestRealRunCreatesThenSeeds: a real run reads, creates, then seeds in one commit, and reports
// the commit SHA.
func TestRealRunCreatesThenSeeds(t *testing.T) {
	t.Parallel()
	f := newFake(t, nil)
	got := runCmd(t, testEnv(map[string]string{"GT_AUTH_FORGEJO_TOKEN": testToken}, f), realArgs(f, seedDir(t))...)
	if got.code != forgejoprov.ExitOK {
		t.Fatalf("exit = %d, want ExitOK; stderr %q", got.code, got.stderr)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("sent %d requests, want read, create, contents: %+v", len(reqs), reqs)
	}
	if reqs[1].path != "/api/v1/admin/users/sloan/repos" || reqs[2].path != "/api/v1/repos/sloan/notes/contents" {
		t.Fatalf("request order = %s %s, want the create then the contents route", reqs[1].path, reqs[2].path)
	}
	var seed struct {
		Files []struct {
			Content string `json:"content"`
		} `json:"files"`
	}
	if err := json.Unmarshal(reqs[2].body, &seed); err != nil {
		t.Fatalf("contents body is not JSON: %v", err)
	}
	if len(seed.Files) != 2 {
		t.Fatalf("contents carried %d files, want 2", len(seed.Files))
	}
	if _, err := base64.StdEncoding.DecodeString(seed.Files[1].Content); err != nil {
		t.Fatalf("binary file is not base64: %v", err)
	}
	if !strings.Contains(got.stdout, "RESULT: seeded sloan/notes with 2 files in commit abc123") {
		t.Fatalf("stdout lacks the RESULT line:\n%s", got.stdout)
	}
}

// TestTokenFromEnvironmentBeforeFile: the environment token wins over the token file, which is
// read only when the environment holds none.
func TestTokenFromEnvironmentBeforeFile(t *testing.T) {
	t.Parallel()
	tokenFile := filepath.Join(t.TempDir(), "admin.env")
	if err := os.WriteFile(tokenFile, []byte("FORGEJO_ADMIN_TOKEN="+fileToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFake(t, nil)
	args := append(realArgs(f, seedDir(t)), "--admin-token-file", tokenFile)
	got := runCmd(t, testEnv(map[string]string{"GT_AUTH_FORGEJO_TOKEN": testToken}, f), args...)
	if got.code != forgejoprov.ExitOK {
		t.Fatalf("exit = %d, want ExitOK; stderr %q", got.code, got.stderr)
	}
	for _, r := range f.requests() {
		if r.auth != "token "+testToken {
			t.Fatalf("%s %s sent Authorization %q, want the environment token", r.method, r.path, r.auth)
		}
	}
}

// TestTokenFromFileWhenEnvironmentHoldsNone: with no token in the environment the token file is
// the source.
func TestTokenFromFileWhenEnvironmentHoldsNone(t *testing.T) {
	t.Parallel()
	tokenFile := filepath.Join(t.TempDir(), "admin.env")
	if err := os.WriteFile(tokenFile, []byte("FORGEJO_ADMIN_TOKEN="+fileToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFake(t, nil)
	args := append(realArgs(f, seedDir(t)), "--admin-token-file", tokenFile)
	got := runCmd(t, testEnv(nil, f), args...)
	if got.code != forgejoprov.ExitOK {
		t.Fatalf("exit = %d, want ExitOK; stderr %q", got.code, got.stderr)
	}
	for _, r := range f.requests() {
		if r.auth != "token "+fileToken {
			t.Fatalf("%s %s sent Authorization %q, want the file token", r.method, r.path, r.auth)
		}
	}
}

// TestNoTokenRefusesBeforeAnyRequest: a real run with no token in any source is a failure that
// sends nothing.
func TestNoTokenRefusesBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	f := newFake(t, nil)
	got := runCmd(t, testEnv(nil, f), realArgs(f, seedDir(t))...)
	if got.code != forgejoprov.ExitFailure {
		t.Fatalf("exit = %d, want ExitFailure; stderr %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "no admin token") {
		t.Fatalf("stderr %q does not name the missing token", got.stderr)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("a run with no token sent %d requests", n)
	}
}

// TestSeedFailureSaysTheRepoIsEmpty: a failed seed after a successful create exits ExitFailure,
// says the repository is still empty, and prints no RESULT line.
func TestSeedFailureSaysTheRepoIsEmpty(t *testing.T) {
	t.Parallel()
	f := newFake(t, func(f *fakeForgejo) { f.seedStatus = http.StatusInternalServerError })
	got := runCmd(t, testEnv(map[string]string{"GT_AUTH_FORGEJO_TOKEN": testToken}, f), realArgs(f, seedDir(t))...)
	if got.code != forgejoprov.ExitFailure {
		t.Fatalf("exit = %d, want ExitFailure; stderr %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "still empty") {
		t.Fatalf("stderr %q does not say the repository is left empty", got.stderr)
	}
	if strings.Contains(got.stdout, "RESULT:") {
		t.Fatalf("a failed seed printed RESULT:\n%s", got.stdout)
	}
}

// TestOutputNeverCarriesTheToken: a refusal whose reply echoes the token prints no token.
func TestOutputNeverCarriesTheToken(t *testing.T) {
	t.Parallel()
	f := newFake(t, func(f *fakeForgejo) {
		f.seedStatus = http.StatusForbidden
		f.seedReply = `{"message":"rejected ` + testToken + `"}`
	})
	got := runCmd(t, testEnv(map[string]string{"GT_AUTH_FORGEJO_TOKEN": testToken}, f), realArgs(f, seedDir(t))...)
	if got.code != forgejoprov.ExitFailure {
		t.Fatalf("exit = %d, want ExitFailure", got.code)
	}
	if strings.Contains(got.stdout+got.stderr, testToken) {
		t.Fatalf("output carries the token:\nstdout %q\nstderr %q", got.stdout, got.stderr)
	}
}
