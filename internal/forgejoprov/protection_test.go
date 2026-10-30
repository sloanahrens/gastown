package forgejoprov

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// protectionRepo is the repository the branch protection tests protect.
const protectionRepo = "acme/rig"

// fixtureMainRule and fixtureLandingBot name the rule the run protects and the landing bot it
// whitelists, as the provisioning run does.
const (
	fixtureMainRule   = "main"
	fixtureLandingBot = DefaultBotPrefix + "landing"
)

// testOptions is the command line the protection tests run with: the defaults, with a
// landing bot and the default gate contexts.
func testOptions() Options {
	return Options{
		MainBranch:   DefaultMainBranch,
		LandBranch:   DefaultLandBranch,
		LandingBot:   DefaultBotPrefix + "landing",
		GateContexts: DefaultGateContexts(),
	}
}

// servedRule is rule as the fake stores and returns it: the JSON round trip gives the
// types a reply decodes to.
func servedRule(t *testing.T, rule map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("encode rule: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode rule: %v", err)
	}
	return out
}

// seedRule puts rule on repo in the fake, as an earlier run would have left it.
func seedRule(t *testing.T, f *forgejotest.Fake, repo string, rule map[string]any) {
	t.Helper()
	f.SeedRule(repo, rule)
}

// ruleNames returns the names of the rules repo holds in the fake, sorted.
func ruleNames(f *forgejotest.Fake, repo string) []string {
	return f.RuleNames(repo)
}

// listJSON encodes rules as the list reply the route answers with.
func listJSON(t *testing.T, rules ...map[string]any) string {
	t.Helper()
	data, err := json.Marshal(rules)
	if err != nil {
		t.Fatalf("encode rule list: %v", err)
	}
	return string(data)
}

// ruleTransport answers the rule list with getStatus and getBody, and every write with
// writeStatus and writeBody, so a test drives one failing branch at a time.
type ruleTransport struct {
	getStatus   int
	getBody     string
	writeStatus int
	writeBody   string
}

func (r *ruleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		return respond(req, r.getStatus, r.getBody)
	}
	return respond(req, r.writeStatus, r.writeBody)
}

// TestMainRuleFields checks every field of the main rule's body, including the landing
// bot on the merge whitelist and both default gate contexts.
func TestMainRuleFields(t *testing.T) {
	t.Parallel()
	opts := testOptions()
	want := map[string]any{
		"rule_name":                  "main",
		"enable_push":                false,
		"enable_push_whitelist":      false,
		"push_whitelist_usernames":   []string{},
		"push_whitelist_deploy_keys": false,
		"apply_to_admins":            true,
		"enable_merge_whitelist":     true,
		"merge_whitelist_usernames":  []string{"bot-landing"},
		"enable_status_check":        true,
		"status_check_contexts":      []string{"ci / gate (push)", "om / review"},
		"block_on_outdated_branch":   true,
	}
	got, err := json.Marshal(MainRule(opts))
	if err != nil {
		t.Fatalf("encode rule: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode want: %v", err)
	}
	if string(got) != string(wantJSON) {
		t.Fatalf("MainRule body = %s, want %s", got, wantJSON)
	}
}

