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
	// failRevoke makes every DELETE fail in transport, so the revoke never reaches the
	// server and the minted token stays live.
	failRevoke bool
	// mintReplyLost makes the mint reach the server, which creates the token, and then
	// fails the call in transport, so no reply comes back.
	mintReplyLost bool
	// mintNotSent makes the mint fail in transport before it reaches the server.
	mintNotSent bool
}

// errNoReply is the transport failure a read, a mint or a revoke made with a fault meets.
var errNoReply = errors.New("no route to host")

// mintPath is the path the run mints its token on, for the viewer account.
const mintPath = forgejotest.APIPath + "/admin/users/" + viewerAccount + "/tokens"

func (v *viewerNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	// A request whose context is done never reaches the server, as on a real transport.
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	auth := req.Header.Get("Authorization")
	const repoRoot = forgejotest.APIPath + "/repos/"
	isMint := req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, mintPath)
	if isMint && v.mintNotSent {
		return nil, errNoReply
	}
	if isMint && v.mintReplyLost {
		if resp, err := v.fake.RoundTrip(req); err == nil {
			resp.Body.Close()
		}
		return nil, errNoReply
	}
	if v.failRevoke && req.Method == http.MethodDelete {
		return nil, errNoReply
	}
	if v.cancelOnMint != nil && isMint {
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

// TestSourcesSecretRevokesAfterInterrupt covers a run canceled after the mint and a read
// that fails in transport. viewerNetwork refuses a request whose context is done, so a
// revoke made on the run's canceled context never reaches the fake: the token is gone only
// when the revoke runs on a context detached from the run's. Either way nothing is stored
// and the run exits 1.
func TestSourcesSecretRevokesAfterInterrupt(t *testing.T) {
	t.Parallel()
	t.Run("the run is canceled after the mint", func(t *testing.T) {
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

// TestSourcesSecretAlarmsWhenTheRevokeFails runs each path that revokes the minted token
// with the revoke failing in transport, so the token stays live. Each run exits 1, its
// last stderr line is the fixed still-live line naming the token and the account, the
// token value appears nowhere in the output, and the fake still holds the token.
func TestSourcesSecretAlarmsWhenTheRevokeFails(t *testing.T) {
	t.Parallel()
	const alarm = "token still live: revoke token " + testName + " for bot-viewer by hand"
	cases := []struct {
		name      string
		statuses  map[string]int
		storeFail int
		cancel    bool
		mintLost  bool
	}{
		{name: "after a refusal", statuses: map[string]int{"sloan/om": http.StatusNotFound}},
		{name: "after a failed store", storeFail: http.StatusInternalServerError},
		{name: "after an interrupt", cancel: true},
		{name: "after a mint lost in transport", mintLost: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSourcesFake(t, "")
			f.SecretRefusal = tc.storeFail
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			net := &viewerNetwork{fake: f, statuses: tc.statuses, failRevoke: true, mintReplyLost: tc.mintLost}
			if tc.cancel {
				net.cancelOnMint = cancel
			}
			res := runNetwork(t, ctx, net)

			if res.code != forgejoprov.ExitFailure {
				t.Fatalf("exit %d, want %d; stdout: %s stderr: %s", res.code, forgejoprov.ExitFailure, res.stdout, res.stderr)
			}
			lines := strings.Split(strings.TrimSuffix(res.stderr, "\n"), "\n")
			if last := lines[len(lines)-1]; last != alarm {
				t.Fatalf("the last stderr line is %q, want the still-live line %q:\n%s", last, alarm, res.stderr)
			}
			if out := res.stdout + res.stderr; strings.Contains(out, f.Token(viewerAccount, testName)) {
				t.Fatalf("the output carries the token value:\n%s", out)
			}
			if _, ok := f.TokenScopes(viewerAccount, testName); !ok {
				t.Fatal("the fake no longer holds the token, but the revoke was made to fail")
			}
		})
	}
}

// TestSourcesSecretRevokesAMintLostInTransport covers a mint whose call got no reply. When
// the server made the token, the run revokes it by the name it minted under. When the
// request never reached the server, the revoke finds no token, which is not a live token,
// so there is no alarm. Either way nothing is stored and the run exits 1.
func TestSourcesSecretRevokesAMintLostInTransport(t *testing.T) {
	t.Parallel()
	t.Run("the server made the token before the reply was lost", func(t *testing.T) {
		t.Parallel()
		f := newSourcesFake(t, "")
		res := runNetwork(t, context.Background(), &viewerNetwork{fake: f, mintReplyLost: true})

		if res.code != forgejoprov.ExitFailure {
			t.Fatalf("exit %d, want %d; stdout: %s stderr: %s", res.code, forgejoprov.ExitFailure, res.stdout, res.stderr)
		}
		if !strings.Contains(res.stderr, "revoked the minted token "+testName) {
			t.Fatalf("stderr does not report the revoke of the lost mint:\n%s", res.stderr)
		}
		if strings.Contains(res.stderr, "token still live") {
			t.Fatalf("stderr alarms about a token that was revoked:\n%s", res.stderr)
		}
		if _, ok := f.TokenScopes(viewerAccount, testName); ok {
			t.Fatal("the token the server made after a lost reply was left live")
		}
		if _, ok := f.Secret(secretOwner+"/"+secretRepo, secretName); ok {
			t.Fatal("a secret was stored after a lost mint")
		}
	})
	t.Run("the request never reached the server", func(t *testing.T) {
		t.Parallel()
		f := newSourcesFake(t, "")
		res := runNetwork(t, context.Background(), &viewerNetwork{fake: f, mintNotSent: true})

		if res.code != forgejoprov.ExitFailure {
			t.Fatalf("exit %d, want %d; stdout: %s stderr: %s", res.code, forgejoprov.ExitFailure, res.stdout, res.stderr)
		}
		if !strings.Contains(res.stderr, "no token "+testName+" is on the server, so none is live") {
			t.Fatalf("stderr does not report that no token was made:\n%s", res.stderr)
		}
		if strings.Contains(res.stderr, "token still live") {
			t.Fatalf("stderr alarms when no token was made:\n%s", res.stderr)
		}
		if _, ok := f.Secret(secretOwner+"/"+secretRepo, secretName); ok {
			t.Fatal("a secret was stored after a lost mint")
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
		"check with dry run":  {"--admin-token-file", adminFile, "--check", "--dry-run"},
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

// checkNetwork is the network a --check run meets. Every request reaches the fake, except
// that the reply to the one GET whose path ends in failSuffix is replaced by failStatus and
// failBody. It records the method of every request, so a test can see a write that was sent.
type checkNetwork struct {
	fake       *forgejotest.Fake
	methods    []string
	failSuffix string
	failStatus int
	failBody   string
}

func (c *checkNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	c.methods = append(c.methods, req.Method)
	if c.failSuffix != "" && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, c.failSuffix) {
		return &http.Response{
			StatusCode: c.failStatus,
			Status:     http.StatusText(c.failStatus),
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(c.failBody)),
			Request:    req,
		}, nil
	}
	return c.fake.RoundTrip(req)
}

// checkResult is what one --check run printed and the code it exited with.
type checkResult struct {
	code   int
	stdout string
	stderr string
}

// runCheckNetwork runs the command with --check against net, with extra arguments after it.
func runCheckNetwork(t *testing.T, net http.RoundTripper, extra ...string) checkResult {
	t.Helper()
	args := append([]string{"--api-url", forgejotest.APIRoot + forgejotest.APIPath, "--admin-token-file", adminFile, "--check"}, extra...)
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), args, env{Now: testClock, ReadFile: adminReader(t), HTTP: &http.Client{Transport: net}}, &stdout, &stderr)
	return checkResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// newCheckFake returns the fake a check reads: the viewer account, the four read repositories
// and the target, with bot-viewer a collaborator on every read repository except ungranted.
func newCheckFake(t *testing.T, ungranted string) *forgejotest.Fake {
	t.Helper()
	f := forgejotest.New()
	f.ViewerExists = true
	for _, repo := range readRepos {
		f.AddRepo(secretOwner + "/" + repo)
		if repo != ungranted {
			grant(t, f, secretOwner+"/"+repo)
		}
	}
	f.AddRepo(secretOwner + "/" + secretRepo)
	return f
}

// mintTokenNamed gives bot-viewer a token of that name on the fake, as an earlier run would
// have left it.
func mintTokenNamed(t *testing.T, f *forgejotest.Fake, name string) {
	t.Helper()
	status, body := f.Call(t, http.MethodPost, "/admin/users/"+viewerAccount+"/tokens",
		forgejotest.JSONBody(map[string]any{"name": name, "scopes": []string{tokenScope}}))
	if status != http.StatusCreated {
		t.Fatalf("mint %s: HTTP %d %s", name, status, body)
	}
}

// TestSourcesSecretCheckReportsTheState checks a --check run against a fake holding the
// secret, the four read grants and two sources-token- tokens. It prints the secret as listed,
// each repository as readable, and the two token names, exits 0, and sends only GETs: it
// mints, revokes and stores nothing.
func TestSourcesSecretCheckReportsTheState(t *testing.T) {
	t.Parallel()
	f := newCheckFake(t, "")
	f.Call(t, http.MethodPut, "/repos/"+secretOwner+"/"+secretRepo+"/actions/secrets/"+secretName,
		forgejotest.JSONBody(map[string]string{"data": "stored-value-0a1b"}))
	mintTokenNamed(t, f, testName)
	mintTokenNamed(t, f, "sources-token-20261106-210000")
	mintTokenNamed(t, f, "manual-token")
	valueBefore := f.Token(viewerAccount, testName)
	f.ClearCalls()
	net := &checkNetwork{fake: f}

	res := runCheckNetwork(t, net)

	if res.code != forgejoprov.ExitOK {
		t.Fatalf("exit %d, want %d; stderr: %s", res.code, forgejoprov.ExitOK, res.stderr)
	}
	for _, want := range []string{
		"sloan/gastown secret SOURCES_TOKEN: listed\n",
		"sloan/beads: readable by bot-viewer (HTTP 204)\n",
		"sloan/om: readable by bot-viewer (HTTP 204)\n",
		"sloan/hm: readable by bot-viewer (HTTP 204)\n",
		"sloan/devops: readable by bot-viewer (HTTP 204)\n",
		"bot-viewer token: " + testName + "\n",
		"bot-viewer token: sources-token-20261106-210000\n",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "manual-token") {
		t.Fatalf("stdout names a token outside the sources-token- prefix:\n%s", res.stdout)
	}
	for _, m := range net.methods {
		if m != http.MethodGet {
			t.Fatalf("the check sent a %s, want GET only; methods: %v", m, net.methods)
		}
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("the fake saw %d write(s), want none", n)
	}
	if got := f.Token(viewerAccount, testName); got != valueBefore {
		t.Fatal("the check changed the token it read")
	}
	if n := len(f.Violations()); n != 0 {
		t.Fatalf("the check made %d request(s) the fake refused, want none", n)
	}
}

// TestSourcesSecretCheckReportsWhatIsMissing checks a --check run where the secret is not
// listed, bot-viewer is not a collaborator on sloan/om, and the token list fails. It says so
// for each, names the repository and the failed list, and exits 1.
func TestSourcesSecretCheckReportsWhatIsMissing(t *testing.T) {
	t.Parallel()
	f := newCheckFake(t, "om")
	f.ClearCalls()
	net := &checkNetwork{
		fake:       f,
		failSuffix: "/admin/users/" + viewerAccount + "/tokens",
		failStatus: http.StatusInternalServerError,
		failBody:   `{"message":"internal error"}`,
	}

	res := runCheckNetwork(t, net)

	if res.code != forgejoprov.ExitFailure {
		t.Fatalf("exit %d, want %d; stdout: %s", res.code, forgejoprov.ExitFailure, res.stdout)
	}
	if !strings.Contains(res.stdout, "sloan/gastown secret SOURCES_TOKEN: not listed\n") {
		t.Fatalf("stdout does not say the secret is not listed:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "sloan/om: not readable by bot-viewer (HTTP 404)\n") {
		t.Fatalf("stdout does not name sloan/om as unreadable:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "sloan/beads: readable by bot-viewer (HTTP 204)\n") {
		t.Fatalf("stdout does not report the readable repositories:\n%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "could not list the tokens of bot-viewer") || !strings.Contains(res.stderr, "HTTP 500") {
		t.Fatalf("stderr does not say the token list failed with HTTP 500:\n%s", res.stderr)
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("the fake saw %d write(s), want none", n)
	}
}

// TestSourcesSecretCheckNeverPrintsAReply checks no output line carries a reply body. A
// faulted reply carries the escaped leaky value and a secret-like value; a success reply
// carries the stored secret value. Neither the raw value nor its escaped form appears in
// stdout or stderr, while the repository names and statuses still print.
func TestSourcesSecretCheckNeverPrintsAReply(t *testing.T) {
	t.Parallel()
	escaped, err := json.Marshal(leakyValue)
	if err != nil {
		t.Fatalf("encode the fixture value: %v", err)
	}
	escapedInner := string(escaped[1 : len(escaped)-1])
	const sha1Like = "0123456789abcdef0123456789abcdef01234567"
	faultBody := `{"message":"` + escapedInner + `","sha1":"` + sha1Like + `"}`
	forms := []string{leakyValue, escapedInner, sha1Like}

	cases := []struct {
		name       string
		failSuffix string
		failStatus int
		wantCode   int
	}{
		{name: "success", wantCode: forgejoprov.ExitOK},
		{name: "secret list fails", failSuffix: "/sloan/gastown/actions/secrets", failStatus: http.StatusInternalServerError, wantCode: forgejoprov.ExitFailure},
		{name: "collaborator check fails", failSuffix: "/sloan/om/collaborators/" + viewerAccount, failStatus: http.StatusInternalServerError, wantCode: forgejoprov.ExitFailure},
		{name: "token list fails", failSuffix: "/admin/users/" + viewerAccount + "/tokens", failStatus: http.StatusInternalServerError, wantCode: forgejoprov.ExitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCheckFake(t, "")
			f.Call(t, http.MethodPut, "/repos/"+secretOwner+"/"+secretRepo+"/actions/secrets/"+secretName,
				forgejotest.JSONBody(map[string]string{"data": leakyValue}))
			mintTokenNamed(t, f, testName)
			net := &checkNetwork{fake: f, failSuffix: tc.failSuffix, failStatus: tc.failStatus, failBody: faultBody}

			res := runCheckNetwork(t, net)

			if res.code != tc.wantCode {
				t.Fatalf("exit %d, want %d; stderr: %s", res.code, tc.wantCode, res.stderr)
			}
			output := res.stdout + res.stderr
			for _, form := range forms {
				if strings.Contains(output, form) {
					t.Fatalf("the output carries a reply body value %q:\n%s", form, output)
				}
			}
			if !strings.Contains(res.stdout, "sloan/beads: ") {
				t.Fatalf("stdout lacks the repository line for sloan/beads:\n%s", res.stdout)
			}
		})
	}
}

// TestSourcesSecretCheckPrintsOnlyMintedNames checks a token whose name is not the shape this
// command mints is counted and not printed: its name, which the server sent, reaches no output.
func TestSourcesSecretCheckPrintsOnlyMintedNames(t *testing.T) {
	t.Parallel()
	f := newCheckFake(t, "")
	mintTokenNamed(t, f, testName)
	mintTokenNamed(t, f, "sources-token-by-hand")
	f.ClearCalls()

	res := runCheckNetwork(t, &checkNetwork{fake: f})

	if res.code != forgejoprov.ExitOK {
		t.Fatalf("exit %d, want %d; stderr: %s", res.code, forgejoprov.ExitOK, res.stderr)
	}
	if strings.Contains(res.stdout, "by-hand") {
		t.Fatalf("stdout prints a token name outside the minted shape:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "bot-viewer token: "+testName+"\n") {
		t.Fatalf("stdout lacks the minted-shape token name:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "1 more sources-token- token(s) do not have a name this command mints") {
		t.Fatalf("stdout does not count the other token:\n%s", res.stdout)
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
