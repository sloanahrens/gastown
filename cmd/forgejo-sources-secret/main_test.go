package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
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

// mangoRepo is the repository the image build fetches that no pin or seed file names: the
// MANGO_URL repository of deploy/ci/images.workflow.yml.
const mangoRepo = "mango"

// testOwner is the neutral Forgejo owner a parser fixture names, the same neutral owner the
// other packages' fixtures use. The command's own tests take the owner from production
// secretOwner instead: the run builds every path it prints and requests from that constant.
const testOwner = "owner"

// sourceRepoFiles are the files that name the repositories the image build fetches, relative
// to this package: the pins the daemon image is built from, and the rigs the polecat seed
// fetches.
var sourceRepoFiles = []string{"../../deploy/daemon/image-pins", "../../deploy/polecat/seed-urls"}

// sourceRepos returns the repository names one source file names: each non-comment line
// carries a URL as its second field, and the name is the last path element without .git. A
// line with no URL, a URL that does not parse, or a URL that is not under owner or has no
// .git suffix is an error naming the file, since the file no longer says what the check reads.
func sourceRepos(file, data, owner string) ([]string, error) {
	var names []string
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s: line %q names no URL", file, line)
		}
		u, err := url.Parse(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%s: URL %q: %v", file, fields[1], err)
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) != 2 || parts[0] != owner || !strings.HasSuffix(parts[1], ".git") {
			return nil, fmt.Errorf("%s: URL %q is not %s/<name>.git", file, fields[1], owner)
		}
		names = append(names, strings.TrimSuffix(parts[1], ".git"))
	}
	return names, nil
}

