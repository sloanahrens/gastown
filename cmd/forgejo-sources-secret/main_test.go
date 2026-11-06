package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejotest"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// The admin token file the runs read, and the instant the token name is taken from.
const (
	adminFile = "/etc/gt/admin.env"
	testName  = "sources-token-20261106-203500"
	// leakyValue holds a quote, a backslash, a control character and non-ASCII
	// runes: a value a JSON encoder escapes, so an echo of it differs from the raw form.
	leakyValue = "q\"uote\\back<tag>&amp\x07noéπ"
)

// testClock is the instant every run takes its token name from.
func testClock() time.Time { return time.Date(2026, 11, 6, 20, 35, 0, 0, time.UTC) }

// adminReader returns a ReadFile that holds the admin token at adminFile and no
// other file.
func adminReader(t *testing.T) func(string) ([]byte, error) {
	t.Helper()
	return func(path string) ([]byte, error) {
		if path != adminFile {
			return nil, fs.ErrNotExist
		}
		return []byte("FORGEJO_ADMIN_TOKEN=" + forgejotest.AdminToken + "\n"), nil
	}
}

// newSourcesFake returns a fake holding the viewer account, the four repositories the
// run reads and the target repository, with bot-viewer a collaborator on the four.
// value is what the fake mints; empty keeps the fake's default.
func newSourcesFake(t *testing.T, value string) *forgejotest.Fake {
	t.Helper()
	f := forgejotest.New()
	f.ViewerExists = true
	if value != "" {
		f.NewToken = func() string { return value }
	}
	for _, repo := range readRepos {
		f.AddRepo(secretOwner + "/" + repo)
		grant(t, f, secretOwner+"/"+repo)
	}
	f.AddRepo(secretOwner + "/" + secretRepo)
	f.ClearCalls()
	return f
}

// grant makes bot-viewer a read collaborator on repo, as the operator does in the UI.
func grant(t *testing.T, f *forgejotest.Fake, repo string) {
	t.Helper()
	status, body := f.Call(t, http.MethodPut, "/repos/"+repo+"/collaborators/"+viewerAccount,
		forgejotest.JSONBody(map[string]string{"permission": "read"}))
	if status != http.StatusNoContent {
		t.Fatalf("grant on %s: HTTP %d %s", repo, status, body)
	}
}

// viewerNetwork is the network as the minted token meets it. The fake answers the admin
// token's calls. A repository read made with the minted token is answered from the
// viewer's collaborator grants, or from statuses set for the case, and only when the
// token is the viewer's and holds the read scope; every other call goes to the fake, which
// refuses it and records a violation.
type viewerNetwork struct {
	fake     *forgejotest.Fake
	statuses map[string]int
	reads    []string
	// cancelOnMint, when set, runs once the mint has been answered, as an interrupt
	// that lands after the token exists.
	cancelOnMint context.CancelFunc
	// failReads makes every read made with the minted token fail in transport, with no
	// reply.
	failReads bool
}

// errNoReply is the transport failure a read made with failReads meets.
var errNoReply = errors.New("no route to host")

func (v *viewerNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	// A request whose context is done never reaches the server, as on a real transport.
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	auth := req.Header.Get("Authorization")
	const repoRoot = forgejotest.APIPath + "/repos/"
	if v.cancelOnMint != nil && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, forgejotest.APIPath+"/admin/users/"+viewerAccount+"/tokens") {
		resp, err := v.fake.RoundTrip(req)
		v.cancelOnMint()
		return resp, err
	}
	if auth == "token "+forgejotest.AdminToken || req.Method != http.MethodGet || !strings.HasPrefix(req.URL.Path, repoRoot) {
		return v.fake.RoundTrip(req)
	}
	minted, ok := v.mintedValue()
	if !ok || auth != "token "+minted {
		return v.fake.RoundTrip(req)
	}
	repo := strings.TrimPrefix(req.URL.Path, repoRoot)
	v.reads = append(v.reads, repo)
	if v.failReads {
		return nil, errNoReply
	}
	status := http.StatusOK
	scopes, _ := v.fake.TokenScopes(viewerAccount, testName)
	switch {
	case !slices.Contains(scopes, tokenScope):
		status = http.StatusForbidden
	case v.statuses[repo] != 0:
		status = v.statuses[repo]
	case v.fake.Permission(repo, viewerAccount) == "":
		status = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(forgejotest.JSONBody(map[string]string{"full_name": repo}))),
		Request:    req,
	}, nil
}