// TestEnsureRuleCreateConvergeAndIdempotent checks that an absent rule is created with one
// POST, a drifted rule is patched with one PATCH, and a rule that already matches sends no
// write. A second run after either write sends nothing more.
func TestEnsureRuleCreateConvergeAndIdempotent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		seed       func(t *testing.T, opts Options) map[string]any
		wantMethod string
		wantLog    string
	}{
		{name: "absent is created", wantMethod: http.MethodPost, wantLog: "created branch protection main"},
		{
			name: "drifted merge whitelist is patched",
			seed: func(t *testing.T, opts Options) map[string]any {
				rule := servedRule(t, MainRule(opts))
				rule["merge_whitelist_usernames"] = []any{}
				return rule
			},
			wantMethod: http.MethodPatch,
			wantLog:    "updated branch protection main",
		},
		{
			name: "drifted is patched",
			seed: func(t *testing.T, opts Options) map[string]any {
				rule := servedRule(t, MainRule(opts))
				rule["enable_push"] = true
				return rule
			},
			wantMethod: http.MethodPatch,
			wantLog:    "updated branch protection main",
		},
		{
			name: "drifted push whitelist is patched",
			seed: func(t *testing.T, opts Options) map[string]any {
				rule := servedRule(t, MainRule(opts))
				rule["enable_push_whitelist"] = true
				rule["push_whitelist_usernames"] = []any{"someone"}
				return rule
			},
			wantMethod: http.MethodPatch,
			wantLog:    "updated branch protection main",
		},
		{
			name: "drifted push whitelist entry alone is patched",
			seed: func(t *testing.T, opts Options) map[string]any {
				rule := servedRule(t, MainRule(opts))
				rule["push_whitelist_usernames"] = []any{"someone"}
				return rule
			},
			wantMethod: http.MethodPatch,
			wantLog:    "updated branch protection main",
		},
		{
			name:    "identical sends no write",
			seed:    func(t *testing.T, opts Options) map[string]any { return servedRule(t, MainRule(opts)) },
			wantLog: "already set",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			opts := testOptions()
			f := forgejotest.New()
			f.AddRepo(protectionRepo)
			if c.seed != nil {
				seedRule(t, f, protectionRepo, c.seed(t, opts))
			}
			var logs []string
			p := userProvisioner(f, false, &logs)
			if err := p.EnsureRule(protectionRepo, opts); err != nil {
				t.Fatalf("first run: %v", err)
			}
			wantWrites := 1
			if c.wantMethod == "" {
				wantWrites = 0
			}
			if n := len(f.Writes()); n != wantWrites {
				t.Fatalf("first run sent %d writes, want %d", n, wantWrites)
			}
			if wantWrites == 1 && f.Writes()[0].Method != c.wantMethod {
				t.Fatalf("first run sent %s, want %s", f.Writes()[0].Method, c.wantMethod)
			}
			if !strings.Contains(strings.Join(logs, "\n"), c.wantLog) {
				t.Fatalf("first run logged %q, want a line containing %q", logs, c.wantLog)
			}
			want, err := json.Marshal(MainRule(opts))
			if err != nil {
				t.Fatalf("encode rule: %v", err)
			}
			got, err := json.Marshal(f.Rule(protectionRepo, "main"))
			if err != nil {
				t.Fatalf("encode stored rule: %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("stored rule = %s, want %s", got, want)
			}
			if err := p.EnsureRule(protectionRepo, opts); err != nil {
				t.Fatalf("second run: %v", err)
			}
			if n := len(f.Writes()); n != wantWrites {
				t.Fatalf("second run sent %d writes in all, want the first run's %d and no more", n, wantWrites)
			}
		})
	}
}

// TestEnsureRuleJudgesEveryField checks that a live rule differing from MainRule in any one
// field is patched, not reported as already set. Each row changes one field and nothing
// else, so a field left out of the drift check fails its row. The rule name is the lookup
// key, so it cannot differ on a rule that was found, and it has no row.
func TestEnsureRuleJudgesEveryField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		field string
		value any
	}{
		{name: "enable_push", field: "enable_push", value: true},
		{name: "enable_push_whitelist", field: "enable_push_whitelist", value: true},
		{name: "push whitelist entry while enable_push is false", field: "push_whitelist_usernames", value: []any{"someone"}},
		{name: "push_whitelist_deploy_keys", field: "push_whitelist_deploy_keys", value: true},
		{name: "apply_to_admins", field: "apply_to_admins", value: false},
		{name: "enable_merge_whitelist", field: "enable_merge_whitelist", value: false},
		{name: "merge whitelist with an extra user", field: "merge_whitelist_usernames", value: []any{"bot-landing", "someone"}},
		{name: "enable_status_check", field: "enable_status_check", value: false},
		{name: "status_check_contexts with one context missing", field: "status_check_contexts", value: []any{"om / review"}},
		{name: "block_on_outdated_branch", field: "block_on_outdated_branch", value: false},
	}
	covered := map[string]bool{"rule_name": true}
	for _, c := range cases {
		covered[c.field] = true
	}
	for key := range MainRule(testOptions()) {
		if !covered[key] {
			t.Errorf("MainRule writes %q but no drift row changes it", key)
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			opts := testOptions()
			f := forgejotest.New()
			f.AddRepo(protectionRepo)
			drifted := servedRule(t, MainRule(opts))
			drifted[c.field] = c.value
			seedRule(t, f, protectionRepo, drifted)
			var logs []string
			p := userProvisioner(f, false, &logs)
			if err := p.EnsureRule(protectionRepo, opts); err != nil {
				t.Fatalf("EnsureRule: %v", err)
			}
			writes := f.Writes()
			if len(writes) != 1 || writes[0].Method != http.MethodPatch {
				t.Fatalf("sent %d writes, want one PATCH", len(writes))
			}
		})
	}
}