// wantRepos returns the sorted, de-duplicated repository names the source files name, with
// mangoRepo added. read returns the text of one source file.
func wantRepos(read func(string) (string, error)) ([]string, error) {
	names := []string{mangoRepo}
	for _, file := range sourceRepoFiles {
		data, err := read(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		fileNames, err := sourceRepos(file, data, secretOwner)
		if err != nil {
			return nil, err
		}
		names = append(names, fileNames...)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// diffRepos compares the names the run reads (got) with the names the sources name (want).
// missing is a name a source names that the run does not read; stray is a name the run
// reads that no source names.
func diffRepos(got, want []string) (missing, stray []string) {
	for _, name := range want {
		if !slices.Contains(got, name) {
			missing = append(missing, name)
		}
	}
	for _, name := range got {
		if !slices.Contains(want, name) {
			stray = append(stray, name)
		}
	}
	return missing, stray
}

// readSourceFile reads one source file for wantRepos, relative to this package.
func readSourceFile(file string) (string, error) {
	data, err := os.ReadFile(file)
	return string(data), err
}

// TestSourcesSecretChecksExactlyTheSourceRepos holds the repositories the run reads to the names
// the image build fetches: every URL name in the pin and seed files, plus mango. A repository the
// build fetches that the run does not read, or a name the run reads that no file names, fails it.
func TestSourcesSecretChecksExactlyTheSourceRepos(t *testing.T) {
	t.Parallel()
	want, err := wantRepos(readSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	missing, stray := diffRepos(readRepos, want)
	if len(missing) > 0 || len(stray) > 0 {
		t.Fatalf("readRepos misses %v that the source files name, and names %v that they do not", missing, stray)
	}
}

// TestSourceReposReadsTheNameOfEachLine holds the parser to the shape the pin and seed files
// use: comments and blank lines are skipped, and each URL's last element without .git is a name.
func TestSourceReposReadsTheNameOfEachLine(t *testing.T) {
	t.Parallel()
	data := "# a comment\n\nbd http://forgejo:3000/" + testOwner + "/beads.git abc\nom http://forgejo:3000/" + testOwner + "/organic-mechanic.git def\n"
	got, err := sourceRepos("pins", data, testOwner)
	if err != nil {
		t.Fatalf("sourceRepos: %v", err)
	}
	if want := []string{"beads", "organic-mechanic"}; !slices.Equal(got, want) {
		t.Fatalf("sourceRepos = %v, want %v", got, want)
	}
}

// TestSourceReposRefusesABadLine drives each refusal of the parser: every bad line is an error
// that names the file, not a panic and not a name taken from the bad line.
func TestSourceReposRefusesABadLine(t *testing.T) {
	t.Parallel()
	const file = "pins"
	tests := []struct {
		name string
		data string
	}{
		{"a line with no URL", "beads\n"},
		{"a URL that does not parse", "bd http://[bad abc\n"},
		{"a URL not under the owner", "bd http://forgejo:3000/other/beads.git abc\n"},
		{"a URL without the .git suffix", "bd http://forgejo:3000/" + testOwner + "/beads abc\n"},
		{"a URL with an extra path element", "bd http://forgejo:3000/" + testOwner + "/x/beads.git abc\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			names, err := sourceRepos(file, tc.data, testOwner)
			if err == nil {
				t.Fatalf("sourceRepos(%q) = %v, want an error", tc.data, names)
			}
			if !strings.Contains(err.Error(), file) {
				t.Fatalf("error %q does not name the file %s", err, file)
			}
		})
	}
}

// TestDiffReposNamesADroppedAndAStrayRepo drives both halves of the comparison: a name the
// sources name that the run does not read is missing, and a name the run reads that no source
// names is stray.
func TestDiffReposNamesADroppedAndAStrayRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		got, want []string
		missing   []string
		stray     []string
	}{
		{"the same names differ in nothing", []string{"beads", "devops"}, []string{"devops", "beads"}, nil, nil},
		{"a repo the build fetches is dropped", []string{"beads"}, []string{"beads", "mango"}, []string{"mango"}, nil},
		{"a name no source names is stray", []string{"beads", "foo"}, []string{"beads"}, nil, []string{"foo"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			missing, stray := diffRepos(tc.got, tc.want)
			if !slices.Equal(missing, tc.missing) || !slices.Equal(stray, tc.stray) {
				t.Fatalf("diffRepos(%v, %v) = missing %v, stray %v; want missing %v, stray %v",
					tc.got, tc.want, missing, stray, tc.missing, tc.stray)
			}
		})
	}
}

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

// newSourcesFake returns a fake holding the viewer account, the repositories the
// run reads and the target repository, with bot-viewer a collaborator on each read one.
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

// TestSourcesSecretStoresAfterAllOKReads runs the command where bot-viewer reads every
// read repository. The command mints one token, reads them all with it, stores it on
// the secret owner's gastown repository and prints one status line each and the RESULT
// line, exiting 0.
func TestSourcesSecretStoresAfterAllOKReads(t *testing.T) {
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
	if !strings.HasSuffix(res.stdout, "RESULT: stored SOURCES_TOKEN on "+secretOwner+"/gastown\n") {
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
	if !slices.Equal(res.net.reads, []string{secretOwner + "/beads", secretOwner + "/organic-mechanic", secretOwner + "/history-man", secretOwner + "/devops", secretOwner + "/mango"}) {
		t.Fatalf("the reads made with the minted token were %v, want every read repository in order", res.net.reads)
	}
	stored, ok := f.Secret(secretOwner+"/"+secretRepo, secretName)
	if !ok || stored != f.Token(viewerAccount, testName) {
		t.Fatal("the fake does not hold the minted token as SOURCES_TOKEN on " + secretOwner + "/gastown")
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
			statuses: map[string]int{secretOwner + "/organic-mechanic": http.StatusNotFound},
			wantErr:  []string{secretOwner + "/organic-mechanic is not readable", "add bot-viewer as a read collaborator on " + secretOwner + "/organic-mechanic", "revoked the minted token " + testName},
		},
		{
			name:     "a repository answers 403",
			statuses: map[string]int{secretOwner + "/history-man": http.StatusForbidden},
			wantErr:  []string{secretOwner + "/history-man is not readable", "add bot-viewer as a read collaborator on " + secretOwner + "/history-man", "revoked the minted token " + testName},
		},
		{
			name:      "the store fails after the mint",
			storeFail: http.StatusInternalServerError,
			wantErr:   []string{"could not store SOURCES_TOKEN on " + secretOwner + "/gastown", "revoked the minted token " + testName},
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
		if !strings.Contains(res.stderr, secretOwner+"/beads was not read (no HTTP reply, a transport failure)") {
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
		{name: "after a refusal", statuses: map[string]int{secretOwner + "/organic-mechanic": http.StatusNotFound}},
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
		"read check: " + secretOwner + "/beads",
		"read check: " + secretOwner + "/organic-mechanic",
		"read check: " + secretOwner + "/history-man",
		"read check: " + secretOwner + "/devops",
		"target: " + secretOwner + "/gastown secret SOURCES_TOKEN",
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
		{name: "success", wantOut: "RESULT: stored SOURCES_TOKEN on " + secretOwner + "/gastown"},
		{name: "404 refusal", statuses: map[string]int{secretOwner + "/organic-mechanic": http.StatusNotFound}, wantOut: secretOwner + "/organic-mechanic is not readable"},
		{name: "403 refusal", statuses: map[string]int{secretOwner + "/devops": http.StatusForbidden}, wantOut: secretOwner + "/devops is not readable"},
		{name: "store refusal", storeFail: http.StatusInternalServerError, wantOut: "could not store SOURCES_TOKEN on " + secretOwner + "/gastown"},
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
			for _, want := range []string{tc.wantOut, secretOwner + "/gastown", secretOwner + "/beads: HTTP "} {
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
// that the one GET whose path ends in failSuffix is answered by failErr when it is set, or
// else by failStatus and failBody. It records the method of every request, so a test can
// see a write that was sent.
type checkNetwork struct {
	fake       *forgejotest.Fake
	methods    []string
	failSuffix string
	failStatus int
	failBody   string
	failErr    error
}

func (c *checkNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	c.methods = append(c.methods, req.Method)
	if c.failSuffix != "" && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, c.failSuffix) {
		if c.failErr != nil {
			return nil, c.failErr
		}
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

// newCheckFake returns the fake a check reads: the viewer account, the read repositories
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
// secret, the read grants and two sources-token- tokens. It prints the secret as listed,
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
		secretOwner + "/gastown secret SOURCES_TOKEN: listed\n",
		secretOwner + "/beads: readable by bot-viewer (HTTP 204)\n",
		secretOwner + "/organic-mechanic: readable by bot-viewer (HTTP 204)\n",
		secretOwner + "/history-man: readable by bot-viewer (HTTP 204)\n",
		secretOwner + "/devops: readable by bot-viewer (HTTP 204)\n",
		secretOwner + "/mango: readable by bot-viewer (HTTP 204)\n",
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

// storeCheckSecret stores SOURCES_TOKEN on the target repository of a check fake, as an
// earlier real run would have.
func storeCheckSecret(t *testing.T, f *forgejotest.Fake) {
	t.Helper()
	f.Call(t, http.MethodPut, "/repos/"+secretOwner+"/"+secretRepo+"/actions/secrets/"+secretName,
		forgejotest.JSONBody(map[string]string{"data": "stored-value-0a1b"}))
}

// TestSourcesSecretCheckUnreadableRepositoryExitsOne checks a required repository that answers
// 404 for bot-viewer makes the check name it as not readable and exit 1, with the secret listed
// and the token list fine. The same check with every read repository readable exits 0.
func TestSourcesSecretCheckUnreadableRepositoryExitsOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		ungranted string
		wantCode  int
	}{
		{name: "one repository answers 404", ungranted: "organic-mechanic", wantCode: forgejoprov.ExitFailure},
		{name: "all read repositories answer 204", ungranted: "", wantCode: forgejoprov.ExitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCheckFake(t, tc.ungranted)
			storeCheckSecret(t, f)
			mintTokenNamed(t, f, testName)
			f.ClearCalls()

			res := runCheckNetwork(t, &checkNetwork{fake: f})

			if res.code != tc.wantCode {
				t.Fatalf("exit %d, want %d; stderr: %s", res.code, tc.wantCode, res.stderr)
			}
			if tc.ungranted == "" {
				return
			}
			if !strings.Contains(res.stdout, secretOwner+"/organic-mechanic: not readable by bot-viewer (HTTP 404)\n") {
				t.Fatalf("stdout does not name organic-mechanic as not readable:\n%s", res.stdout)
			}
			if !strings.Contains(res.stderr, secretOwner+"/organic-mechanic is not readable by bot-viewer") {
				t.Fatalf("stderr does not name organic-mechanic as not readable:\n%s", res.stderr)
			}
			if !strings.Contains(res.stdout, secretOwner+"/beads: readable by bot-viewer (HTTP 204)\n") {
				t.Fatalf("stdout does not report the readable repositories:\n%s", res.stdout)
			}
			if n := len(f.Writes()); n != 0 {
				t.Fatalf("the fake saw %d write(s), want none", n)
			}
		})
	}
}

// TestSourcesSecretCheckUnlistedSecretIsReportedAndExitsZero checks that with the secret absent
// and every other read fine, the check prints that SOURCES_TOKEN is not listed and exits 0. That
// is the state before the first real run, so it is reported, not failed.
func TestSourcesSecretCheckUnlistedSecretIsReportedAndExitsZero(t *testing.T) {
	t.Parallel()
	f := newCheckFake(t, "")
	mintTokenNamed(t, f, testName)
	f.ClearCalls()

	res := runCheckNetwork(t, &checkNetwork{fake: f})

	if res.code != forgejoprov.ExitOK {
		t.Fatalf("exit %d, want %d; stderr: %s", res.code, forgejoprov.ExitOK, res.stderr)
	}
	if !strings.Contains(res.stdout, secretOwner+"/gastown secret SOURCES_TOKEN: not listed\n") {
		t.Fatalf("stdout does not say the secret is not listed:\n%s", res.stdout)
	}
	if res.stderr != "" {
		t.Fatalf("stderr is not empty for an unlisted secret:\n%s", res.stderr)
	}
}

// TestSourcesSecretCheckReportsWhatIsMissing checks a failing token list alone, with the secret
// listed and the read repositories readable, is reported and exits 1. No other read fails, so
// the exit code rides on the token list alone.
func TestSourcesSecretCheckReportsWhatIsMissing(t *testing.T) {
	t.Parallel()
	f := newCheckFake(t, "")
	storeCheckSecret(t, f)
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
	if !strings.Contains(res.stdout, secretOwner+"/gastown secret SOURCES_TOKEN: listed\n") {
		t.Fatalf("stdout does not say the secret is listed:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, secretOwner+"/organic-mechanic: readable by bot-viewer (HTTP 204)\n") {
		t.Fatalf("stdout does not report the readable repositories:\n%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "could not list the tokens of bot-viewer") || !strings.Contains(res.stderr, "HTTP 500") {
		t.Fatalf("stderr does not say the token list failed with HTTP 500:\n%s", res.stderr)
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("the fake saw %d write(s), want none", n)
	}
}

// TestSourcesSecretCheckRefusesANullListReply checks a 200 reply that is JSON null, or any
// other non-array body, for the secret list or the token list is a failed read: the check
// reports it and exits 1, and never prints the malformed reply as a state (the secret as
// "not listed", or the tokens as none listed).
func TestSourcesSecretCheckRefusesANullListReply(t *testing.T) {
	t.Parallel()
	tokenPath := "/admin/users/" + viewerAccount + "/tokens"
	cases := []struct {
		name       string
		failSuffix string
		body       string
		wantErr    string
		wantAbsent string
	}{
		{
			name:       "the secret list replies null",
			failSuffix: "/" + secretOwner + "/gastown/actions/secrets",
			body:       `null`,
			wantErr:    "could not list the Actions secrets of " + secretOwner + "/gastown",
			wantAbsent: "secret SOURCES_TOKEN: not listed",
		},
		{
			name:       "the secret list replies an object",
			failSuffix: "/" + secretOwner + "/gastown/actions/secrets",
			body:       `{"data":[]}`,
			wantErr:    "could not list the Actions secrets of " + secretOwner + "/gastown",
			wantAbsent: "secret SOURCES_TOKEN: not listed",
		},
		{
			name:       "the token list replies null",
			failSuffix: tokenPath,
			body:       `null`,
			wantErr:    "could not list the tokens of bot-viewer",
			wantAbsent: "tokens listed",
		},
		{
			name:       "the token list replies an object",
			failSuffix: tokenPath,
			body:       `{"tokens":[]}`,
			wantErr:    "could not list the tokens of bot-viewer",
			wantAbsent: "tokens listed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCheckFake(t, "")
			storeCheckSecret(t, f)
			mintTokenNamed(t, f, testName)
			net := &checkNetwork{fake: f, failSuffix: tc.failSuffix, failStatus: http.StatusOK, failBody: tc.body}

			res := runCheckNetwork(t, net)

			if res.code != forgejoprov.ExitFailure {
				t.Fatalf("exit %d, want %d; stdout: %s stderr: %s", res.code, forgejoprov.ExitFailure, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.wantErr) || !strings.Contains(res.stderr, "not a JSON array") {
				t.Fatalf("stderr lacks %q and the refusal reason:\n%s", tc.wantErr, res.stderr)
			}
			if strings.Contains(res.stdout, tc.wantAbsent) {
				t.Fatalf("stdout prints a malformed reply as a state (%q):\n%s", tc.wantAbsent, res.stdout)
			}
		})
	}
}

// TestSourcesSecretCheckReportsATransportFailure checks a collaborator check that gets no
// HTTP reply at all is reported as such and exits 1, not read as a repository state.
func TestSourcesSecretCheckReportsATransportFailure(t *testing.T) {
	t.Parallel()
	f := newCheckFake(t, "")
	storeCheckSecret(t, f)
	mintTokenNamed(t, f, testName)
	net := &checkNetwork{
		fake:       f,
		failSuffix: "/" + secretOwner + "/history-man/collaborators/" + viewerAccount,
		failErr:    errNoReply,
	}

	res := runCheckNetwork(t, net)

	if res.code != forgejoprov.ExitFailure {
		t.Fatalf("exit %d, want %d; stdout: %s", res.code, forgejoprov.ExitFailure, res.stdout)
	}
	if !strings.Contains(res.stderr, "the collaborator check on "+secretOwner+"/history-man got no HTTP reply") {
		t.Fatalf("stderr does not name the transport failure:\n%s", res.stderr)
	}
	// The needle is the production owner, not a literal: secretOwner is the
	// single source for the path the command prints.
	if strings.Contains(res.stdout, secretOwner+"/history-man: ") {
		t.Fatalf("stdout reports a state for a repository whose check got no reply:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, secretOwner+"/organic-mechanic: readable by bot-viewer (HTTP 204)") {
		t.Fatalf("stdout does not report the readable repositories:\n%s", res.stdout)
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
		{name: "secret list fails", failSuffix: "/" + secretOwner + "/gastown/actions/secrets", failStatus: http.StatusInternalServerError, wantCode: forgejoprov.ExitFailure},
		{name: "collaborator check fails", failSuffix: "/" + secretOwner + "/organic-mechanic/collaborators/" + viewerAccount, failStatus: http.StatusInternalServerError, wantCode: forgejoprov.ExitFailure},
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
			if !strings.Contains(res.stdout, secretOwner+"/beads: ") {
				t.Fatalf("stdout lacks the repository line for beads:\n%s", res.stdout)
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