// mintedValue returns the value of the token this run minted, when it minted one.
func (v *viewerNetwork) mintedValue() (string, bool) {
	value := v.fake.Token(viewerAccount, testName)
	return value, value != ""
}

// runResult is what one run of the command produced.
type runResult struct {
	code   int
	stdout string
	stderr string
	net    *viewerNetwork
}

// runCommand runs the command against f with the given statuses overriding the
// viewer's grants, and extra arguments after the admin token file.
func runCommand(t *testing.T, f *forgejotest.Fake, statuses map[string]int, extra ...string) runResult {
	t.Helper()
	return runNetwork(t, context.Background(), &viewerNetwork{fake: f, statuses: statuses}, extra...)
}

// runNetwork runs the command under ctx against net, with extra arguments after the
// admin token file.
func runNetwork(t *testing.T, ctx context.Context, net *viewerNetwork, extra ...string) runResult {
	t.Helper()
	args := append([]string{"--api-url", forgejotest.APIRoot + forgejotest.APIPath, "--admin-token-file", adminFile}, extra...)
	var stdout, stderr bytes.Buffer
	code := run(ctx, args, env{
		Now:      testClock,
		ReadFile: adminReader(t),
		HTTP:     &http.Client{Transport: net},
	}, &stdout, &stderr)
	return runResult{code: code, stdout: stdout.String(), stderr: stderr.String(), net: net}
}

// TestSourcesSecretStoresAfterFourOKReads runs the command where bot-viewer reads all
// four repositories. The command mints one token, reads the four with it, stores it on
// sloan/gastown and prints four status lines and the RESULT line, exiting 0.
func TestSourcesSecretStoresAfterFourOKReads(t *testing.T) {
	t.Parallel()
	f := newSourcesFake(t, "")
	res := runCommand(t, f, nil)

	if res.code != forgejoprov.ExitOK {
		t.Fatalf("exit %d, want %d; stderr: %s", res.code, forgejoprov.ExitOK, res.stderr)
	}
	for _, repo := range readRepos {
		want := secretOwner + "/" + repo + ": HTTP 200\n"
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("stdout lacks the status line %q:\n%s", want, res.stdout)
		}
	}
	if !strings.HasSuffix(res.stdout, "RESULT: stored SOURCES_TOKEN on sloan/gastown\n") {
		t.Fatalf("stdout does not end with the RESULT line:\n%s", res.stdout)
	}
	if got := f.Token(viewerAccount, testName); got == "" {
		t.Fatal("the fake minted no token under the run's name")
	}
	mints := 0
	for _, c := range f.Writes() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/admin/users/"+viewerAccount+"/tokens") {
			mints++
		}
	}
	if mints != 1 {
		t.Fatalf("the run minted %d token(s), want exactly one", mints)
	}
	if scopes, ok := f.TokenScopes(viewerAccount, testName); !ok || !slices.Equal(scopes, []string{tokenScope}) {
		t.Fatalf("the minted token holds scopes %v (present %v), want exactly [%s]", scopes, ok, tokenScope)
	}
	if !slices.Equal(res.net.reads, []string{"sloan/beads", "sloan/om", "sloan/hm", "sloan/devops"}) {
		t.Fatalf("the reads made with the minted token were %v, want the four repositories in order", res.net.reads)
	}
	stored, ok := f.Secret(secretOwner+"/"+secretRepo, secretName)
	if !ok || stored != f.Token(viewerAccount, testName) {
		t.Fatal("the fake does not hold the minted token as SOURCES_TOKEN on sloan/gastown")
	}
	if n := len(f.Violations()); n != 0 {
		t.Fatalf("the run made %d request(s) the fake refused, want none", n)
	}
}