// TestEnsureRuleFailsWhenTheWriteDoesNotTake checks that a PATCH whose reply still shows
// the old merge whitelist fails the run, instead of passing as set.
func TestEnsureRuleFailsWhenTheWriteDoesNotTake(t *testing.T) {
	t.Parallel()
	opts := testOptions()
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	f.DropWhitelistEntry = true
	drifted := servedRule(t, MainRule(opts))
	drifted["enable_push"] = true
	drifted["merge_whitelist_usernames"] = []any{}
	seedRule(t, f, protectionRepo, drifted)
	var logs []string
	p := userProvisioner(f, false, &logs)
	err := p.EnsureRule(protectionRepo, opts)
	if err == nil || !strings.Contains(err.Error(), "the write for main on acme/rig did not take") {
		t.Fatalf("EnsureRule = %v, want the write for main on acme/rig did not take", err)
	}
}

// TestEnsureRuleMissingRepoIs404Message checks that a repository the token cannot see
// fails with the message that names the API root and the token, and sends no write.
func TestEnsureRuleMissingRepoIs404Message(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	f.RepoMissing = true
	var logs []string
	p := userProvisioner(f, false, &logs)
	err := p.EnsureRule(protectionRepo, testOptions())
	want := "GET /repos/acme/rig/branch_protections returned 404: the repository is not on " +
		forgejotest.APIRoot + forgejotest.APIPath + ", or the admin token cannot see it"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("EnsureRule = %v, want a message starting %q", err, want)
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("a missing repository sent %d writes, want none", n)
	}
}

// TestEnsureRuleRefusesEachFailingBranch drives every error branch of EnsureRule. Each
// failing reply echoes the admin token and the minted secret, so each error is also
// checked not to carry either value.
func TestEnsureRuleRefusesEachFailingBranch(t *testing.T) {
	t.Parallel()
	opts := testOptions()
	drifted := servedRule(t, MainRule(opts))
	drifted["enable_push"] = true
	driftedList := listJSON(t, drifted)
	cases := []struct {
		name      string
		transport *ruleTransport
		wantErr   string
	}{
		{
			name:      "list refused",
			transport: &ruleTransport{getStatus: http.StatusInternalServerError, getBody: echoReply},
			wantErr:   "GET /repos/acme/rig/branch_protections: unexpected HTTP 500",
		},
		{
			name:      "list not found",
			transport: &ruleTransport{getStatus: http.StatusNotFound, getBody: echoReply},
			wantErr:   "returned 404",
		},
		{
			name:      "list is not a rule list",
			transport: &ruleTransport{getStatus: http.StatusOK, getBody: echoReply},
			wantErr:   "reply is not a list of rules",
		},
		{
			name:      "create refused",
			transport: &ruleTransport{getStatus: http.StatusOK, getBody: "[]", writeStatus: http.StatusUnprocessableEntity, writeBody: echoReply},
			wantErr:   "POST /repos/acme/rig/branch_protections: unexpected HTTP 422",
		},
		{
			name:      "patch refused",
			transport: &ruleTransport{getStatus: http.StatusOK, getBody: driftedList, writeStatus: http.StatusInternalServerError, writeBody: echoReply},
			wantErr:   "PATCH /repos/acme/rig/branch_protections/main: unexpected HTTP 500",
		},
		{
			name:      "patch names no rule",
			transport: &ruleTransport{getStatus: http.StatusOK, getBody: driftedList, writeStatus: http.StatusNotFound, writeBody: echoReply},
			wantErr:   "Set 'main' by hand in the web UI",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var logs []string
			p := &Provisioner{Client: &Client{
				API:   forgejotest.APIRoot + forgejotest.APIPath,
				HTTP:  &http.Client{Transport: c.transport},
				Token: newSecret(fixtureToken),
				Log:   func(line string) { logs = append(logs, line) },
			}}
			err := p.EnsureRule(protectionRepo, opts)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("EnsureRule = %v, want an error containing %q", err, c.wantErr)
			}
			if strings.Contains(err.Error(), fixtureToken) || strings.Contains(err.Error(), mintedSecret) {
				t.Fatalf("error carries a secret: %v", err)
			}
		})
	}
}

