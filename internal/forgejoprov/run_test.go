package forgejoprov

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"strings"
	"testing"
)

// decoyToken is an admin-looking value no run may present. The fake accepts only
// fixtureToken, so a request that carries the decoy is recorded as a violation.
const decoyToken = "decoy-admin-2b8e41"

// The paths a run's admin token file, its home directory and its config dir live at.
// A run reads and writes through the seams below, so none of them exists on disk.
const (
	fixtureAdminFile = "/fixture/admin.env"
	fixtureHomeEnv   = "/fixture/home/forgejo/.env"
	fixtureConfigDir = "/fixture/config/gt"
)

// fixtureFiles is what a run's ReadFile serves: the stand-in admin token file, and the
// file under $HOME that holds a decoy no run may use. Any other path is absent.
var fixtureFiles = map[string]string{
	fixtureAdminFile: "FORGEJO_ADMIN_TOKEN=" + fixtureToken + "\n",
	fixtureHomeEnv:   "FORGEJO_ADMIN_TOKEN=" + decoyToken + "\n",
}

// fixtureTokenFile is the token file a run writes for a role.
func fixtureTokenFile(role string) string {
	return fixtureConfigDir + "/forgejo-" + role + ".env"
}

// baseEnv is the environment a run starts from: the fake's API root, the stand-in admin
// token file, and a config dir and home under the fixture paths. A test names only what
// it changes, and an empty value is an unset variable.
func baseEnv() map[string]string {
	return map[string]string{
		"FORGEJO_API_URL":   fakeAPIRoot + fakeAPIPath,
		"FORGEJO_ADMIN_ENV": fixtureAdminFile,
		"XDG_CONFIG_HOME":   "/fixture/config",
		"HOME":              "/fixture/home",
	}
}

// runResult is what one run left behind: its exit code, the progress lines it logged,
// and the paths it asked the admin token reader for.
type runResult struct {
	code  int
	logs  string
	reads []string
}

// runProvisioning parses args with env as the environment, then runs the command through
// Run, sending its requests through rt (the fake, or a transport that fails one route) and
// writing its token files into files. No request leaves the process.
func runProvisioning(t *testing.T, rt http.RoundTripper, files *memFiles, env map[string]string, args ...string) runResult {
	t.Helper()
	return runProvisioningCtx(context.Background(), t, rt, files, env, args...)
}