// TestSourcesSecretRefusesAndRevokesOnAnUnreadableRepo covers each refusal: a repository
// answering 404 or 403, and a store that fails after the mint. Each one revokes the token
// it minted, stores nothing, names what the operator must do, and exits 1.
func TestSourcesSecretRefusesAndRevokesOnAnUnreadableRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		statuses  map[string]int
		storeFail int
		wantErr   []string
	}{
		{
			name:     "a repository answers 404",
			statuses: map[string]int{"sloan/om": http.StatusNotFound},
			wantErr:  []string{"sloan/om is not readable", "add bot-viewer as a read collaborator on sloan/om", "revoked the minted token " + testName},
		},
		{
			name:     "a repository answers 403",
			statuses: map[string]int{"sloan/hm": http.StatusForbidden},
			wantErr:  []string{"sloan/hm is not readable", "add bot-viewer as a read collaborator on sloan/hm", "revoked the minted token " + testName},
		},
		{
			name:      "the store fails after the mint",
			storeFail: http.StatusInternalServerError,
			wantErr:   []string{"could not store SOURCES_TOKEN on sloan/gastown", "revoked the minted token " + testName},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSourcesFake(t, "")
			f.SecretRefusal = tc.storeFail
			res := runCommand(t, f, tc.statuses)

			if res.code != forgejoprov.ExitFailure {
				t.Fatalf("exit %d, want %d; stdout: %s stderr: %s", res.code, forgejoprov.ExitFailure, res.stdout, res.stderr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(res.stderr, want) {
					t.Fatalf("stderr lacks %q:\n%s", want, res.stderr)
				}
			}
			if _, ok := f.TokenScopes(viewerAccount, testName); ok {
				t.Fatal("the minted token was not revoked")
			}
			if _, ok := f.Secret(secretOwner+"/"+secretRepo, secretName); ok {
				t.Fatal("a secret was stored on a refused run")
			}
			if strings.Contains(res.stdout, "RESULT") {
				t.Fatalf("a refused run printed the RESULT line:\n%s", res.stdout)
			}
		})
	}
}