// TestEnsureRuleDryRunSendsNoWrite checks that a dry run reports the create and sends no
// write.
func TestEnsureRuleDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	var logs []string
	p := userProvisioner(f, true, &logs)
	if err := p.EnsureRule(protectionRepo, testOptions()); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("dry run sent %d writes, want none", n)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "would create branch protection main") {
		t.Fatalf("dry run logged %q, want it to say it would create the rule", logs)
	}
}

// TestEnsureNoRuleRemovesOnlyTheLandPattern checks that the land/* rule is deleted, with
// its name escaped as land%2F*, and that the main rule is left as it was.
func TestEnsureNoRuleRemovesOnlyTheLandPattern(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.ProtectedLandBranch = true
	f.AddRepo(protectionRepo)
	opts := testOptions()
	seedRule(t, f, protectionRepo, servedRule(t, MainRule(opts)))
	var logs []string
	p := userProvisioner(f, false, &logs)
	if err := p.EnsureNoRule(protectionRepo, DefaultLandBranch); err != nil {
		t.Fatalf("EnsureNoRule: %v", err)
	}
	writes := f.Writes()
	if len(writes) != 1 || writes[0].Method != http.MethodDelete {
		t.Fatalf("sent %v, want one DELETE", writes)
	}
	if !strings.HasSuffix(writes[0].Path, "/branch_protections/land%2F*") {
		t.Fatalf("DELETE path = %q, want it to end with land%%2F*", writes[0].Path)
	}
	if got := ruleNames(f, protectionRepo); !slices.Equal(got, []string{"main"}) {
		t.Fatalf("rules after the run = %v, want only main", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "removed branch protection land/*") {
		t.Fatalf("logged %q, want a line saying the land rule was removed", logs)
	}
}

// TestEnsureNoRuleAcceptsAnAbsentRule checks that a repository with no land/* rule sends no
// write and says so.
func TestEnsureNoRuleAcceptsAnAbsentRule(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	var logs []string
	p := userProvisioner(f, false, &logs)
	if err := p.EnsureNoRule(protectionRepo, DefaultLandBranch); err != nil {
		t.Fatalf("EnsureNoRule: %v", err)
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("sent %d writes, want none", n)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "no branch protection land/* on acme/rig: the main rule is the only protected branch") {
		t.Fatalf("logged %q, want a line saying no land rule exists", logs)
	}
}

// listThenFailTransport answers the rule list with list and fails every write at the
// transport, so a test reaches a write's own transport error.
type listThenFailTransport struct {
	list string
}

func (l listThenFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		return respond(req, http.StatusOK, l.list)
	}
	return failingTransport{}.RoundTrip(req)
}