// runProvisioningCtx is runProvisioning under ctx, so a test can cancel the run.
func runProvisioningCtx(ctx context.Context, t *testing.T, rt http.RoundTripper, files *memFiles, env map[string]string, args ...string) runResult {
	t.Helper()
	merged := baseEnv()
	maps.Copy(merged, env)
	getenv := func(name string) string { return merged[name] }
	opts, err := Parse(args, getenv)
	if err != nil {
		t.Fatalf("Parse(%v) = %v, want the command line accepted", args, err)
	}
	var reads []string
	readFile := func(path string) ([]byte, error) {
		reads = append(reads, path)
		if data, ok := fixtureFiles[path]; ok {
			return []byte(data), nil
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	var logs strings.Builder
	code := Run(ctx, opts, Env{
		Getenv:   getenv,
		ReadFile: readFile,
		Log:      &logs,
		HTTP:     &http.Client{Transport: rt},
		Files:    files,
		Random:   &countingRandom{},
	})
	return runResult{code: code, logs: logs.String(), reads: reads}
}

// holdDriftedMain creates the main rule on protectionRepo with a gate context that is
// not the one a run requires, so the run finds it drifted and patches it.
func holdDriftedMain(t *testing.T, f *fakeForgejo) {
	t.Helper()
	body := jsonBody(ruleBody(fixtureMainRule, nil))
	if status, _ := f.call(t, http.MethodPost, "/repos/"+protectionRepo+"/branch_protections", body); status != http.StatusCreated {
		t.Fatalf("create the drifted %s rule: status %d, want 201", fixtureMainRule, status)
	}
}

// failingRoute answers the requests whose escaped path holds match with status, and passes
// every other request on to next. It is how a test makes one read fail: the fake answers
// only the statuses the live server gives on the routes it models.
type failingRoute struct {
	next   http.RoundTripper
	match  string
	status int
}

// RoundTrip answers a matching request with the failing status and a fixed body.
func (r failingRoute) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.Contains(req.URL.EscapedPath(), r.match) {
		return r.next.RoundTrip(req)
	}
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"message":"internal failure"}`)),
		Request:    req,
	}, nil
}

// ctxRoute serves requests through next and honors the request context the way
// net/http's transport does: a request whose context is already done is refused
// before it reaches next. After it has served cancelAfter requests it calls cancel,
// which is how a test cancels the run between two requests; a nil cancel never does.
type ctxRoute struct {
	next        http.RoundTripper
	cancelAfter int
	cancel      context.CancelFunc
	served      int
}

// RoundTrip refuses a request under a done context, and otherwise serves it through next.
func (c *ctxRoute) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	c.served++
	if c.cancel != nil && c.served == c.cancelAfter {
		c.cancel()
	}
	return c.next.RoundTrip(req)
}

// namedSecret is a value a run must never print, with the name a failure message
// uses for it in place of the value.
type namedSecret struct {
	name  string
	value string
}

// leakReport says where the first secret that output holds sits: its name, the byte
// offset of its first match and the length of output. It never quotes output, since
// output is what may hold the secret and a failure message gets pasted into a bead.
// It returns "" when output holds none of the secrets.
func leakReport(output string, secrets []namedSecret) string {
	for _, s := range secrets {
		if s.value == "" {
			continue
		}
		if at := strings.Index(output, s.value); at >= 0 {
			return fmt.Sprintf("output holds the %s at byte %d of %d (output not quoted)", s.name, at, len(output))
		}
	}
	return ""
}

// nonGET returns the calls that were not reads.
func nonGET(calls []fakeCall) []fakeCall {
	var out []fakeCall
	for _, c := range calls {
		if c.method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// countCalls counts the calls with the method whose escaped path holds pathPart.
func countCalls(calls []fakeCall, method, pathPart string) int {
	n := 0
	for _, c := range calls {
		if c.method == method && strings.Contains(c.path, pathPart) {
			n++
		}
	}
	return n
}

// mintedValues returns every token value the fake minted, so a test can search output
// for any of them.
func mintedValues(f *fakeForgejo) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, u := range f.users {
		for _, tok := range u.tokens {
			out = append(out, tok.value)
		}
	}
	return out
}

// TestRunFirstThenSecondRunIsIdempotent: the first run creates the bots, their tokens and
// the protection; the second, against the same state, sends no write and rewrites no
// token file.
func TestRunFirstThenSecondRunIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	files := newMemFiles()

	first := runProvisioning(t, f, files, nil, "--repo", protectionRepo)
	if first.code != ExitOK {
		t.Fatalf("first run = %d, want %d (logs: %s)", first.code, ExitOK, first.logs)
	}
	if !strings.Contains(first.logs, "no bot-viewer user; skipping its read grant on acme/rig") {
		t.Errorf("first run logs = %q, want the missing viewer notice", first.logs)
	}
	if got := len(files.files); got != 3 {
		t.Fatalf("first run wrote %d files, want the three role token files", got)
	}
	tokens := maps.Clone(files.files)

	before := len(f.callLog())
	second := runProvisioning(t, f, files, nil, "--repo", protectionRepo)
	if second.code != ExitOK {
		t.Fatalf("second run = %d, want %d (logs: %s)", second.code, ExitOK, second.logs)
	}
	if writes := nonGET(f.callLog()[before:]); len(writes) != 0 {
		t.Errorf("second run sent writes %v, want none", writes)
	}
	for path, data := range tokens {
		if !bytes.Equal(files.files[path], data) {
			t.Errorf("second run rewrote %s", path)
		}
	}
	for _, want := range []string{"bot-polecat already has write access to acme/rig", "branch protection main on acme/rig is already set"} {
		if !strings.Contains(second.logs, want) {
			t.Errorf("second run logs = %q, want %q", second.logs, want)
		}
	}
}

// TestRunOverAnExistingViewerBot: with the viewer bot present, the run grants it read on
// the repository, mints it no token, and a repeat sends no collaborator write.
func TestRunOverAnExistingViewerBot(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.viewerExists = true
	f.addRepo(protectionRepo)
	files := newMemFiles()

	first := runProvisioning(t, f, files, nil, "--repo", protectionRepo)
	if first.code != ExitOK {
		t.Fatalf("first run = %d, want %d (logs: %s)", first.code, ExitOK, first.logs)
	}
	if got := f.permission(protectionRepo, "bot-viewer"); got != permissionRead {
		t.Errorf("viewer permission = %q, want %q", got, permissionRead)
	}
	if !strings.Contains(first.logs, "granted bot-viewer read access to acme/rig") {
		t.Errorf("first run logs = %q, want the viewer grant", first.logs)
	}
	if _, ok := files.files[fixtureTokenFile("viewer")]; ok {
		t.Errorf("viewer token file written, want none: the viewer grant mints no token")
	}

	before := len(f.callLog())
	second := runProvisioning(t, f, files, nil, "--repo", protectionRepo)
	if second.code != ExitOK {
		t.Fatalf("second run = %d, want %d (logs: %s)", second.code, ExitOK, second.logs)
	}
	if writes := nonGET(f.callLog()[before:]); len(writes) != 0 {
		t.Errorf("second run sent writes %v, want none", writes)
	}
	if !strings.Contains(second.logs, "bot-viewer already has read access to acme/rig") {
		t.Errorf("second run logs = %q, want the viewer grant held", second.logs)
	}
}

// TestRunDriftIsRepaired: a main rule that has drifted is patched in place, not created
// again, and the run says so.
func TestRunDriftIsRepaired(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	holdDriftedMain(t, f)
	before := len(f.callLog())

	res := runProvisioning(t, f, newMemFiles(), nil, "--repo", protectionRepo)
	if res.code != ExitOK {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
	}
	calls := f.callLog()[before:]
	if got := countCalls(calls, http.MethodPatch, "/branch_protections/main"); got != 1 {
		t.Errorf("PATCHes of main = %d, want 1", got)
	}
	if got := countCalls(calls, http.MethodPost, "/branch_protections"); got != 0 {
		t.Errorf("POSTs of a branch protection = %d, want 0: the rule exists", got)
	}
	if !strings.Contains(res.logs, "updated branch protection main on acme/rig") {
		t.Errorf("logs = %q, want the update reported", res.logs)
	}
}

// TestRunRemovesCandidateBranchRule: a land/* rule left by an earlier cutover is deleted
// through its encoded name, and main is created, not touched.
func TestRunRemovesCandidateBranchRule(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.protectedLandBranch = true
	f.addRepo(protectionRepo)

	res := runProvisioning(t, f, newMemFiles(), nil, "--repo", protectionRepo)
	if res.code != ExitOK {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
	}
	writes := f.writes()
	if got := countCalls(writes, http.MethodDelete, "/branch_protections/land%2F"); got != 1 {
		t.Errorf("DELETEs of the land/* rule = %d, want 1", got)
	}
	if got := countCalls(writes, http.MethodPost, "/branch_protections"); got != 1 {
		t.Errorf("POSTs of a branch protection = %d, want 1 (main only)", got)
	}
	if !strings.Contains(res.logs, "removed branch protection land/* on acme/rig") {
		t.Errorf("logs = %q, want the removal reported", res.logs)
	}
}

// TestRunRotateReplacesToken: --rotate revokes the previous token of each role by name,
// mints a new one, and the token file holds the new value.
func TestRunRotateReplacesToken(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	files := newMemFiles()
	if first := runProvisioning(t, f, files, nil, "--repo", protectionRepo); first.code != ExitOK {
		t.Fatalf("first run = %d, want %d (logs: %s)", first.code, ExitOK, first.logs)
	}

	before := len(f.callLog())
	res := runProvisioning(t, f, files, nil, "--repo", protectionRepo, "--rotate")
	if res.code != ExitOK {
		t.Fatalf("rotate run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
	}
	calls := f.callLog()[before:]
	if got := countCalls(calls, http.MethodDelete, "/admin/users/"+fixtureLandingBot+"/tokens/"+DefaultTokenName); got != 1 {
		t.Errorf("DELETEs of the previous landing token = %d, want 1", got)
	}
	if got := countCalls(calls, http.MethodPost, "/tokens"); got != 3 {
		t.Errorf("token mints = %d, want 3", got)
	}
	want := "FORGEJO_TOKEN=" + f.token(fixtureLandingBot, DefaultTokenName) + "\n"
	if got := string(files.files[fixtureTokenFile("landing")]); !strings.Contains(got, want) {
		t.Errorf("landing token file = %q, want it to hold the new token", got)
	}
}

// TestRunAdminTokenSources: whichever source holds the admin token, the run presents only
// the one the fake accepts, and neither the token nor a decoy reaches the output.
func TestRunAdminTokenSources(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"the broker's token", map[string]string{"GT_AUTH_FORGEJO_TOKEN": fixtureToken, "FORGEJO_ADMIN_ENV": ""}},
		{"both sources, the broker's wins", map[string]string{"GT_AUTH_FORGEJO_TOKEN": fixtureToken, "FORGEJO_ADMIN_TOKEN": decoyToken}},
		{"the override in the environment", map[string]string{"FORGEJO_ADMIN_TOKEN": fixtureToken, "FORGEJO_ADMIN_ENV": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			f.addRepo(protectionRepo)
			res := runProvisioning(t, f, newMemFiles(), tc.env, "--repo", protectionRepo)
			if res.code != ExitOK {
				t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
			}
			if v := f.violations(); len(v) != 0 {
				t.Errorf("requests without the accepted token: %v", v)
			}
		})
	}
}

// TestRunWithoutAdminToken: a run that names no admin token, or names a file that does not
// exist, fails before any request and says how to get a token.
func TestRunWithoutAdminToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		env     map[string]string
		wantLog string
	}{
		{"no admin token file named", map[string]string{"FORGEJO_ADMIN_ENV": ""}, "sudo -u gt-forge gt-approve run forgejo-provision"},
		{"the admin token file is absent", map[string]string{"FORGEJO_ADMIN_ENV": "/fixture/absent.env"}, "/fixture/absent.env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			f.addRepo(protectionRepo)
			res := runProvisioning(t, f, newMemFiles(), tc.env, "--repo", protectionRepo)
			if res.code != ExitFailure {
				t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.logs)
			}
			if !strings.Contains(res.logs, "no admin token") || !strings.Contains(res.logs, tc.wantLog) {
				t.Errorf("logs = %q, want the no admin token failure naming %q", res.logs, tc.wantLog)
			}
			if calls := f.callLog(); len(calls) != 0 {
				t.Errorf("requests sent without a token: %v", calls)
			}
		})
	}
}

// TestRunIgnoresHomeEnvToken: a token in $HOME/forgejo/.env is never read, so with no
// other source the run fails and the decoy stays unread.
func TestRunIgnoresHomeEnvToken(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	res := runProvisioning(t, f, newMemFiles(), map[string]string{"FORGEJO_ADMIN_ENV": ""}, "--repo", protectionRepo)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.logs)
	}
	for _, path := range res.reads {
		if path == fixtureHomeEnv {
			t.Errorf("the run read %s, which holds a token it must not use", path)
		}
	}
	if calls := f.callLog(); len(calls) != 0 {
		t.Errorf("requests sent with the decoy available: %v", calls)
	}
}

// TestRunDryRunSendsNoWriteAndSaysWould: a dry run against an empty fake reports each write
// it would send as a would line, creates no user, writes no token file and sends no write.
func TestRunDryRunSendsNoWriteAndSaysWould(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	files := newMemFiles()

	res := runProvisioning(t, f, files, nil, "--repo", protectionRepo, "--dry-run")
	if res.code != ExitOK {
		t.Fatalf("dry run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Errorf("dry run sent writes %v, want none", writes)
	}
	if got := len(files.files); got != 0 {
		t.Errorf("dry run wrote %d token files, want none", got)
	}
	for _, want := range []string{
		"would create user bot-polecat",
		"would grant bot-polecat write access to acme/rig",
		"would create branch protection main on acme/rig",
		"dry run: no write was sent",
	} {
		if !strings.Contains(res.logs, want) {
			t.Errorf("dry run logs = %q, want %q", res.logs, want)
		}
	}
}

// TestRunDryRunReportsRemovalAndViewerGrant: over a repository with a candidate-branch
// rule and an existing viewer bot, a dry run says it would remove the rule and grant the
// viewer read, and still sends no write.
func TestRunDryRunReportsRemovalAndViewerGrant(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.viewerExists = true
	f.protectedLandBranch = true
	f.addRepo(protectionRepo)

	res := runProvisioning(t, f, newMemFiles(), nil, "--repo", protectionRepo, "--dry-run")
	if res.code != ExitOK {
		t.Fatalf("dry run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Errorf("dry run sent writes %v, want none", writes)
	}
	for _, want := range []string{
		"would grant bot-viewer read access to acme/rig",
		"would remove branch protection land/* on acme/rig",
	} {
		if !strings.Contains(res.logs, want) {
			t.Errorf("dry run logs = %q, want %q", res.logs, want)
		}
	}
}

// TestRunExitCodes: a run that fails at a step exits 1 and names the step's failure.
func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(f *fakeForgejo)
		env     map[string]string
		args    []string
		wantLog string
	}{
		{
			name:    "a 403 on the mint",
			setup:   func(f *fakeForgejo) { f.adminless = true },
			args:    []string{"--repo", protectionRepo},
			wantLog: "site-administrator",
		},
		{
			name:    "a dropped whitelist entry",
			setup:   func(f *fakeForgejo) { f.dropWhitelistEntry = true },
			args:    []string{"--role", "registry", "--repo", protectionRepo},
			wantLog: "the write for main on acme/rig did not take",
		},
		{
			name:    "a repo the instance does not hold",
			args:    []string{"--repo", "acme/missing"},
			wantLog: "/repos/acme/missing/collaborators/bot-polecat: unexpected HTTP 404",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			f.addRepo(protectionRepo)
			if tc.setup != nil {
				tc.setup(f)
			}
			res := runProvisioning(t, f, newMemFiles(), tc.env, tc.args...)
			if res.code != ExitFailure {
				t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.logs)
			}
			if !strings.Contains(res.logs, tc.wantLog) {
				t.Errorf("logs = %q, want %q", res.logs, tc.wantLog)
			}
		})
	}
}

// TestRunRefusesUsageWithItsMessage: a refused command line never reaches Run. Parse
// returns a *UsageError whose message names the value that failed, and the command
// prints that message and exits 2 (cmd/forgejo-provision's TestMainUsageExitsTwo
// holds the exit code for the same command lines).
func TestRunRefusesUsageWithItsMessage(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args        []string
		wantMessage string
	}{
		"unknown option":            {args: []string{"--nope"}, wantMessage: "unknown option: --nope"},
		"unknown role":              {args: []string{"--role", "admin"}, wantMessage: "unknown role 'admin'"},
		"--repo without OWNER/NAME": {args: []string{"--repo", "gastown"}, wantMessage: "--repo 'gastown' must be OWNER/NAME"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.args, func(string) string { return "" })
			var refused *UsageError
			if !errors.As(err, &refused) {
				t.Fatalf("Parse(%v) = %v, want a usage refusal", tc.args, err)
			}
			if !strings.Contains(refused.Message, tc.wantMessage) {
				t.Errorf("Parse(%v) message = %q, want it to hold %q", tc.args, refused.Message, tc.wantMessage)
			}
		})
	}
}

// TestRunDeniedMintWritesNoTokenFile: a token the server refuses leaves no token file
// behind, so a later run is not fooled into thinking a token exists.
func TestRunDeniedMintWritesNoTokenFile(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.adminless = true
	f.addRepo(protectionRepo)
	files := newMemFiles()
	if res := runProvisioning(t, f, files, nil, "--repo", protectionRepo); res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.logs)
	}
	if got := len(files.files); got != 0 {
		t.Errorf("refused mint wrote %d files, want none", got)
	}
}

// TestRunUnprotectedRepoProvisions: a repository with no protection yet gets only the
// main rule, created.
func TestRunUnprotectedRepoProvisions(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)

	res := runProvisioning(t, f, newMemFiles(), nil, "--repo", protectionRepo)
	if res.code != ExitOK {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
	}
	writes := f.writes()
	if got := countCalls(writes, http.MethodPost, "/branch_protections"); got != 1 {
		t.Errorf("POSTs of a branch protection = %d, want 1 (main only)", got)
	}
	if got := countCalls(writes, http.MethodDelete, "/branch_protections"); got != 0 {
		t.Errorf("DELETEs of a branch protection = %d, want 0", got)
	}
}

// TestRunGrantsAccessBeforeProtection: a repository's collaborator grant is written before
// its protection, because Forgejo drops a whitelist entry for a login that is not yet a
// collaborator (gt-fn9e6.23).
func TestRunGrantsAccessBeforeProtection(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	if res := runProvisioning(t, f, newMemFiles(), nil, "--repo", protectionRepo); res.code != ExitOK {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
	}
	grant, protect := -1, -1
	for i, c := range f.writes() {
		if c.method == http.MethodPut && strings.Contains(c.path, "/collaborators/") && grant < 0 {
			grant = i
		}
		if c.method == http.MethodPost && strings.Contains(c.path, "/branch_protections") && protect < 0 {
			protect = i
		}
	}
	if grant < 0 || protect < 0 || grant > protect {
		t.Errorf("grant write at %d, protection write at %d: want the grant first", grant, protect)
	}
}

// TestRunViewerRoleReadsOnly: provisioning the viewer role grants it read on the repository
// and gives no write to the polecat bot that the run does not name.
func TestRunViewerRoleReadsOnly(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	// The landing bot already holds write access, or the protection's whitelist entry is
	// dropped and the run fails (gt-fn9e6.23).
	if status, _ := f.call(t, http.MethodPut, "/repos/"+protectionRepo+"/collaborators/"+fixtureLandingBot, jsonBody(map[string]string{"permission": permissionWrite})); status != http.StatusNoContent {
		t.Fatalf("grant %s write: status %d, want 204", fixtureLandingBot, status)
	}

	res := runProvisioning(t, f, newMemFiles(), nil, "--role", "viewer", "--repo", protectionRepo)
	if res.code != ExitOK {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitOK, res.logs)
	}
	if got := f.permission(protectionRepo, "bot-viewer"); got != permissionRead {
		t.Errorf("viewer permission = %q, want %q", got, permissionRead)
	}
	if got := f.permission(protectionRepo, "bot-polecat"); got != "" {
		t.Errorf("polecat permission = %q, want none: the viewer run names no polecat", got)
	}
	if got := f.token("bot-viewer", DefaultTokenName); got == "" {
		t.Errorf("viewer holds no token, want one minted for the viewer role")
	}
}

// TestRunFailsWhenAUserReadFails: a bot account read that answers neither found nor missing
// stops the run at that role, before any token is minted or any repository is touched.
func TestRunFailsWhenAUserReadFails(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	files := newMemFiles()

	res := runProvisioning(t, failingRoute{next: f, match: "/users/bot-polecat", status: http.StatusInternalServerError}, files, nil, "--repo", protectionRepo)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.logs)
	}
	if !strings.Contains(res.logs, "GET /users/bot-polecat: unexpected HTTP 500") {
		t.Errorf("logs = %q, want the failed read named", res.logs)
	}
	if got := len(files.files); got != 0 {
		t.Errorf("failed read wrote %d token files, want none", got)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Errorf("failed read sent writes %v, want none", writes)
	}
}

// TestRunFailsWhenTheViewerReadFails: the viewer read is made only for a run that does not
// provision the viewer role, and a read it cannot judge fails the run rather than skipping
// the grant.
func TestRunFailsWhenTheViewerReadFails(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)

	res := runProvisioning(t, failingRoute{next: f, match: "/users/bot-viewer", status: http.StatusInternalServerError}, newMemFiles(), nil, "--repo", protectionRepo)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.logs)
	}
	if !strings.Contains(res.logs, "GET /users/bot-viewer: unexpected HTTP 500") {
		t.Errorf("logs = %q, want the failed viewer read named", res.logs)
	}
	if got := f.permission(protectionRepo, "bot-viewer"); got != "" {
		t.Errorf("viewer permission = %q, want none: the grant was never reached", got)
	}
}

// TestRunProvisionRepoRejectsAnUnknownRole: provisionRepo is the step that asks each role's
// repository access, so a role it has no access for is refused before any request. A
// command line never gets this far, since Parse refuses an unknown role first.
func TestRunProvisionRepoRejectsAnUnknownRole(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	p := &Provisioner{
		Client:    &Client{API: fakeAPIRoot + fakeAPIPath, HTTP: &http.Client{Transport: f}, Token: newSecret(fixtureToken)},
		BotPrefix: DefaultBotPrefix,
	}

	err := provisionRepo(p, protectionRepo, &Options{Roles: []string{"widget"}})
	if err == nil || !strings.Contains(err.Error(), "widget") {
		t.Fatalf("provisionRepo = %v, want the unknown role named", err)
	}
	if calls := f.callLog(); len(calls) != 0 {
		t.Errorf("requests sent for an unknown role: %v", calls)
	}
}

// TestRunNeverPrintsATokenOrTouchesTheRealHome: over every scenario, the output holds
// neither the admin token, the decoy nor any token minted; every file written lands under
// the config dir; and the HOME file is never read.
func TestRunNeverPrintsATokenOrTouchesTheRealHome(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name  string
		setup func(t *testing.T, f *fakeForgejo)
		env   map[string]string
		prime []string
		args  []string
	}{
		{
			name: "a fresh provision",
			setup: func(t *testing.T, f *fakeForgejo) {
				f.addRepo(protectionRepo)
			},
			args: []string{"--repo", protectionRepo},
		},
		{
			name: "a drifted rule",
			setup: func(t *testing.T, f *fakeForgejo) {
				f.addRepo(protectionRepo)
				holdDriftedMain(t, f)
			},
			args: []string{"--repo", protectionRepo},
		},
		{
			name: "a rotation",
			setup: func(t *testing.T, f *fakeForgejo) {
				f.addRepo(protectionRepo)
			},
			prime: []string{"--repo", protectionRepo},
			args:  []string{"--repo", protectionRepo, "--rotate"},
		},
		{
			name: "the broker's token",
			setup: func(t *testing.T, f *fakeForgejo) {
				f.addRepo(protectionRepo)
			},
			env:  map[string]string{"GT_AUTH_FORGEJO_TOKEN": fixtureToken, "FORGEJO_ADMIN_ENV": ""},
			args: []string{"--repo", protectionRepo},
		},
		{
			name: "a dry run",
			setup: func(t *testing.T, f *fakeForgejo) {
				f.addRepo(protectionRepo)
			},
			args: []string{"--repo", protectionRepo, "--dry-run"},
		},
		{
			name: "a refused mint",
			setup: func(t *testing.T, f *fakeForgejo) {
				f.adminless = true
				f.addRepo(protectionRepo)
			},
			args: []string{"--repo", protectionRepo},
		},
		{
			name: "a dropped whitelist entry",
			setup: func(t *testing.T, f *fakeForgejo) {
				f.dropWhitelistEntry = true
				f.addRepo(protectionRepo)
			},
			args: []string{"--role", "registry", "--repo", protectionRepo},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			sc.setup(t, f)
			files := newMemFiles()
			var logs string
			if sc.prime != nil {
				logs += runProvisioning(t, f, files, sc.env, sc.prime...).logs
			}
			res := runProvisioning(t, f, files, sc.env, sc.args...)
			logs += res.logs

			secrets := []namedSecret{{"admin token", fixtureToken}, {"decoy token", decoyToken}}
			for i, minted := range mintedValues(f) {
				secrets = append(secrets, namedSecret{fmt.Sprintf("minted token %d", i+1), minted})
			}
			if msg := leakReport(logs, secrets); msg != "" {
				t.Error(msg)
			}
			for path := range files.files {
				if !strings.HasPrefix(path, fixtureConfigDir+"/") {
					t.Errorf("wrote %s, outside the config dir %s", path, fixtureConfigDir)
				}
			}
			for _, path := range res.reads {
				if path != fixtureAdminFile {
					t.Errorf("read %s, want only the admin token file", path)
				}
			}
		})
	}
}

// TestRunLeakReportNeverQuotesOutput: the failure message of the no-token check names
// the secret it found and where, and never quotes the output that holds it. A failing
// assertion has to be safe to paste into a bead.
func TestRunLeakReportNeverQuotesOutput(t *testing.T) {
	t.Parallel()
	secret := namedSecret{name: "minted token 1", value: "0123456789abcdef0123456789abcdef01234567"}
	output := "forgejo-provision: prefix " + secret.value + " suffix"
	msg := leakReport(output, []namedSecret{secret})
	if msg == "" {
		t.Fatalf("leakReport(output holding the secret) = \"\", want the leak named")
	}
	if strings.Contains(msg, secret.value) {
		t.Errorf("leakReport message %q quotes the secret", msg)
	}
	if strings.Contains(msg, output) {
		t.Errorf("leakReport message %q quotes the output", msg)
	}
	if !strings.Contains(msg, secret.name) {
		t.Errorf("leakReport message %q does not name %q", msg, secret.name)
	}
	if clean := leakReport("forgejo-provision: provisioning complete", []namedSecret{secret}); clean != "" {
		t.Errorf("leakReport(clean output) = %q, want none", clean)
	}
}

// TestRunCancelledBeforeStartSendsNothing: a run whose context is already cancelled
// fails at its first request and sends none: the fake records no call.
func TestRunCancelledBeforeStartSendsNothing(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := runProvisioningCtx(ctx, t, &ctxRoute{next: f}, newMemFiles(), nil, "--repo", protectionRepo)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.logs)
	}
	if !strings.Contains(res.logs, "request failed") {
		t.Errorf("logs = %q, want the cancelled request failed", res.logs)
	}
	if calls := f.callLog(); len(calls) != 0 {
		t.Errorf("requests reached the server under a cancelled context: %v", calls)
	}
}

// TestRunCancelledBetweenTwoRequestsSendsNoMore: cancelled after the second request
// is served, the run fails at the third, and the server records exactly two requests.
func TestRunCancelledBetweenTwoRequestsSendsNoMore(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.addRepo(protectionRepo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := runProvisioningCtx(ctx, t, &ctxRoute{next: f, cancelAfter: 2, cancel: cancel}, newMemFiles(), nil, "--repo", protectionRepo)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.logs)
	}
	if !strings.Contains(res.logs, "request failed") {
		t.Errorf("logs = %q, want the request after the cancel failed", res.logs)
	}
	if calls := f.callLog(); len(calls) != 2 {
		t.Errorf("the server recorded %d requests, want 2: the request after the cancel must not be sent", len(calls))
	}
}