// TestSourcesSecretRevokesAfterInterrupt covers a run cancelled after the mint and a read
// that fails in transport. The fake refuses a request whose context is done, so a revoke
// made on the run's cancelled context never reaches it: the token is gone only when the
// revoke runs on a context detached from the run's. Either way nothing is stored and the
// run exits 1.
func TestSourcesSecretRevokesAfterInterrupt(t *testing.T) {
	t.Parallel()
	t.Run("the run is cancelled after the mint", func(t *testing.T) {
		t.Parallel()
		f := newSourcesFake(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		res := runNetwork(t, ctx, &viewerNetwork{fake: f, cancelOnMint: cancel})

		if res.code != forgejoprov.ExitFailure {
			t.Fatalf("exit %d, want %d; stdout: %s stderr: %s", res.code, forgejoprov.ExitFailure, res.stdout, res.stderr)
		}
		if !strings.Contains(res.stderr, "interrupted; nothing is stored") {
			t.Fatalf("stderr does not report the interrupt:\n%s", res.stderr)
		}
		if _, ok := f.TokenScopes(viewerAccount, testName); ok {
			t.Fatal("the minted token was left live after the interrupt")
		}
		if _, ok := f.Secret(secretOwner+"/"+secretRepo, secretName); ok {
			t.Fatal("a secret was stored after the interrupt")
		}
		if strings.Contains(res.stdout, "RESULT") {
			t.Fatalf("an interrupted run printed the RESULT line:\n%s", res.stdout)
		}
	})
	t.Run("a read fails in transport", func(t *testing.T) {
		t.Parallel()
		f := newSourcesFake(t, "")
		res := runNetwork(t, context.Background(), &viewerNetwork{fake: f, failReads: true})

		if res.code != forgejoprov.ExitFailure {
			t.Fatalf("exit %d, want %d; stdout: %s stderr: %s", res.code, forgejoprov.ExitFailure, res.stdout, res.stderr)
		}
		for _, repo := range readRepos {
			want := secretOwner + "/" + repo + ": request failed (no HTTP status)"
			if !strings.Contains(res.stdout, want) {
				t.Fatalf("stdout lacks %q:\n%s", want, res.stdout)
			}
		}
		if !strings.Contains(res.stderr, "sloan/beads was not read (no HTTP reply, a transport failure)") {
			t.Fatalf("stderr does not name the transport failure:\n%s", res.stderr)
		}
		if out := res.stdout + res.stderr; strings.Contains(out, "HTTP 0") {
			t.Fatalf("the output reports a transport failure as HTTP 0:\n%s", out)
		}
		if _, ok := f.TokenScopes(viewerAccount, testName); ok {
			t.Fatal("the minted token was not revoked after the transport failure")
		}
		if _, ok := f.Secret(secretOwner+"/"+secretRepo, secretName); ok {
			t.Fatal("a secret was stored after a transport failure")
		}
	})
}

// TestSourcesSecretRefusesAnAdminFileWithNoToken checks an admin token file that holds no
// token stops the run before any request, with the message naming the file.
func TestSourcesSecretRefusesAnAdminFileWithNoToken(t *testing.T) {
	t.Parallel()
	f := newSourcesFake(t, "")
	var stdout, stderr bytes.Buffer
	readFile := func(string) ([]byte, error) { return []byte("# no token here\n"), nil }
	code := run(context.Background(), []string{"--admin-token-file", adminFile}, env{
		Now:      testClock,
		ReadFile: readFile,
		HTTP:     &http.Client{Transport: f},
	}, &stdout, &stderr)

	if code != forgejoprov.ExitFailure {
		t.Fatalf("exit %d, want %d; stderr: %s", code, forgejoprov.ExitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no admin token in "+adminFile) {
		t.Fatalf("stderr does not name the file:\n%s", stderr.String())
	}
	if n := len(f.CallLog()); n != 0 {
		t.Fatalf("a run with no admin token made %d request(s)", n)
	}
}

// TestSourcesSecretDryRunMintsNothing runs with --dry-run. No request reaches the fake
// or the repository reads, the plan names the account, scope, repositories and target,
// and the exit is 0.
func TestSourcesSecretDryRunMintsNothing(t *testing.T) {
	t.Parallel()
	f := newSourcesFake(t, "")
	res := runCommand(t, f, nil, "--dry-run")

	if res.code != forgejoprov.ExitOK {
		t.Fatalf("exit %d, want %d; stderr: %s", res.code, forgejoprov.ExitOK, res.stderr)
	}
	if n := len(f.CallLog()); n != 0 {
		t.Fatalf("the dry run made %d request(s), want none", n)
	}
	if len(res.net.reads) != 0 {
		t.Fatalf("the dry run read %d repositor(ies), want none", len(res.net.reads))
	}
	for _, want := range []string{
		"account: " + viewerAccount,
		"scope: " + tokenScope,
		"token name: " + testName,
		"read check: sloan/beads",
		"read check: sloan/om",
		"read check: sloan/hm",
		"read check: sloan/devops",
		"target: sloan/gastown secret SOURCES_TOKEN",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("the plan lacks %q:\n%s", want, res.stdout)
		}
	}
}

// TestSourcesSecretNeverPrintsTheToken runs the success path and each refusal with a
// minted value that holds a quote, a backslash and non-ASCII runes. The fake's store
// refusal echoes that value back in its body; the command never reads a store reply, so
// the echo can reach the output only if the command starts reading it. No output line
// carries the value or its JSON-escaped form, while the repository names, statuses and
// target still print.
func TestSourcesSecretNeverPrintsTheToken(t *testing.T) {
	t.Parallel()
	escaped, err := json.Marshal(leakyValue)
	if err != nil {
		t.Fatalf("encode the fixture value: %v", err)
	}
	forms := []string{leakyValue, string(escaped[1 : len(escaped)-1])}
	cases := []struct {
		name      string
		statuses  map[string]int
		storeFail int
		wantOut   string
	}{
		{name: "success", wantOut: "RESULT: stored SOURCES_TOKEN on sloan/gastown"},
		{name: "404 refusal", statuses: map[string]int{"sloan/om": http.StatusNotFound}, wantOut: "sloan/om is not readable"},
		{name: "403 refusal", statuses: map[string]int{"sloan/devops": http.StatusForbidden}, wantOut: "sloan/devops is not readable"},
		{name: "store refusal", storeFail: http.StatusInternalServerError, wantOut: "could not store SOURCES_TOKEN on sloan/gastown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSourcesFake(t, leakyValue)
			f.SecretRefusal = tc.storeFail
			res := runCommand(t, f, tc.statuses)

			output := res.stdout + res.stderr
			for _, form := range forms {
				if strings.Contains(output, form) {
					t.Fatalf("the output carries the token value or its escaped form:\n%s", output)
				}
			}
			for _, want := range []string{tc.wantOut, "sloan/gastown", "sloan/beads: HTTP "} {
				if !strings.Contains(output, want) {
					t.Fatalf("the output lacks %q:\n%s", want, output)
				}
			}
		})
	}
}