// TestProtectionStopsOnARefusedRequest checks that a request the transport fails stops
// the run with that error, whichever request it is.
func TestProtectionStopsOnARefusedRequest(t *testing.T) {
	t.Parallel()
	opts := testOptions()
	drifted := servedRule(t, MainRule(opts))
	drifted["enable_push"] = true
	driftedList := listJSON(t, drifted)
	landList := listJSON(t, map[string]any{"rule_name": DefaultLandBranch})
	cases := []struct {
		name      string
		transport http.RoundTripper
		run       func(p *Provisioner) error
		wantErr   string
	}{
		{
			name:      "rule list fails",
			transport: failingTransport{},
			run:       func(p *Provisioner) error { return p.EnsureRule(protectionRepo, opts) },
			wantErr:   "/branch_protections: request failed",
		},
		{
			name:      "patch fails",
			transport: listThenFailTransport{list: driftedList},
			run:       func(p *Provisioner) error { return p.EnsureRule(protectionRepo, opts) },
			wantErr:   "/branch_protections/main: request failed",
		},
		{
			name:      "create fails",
			transport: listThenFailTransport{list: "[]"},
			run:       func(p *Provisioner) error { return p.EnsureRule(protectionRepo, opts) },
			wantErr:   "/branch_protections: request failed",
		},
		{
			name:      "no-rule list fails",
			transport: failingTransport{},
			run:       func(p *Provisioner) error { return p.EnsureNoRule(protectionRepo, DefaultLandBranch) },
			wantErr:   "/branch_protections: request failed",
		},
		{
			name:      "delete fails",
			transport: listThenFailTransport{list: landList},
			run:       func(p *Provisioner) error { return p.EnsureNoRule(protectionRepo, DefaultLandBranch) },
			wantErr:   "/branch_protections/land%2F*: request failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := &Provisioner{Client: &Client{
				API:   forgejotest.APIRoot + forgejotest.APIPath,
				HTTP:  &http.Client{Transport: c.transport},
				Token: newSecret(fixtureToken),
			}}
			err := c.run(p)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("run = %v, want an error containing %q", err, c.wantErr)
			}
		})
	}
}

// TestProtectionRefusesABadRepoBeforeAnyRequest checks that a repository name that is not
// two plain names stops both commands before a request is sent.
func TestProtectionRefusesABadRepoBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	opts := testOptions()
	cases := []struct {
		name string
		run  func(p *Provisioner) error
	}{
		{name: "EnsureRule", run: func(p *Provisioner) error { return p.EnsureRule("acme", opts) }},
		{name: "EnsureNoRule", run: func(p *Provisioner) error { return p.EnsureNoRule("acme", DefaultLandBranch) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := forgejotest.New()
			var logs []string
			p := userProvisioner(f, false, &logs)
			if err := c.run(p); err == nil {
				t.Fatalf("%s accepted a repository name with no owner", c.name)
			}
			if n := len(f.CallLog()); n != 0 {
				t.Fatalf("%s sent %d requests, want none", c.name, n)
			}
		})
	}
}

// TestEnsureNoRuleBranches drives the delete branches of EnsureNoRule that the fake does
// not reach: a rule removed by another run between the read and the delete, a refused
// delete, and a dry run that names the removal and sends none.
func TestEnsureNoRuleBranches(t *testing.T) {
	t.Parallel()
	landList := listJSON(t, map[string]any{"rule_name": DefaultLandBranch})
	cases := []struct {
		name      string
		transport *ruleTransport
		dryRun    bool
		wantErr   string
		wantLog   string
	}{
		{
			name:      "already absent at the delete",
			transport: &ruleTransport{getStatus: http.StatusOK, getBody: landList, writeStatus: http.StatusNotFound, writeBody: echoReply},
			wantLog:   "removed branch protection land/* on acme/rig (already absent)",
		},
		{
			name:      "delete refused",
			transport: &ruleTransport{getStatus: http.StatusOK, getBody: landList, writeStatus: http.StatusInternalServerError, writeBody: echoReply},
			wantErr:   "DELETE /repos/acme/rig/branch_protections/land%2F*: unexpected HTTP 500",
		},
		{
			name:      "dry run names the removal",
			transport: &ruleTransport{getStatus: http.StatusOK, getBody: landList},
			dryRun:    true,
			wantLog:   "would remove branch protection land/* on acme/rig",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var logs []string
			p := &Provisioner{Client: &Client{
				API:    forgejotest.APIRoot + forgejotest.APIPath,
				HTTP:   &http.Client{Transport: c.transport},
				Token:  newSecret(fixtureToken),
				DryRun: c.dryRun,
				Log:    func(line string) { logs = append(logs, line) },
			}}
			err := p.EnsureNoRule(protectionRepo, DefaultLandBranch)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("EnsureNoRule = %v, want an error containing %q", err, c.wantErr)
				}
				if strings.Contains(err.Error(), fixtureToken) || strings.Contains(err.Error(), mintedSecret) {
					t.Fatalf("error carries a secret: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureNoRule: %v", err)
			}
			if !strings.Contains(strings.Join(logs, "\n"), c.wantLog) {
				t.Fatalf("logged %q, want a line containing %q", logs, c.wantLog)
			}
		})
	}
}