// TestSourcesSecretStoresTheLeakyValueUnchanged checks the value the run stores is the
// one the fake minted, byte for byte, so the leak test above compares a real value.
func TestSourcesSecretStoresTheLeakyValueUnchanged(t *testing.T) {
	t.Parallel()
	f := newSourcesFake(t, leakyValue)
	res := runCommand(t, f, nil)
	if res.code != forgejoprov.ExitOK {
		t.Fatalf("exit %d, want %d; stderr: %s", res.code, forgejoprov.ExitOK, res.stderr)
	}
	if got, ok := f.Secret(secretOwner+"/"+secretRepo, secretName); !ok || got != leakyValue {
		t.Fatal("the stored secret is not the minted value that holds the quote, backslash and runes")
	}
}

// TestSourcesSecretRefusesBadCommandLines checks a command line that names no admin
// token file, or passes an argument the command does not take, exits 2 and mints nothing.
func TestSourcesSecretRefusesBadCommandLines(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"no admin token file": {"--api-url", forgejotest.APIRoot + forgejotest.APIPath},
		"a positional":        {"--admin-token-file", adminFile, "extra"},
		"an unknown flag":     {"--admin-token-file", adminFile, "--nope"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSourcesFake(t, "")
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, env{Now: testClock, ReadFile: adminReader(t), HTTP: &http.Client{Transport: f}}, &stdout, &stderr)
			if code != forgejoprov.ExitUsage {
				t.Fatalf("exit %d, want %d; stderr: %s", code, forgejoprov.ExitUsage, stderr.String())
			}
			if n := len(f.CallLog()); n != 0 {
				t.Fatalf("a refused command line made %d request(s)", n)
			}
		})
	}
}

// TestSourcesSecretTokenNameIsUTC pins the name the run mints: the UTC time, to the
// second, after the sources-token- prefix.
func TestSourcesSecretTokenNameIsUTC(t *testing.T) {
	t.Parallel()
	local := time.Date(2026, 11, 6, 22, 35, 0, 0, time.FixedZone("UTC+2", 2*3600))
	if got := tokenName(local); got != testName {
		t.Fatalf("tokenName = %q, want %q", got, testName)
	}
}