// countTransport answers the rule list with getBody and counts every write.
type countTransport struct {
	getBody string
	writes  int
}

func (c *countTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		return respond(req, http.StatusOK, c.getBody)
	}
	c.writes++
	return respond(req, http.StatusInternalServerError, echoReply)
}

// TestEnsureRuleTreatsNullAndEmptyListAsTheSame checks that a rule whose empty list the
// server answers as null, or leaves out, is not drift: Forgejo encodes "no entries"
// either way, and a PATCH for it would repeat on every run.
func TestEnsureRuleTreatsNullAndEmptyListAsTheSame(t *testing.T) {
	t.Parallel()
	opts := testOptions()
	for _, shape := range []string{"null", "absent"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			held := servedRule(t, MainRule(opts))
			if shape == "null" {
				held["push_whitelist_usernames"] = nil
			} else {
				delete(held, "push_whitelist_usernames")
			}
			tr := &countTransport{getBody: listJSON(t, held)}
			p := &Provisioner{Client: &Client{
				API:   forgejotest.APIRoot + forgejotest.APIPath,
				HTTP:  &http.Client{Transport: tr},
				Token: newSecret(fixtureToken),
				Log:   func(string) {},
			}}
			if err := p.EnsureRule(protectionRepo, opts); err != nil {
				t.Fatalf("EnsureRule: %v", err)
			}
			if tr.writes != 0 {
				t.Fatalf("sent %d writes for an empty list the server answered as %s, want none", tr.writes, shape)
			}
		})
	}
}

// TestSameJSONNullIsOnlyTheEmptyList checks that null equals [] and nothing else: a
// false flag or a one-entry list is not "no entries".
func TestSameJSONNullIsOnlyTheEmptyList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{name: "null and empty list", a: nil, b: []string{}, want: true},
		{name: "empty list and null", a: []string{}, b: nil, want: true},
		{name: "null and false", a: nil, b: false, want: false},
		{name: "null and one entry", a: nil, b: []string{"x"}, want: false},
		{name: "equal lists", a: []any{"x"}, b: []string{"x"}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := sameJSON(c.a, c.b); got != c.want {
				t.Fatalf("sameJSON(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestEnsureRuleRefusesANullRuleList checks that a literal null reply to the list is an
// error, not an absent rule that would trigger a create.
func TestEnsureRuleRefusesANullRuleList(t *testing.T) {
	t.Parallel()
	tr := &countTransport{getBody: "null"}
	p := &Provisioner{Client: &Client{
		API:   forgejotest.APIRoot + forgejotest.APIPath,
		HTTP:  &http.Client{Transport: tr},
		Token: newSecret(fixtureToken),
		Log:   func(string) {},
	}}
	err := p.EnsureRule(protectionRepo, testOptions())
	if err == nil || !strings.Contains(err.Error(), "reply is not a list of rules") {
		t.Fatalf("EnsureRule = %v, want reply is not a list of rules", err)
	}
	if tr.writes != 0 {
		t.Fatalf("sent %d writes after a null list, want none", tr.writes)
	}
}

// TestRuleSegmentEscapesWhatEndsAPath checks that no character of a rule name can end the
// path segment or start a query, while the glob's "*" stays as written.
func TestRuleSegmentEscapesWhatEndsAPath(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, rule, want string }{
		{name: "slash", rule: "release/*", want: "release%2F*"},
		{name: "percent", rule: "a%b", want: "a%25b"},
		{name: "question mark", rule: "a?b", want: "a%3Fb"},
		{name: "hash", rule: "a#b", want: "a%23b"},
		{name: "space", rule: "a b", want: "a%20b"},
		{name: "plain", rule: "main", want: "main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ruleSegment(c.rule); got != c.want {
				t.Fatalf("ruleSegment(%q) = %q, want %q", c.rule, got, c.want)
			}
		})
	}
}
