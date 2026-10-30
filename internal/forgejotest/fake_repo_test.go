package forgejotest

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The fixtures name a repository, a branch rule and a merge whitelist the way the
// provisioning run does: the default bot prefix, the landing bot, and the land/* rule
// that the run deletes.
const (
	fixtureRepo       = "forgejo-owner/gastown"
	fixtureOtherRepo  = "forgejo-owner/elsewhere"
	fixtureMainRule   = "main"
	fixtureLandRule   = "land/*"
	fixtureLandingBot = botPrefix + "landing"
	fixtureViewerBot  = botPrefix + "viewer"
	fixturePermission = "write"
)

// listedRule is the part of a branch protection rule the provisioning run reads back.
type listedRule struct {
	RuleName                string   `json:"rule_name"`
	MergeWhitelistUsernames []string `json:"merge_whitelist_usernames"`
	BlockOnOutdatedBranch   bool     `json:"block_on_outdated_branch"`
}

// collaboratorRoute is the route of one user's access to the fixture repository.
func collaboratorRoute(user string) string {
	return "/repos/" + fixtureRepo + "/collaborators/" + user
}

// rulesRoute is the route that lists and creates the fixture repository's rules.
func rulesRoute() string {
	return "/repos/" + fixtureRepo + "/branch_protections"
}

// ruleRoute names a rule in the path the way the provisioning run does: each "/" in
// the name becomes %2F, so the rule stays one path segment.
func ruleRoute(rule string) string {
	return rulesRoute() + "/" + strings.ReplaceAll(rule, "/", "%2F")
}

// holdRule creates a rule on the fixture repository through the route the run posts to.
func holdRule(t *testing.T, f *Fake, name string) {
	t.Helper()
	if status, _ := f.Call(t, http.MethodPost, rulesRoute(), JSONBody(RuleBody(name, nil))); status != http.StatusCreated {
		t.Fatalf("create rule %s: status %d, want 201", name, status)
	}
}

// listedRules reads the fixture repository's rule list through the route the run reads.
func listedRules(t *testing.T, f *Fake) []listedRule {
	t.Helper()
	status, body := f.Call(t, http.MethodGet, rulesRoute(), "")
	if status != http.StatusOK {
		t.Fatalf("GET of the rule list: status %d, want 200", status)
	}
	var rules []listedRule
	if err := json.Unmarshal([]byte(body), &rules); err != nil {
		t.Fatalf("rule list is not JSON: %v", err)
	}
	return rules
}

// ruleNamed returns the listed rule with the name, and fails the test when it is absent.
func ruleNamed(t *testing.T, rules []listedRule, name string) listedRule {
	t.Helper()
	for _, r := range rules {
		if r.RuleName == name {
			return r
		}
	}
	t.Fatalf("no rule %s in %+v", name, rules)
	return listedRule{}
}

// TestFakeRepoCollaborators checks that a user with no access is 404, that a PUT grants
// the permission its body names (204), and that the stored permission follows each PUT.
func TestFakeRepoCollaborators(t *testing.T) {
	t.Parallel()
	f := New()
	f.AddRepo(fixtureRepo)
	route := collaboratorRoute(fixtureBot)

	if status, _ := f.Call(t, http.MethodGet, route, ""); status != http.StatusNotFound {
		t.Fatalf("GET of a user with no access: status %d, want 404", status)
	}
	if status, _ := f.Call(t, http.MethodPut, route, JSONBody(map[string]string{"permission": fixturePermission})); status != http.StatusNoContent {
		t.Fatalf("PUT of %s access: status %d, want 204", fixturePermission, status)
	}
	if status, _ := f.Call(t, http.MethodGet, route, ""); status != http.StatusNoContent {
		t.Fatalf("GET after the grant: status %d, want 204", status)
	}
	if got := f.Permission(fixtureRepo, fixtureBot); got != fixturePermission {
		t.Fatalf("stored permission %q, want %q", got, fixturePermission)
	}
	if status, _ := f.Call(t, http.MethodPut, route, JSONBody(map[string]string{"permission": "read"})); status != http.StatusNoContent {
		t.Fatalf("PUT of read access: status %d, want 204", status)
	}
	if got := f.Permission(fixtureRepo, fixtureBot); got != "read" {
		t.Fatalf("stored permission after a second PUT %q, want read", got)
	}

	want := []LoggedCall{
		{http.MethodPut, APIPath + route, http.StatusNoContent},
		{http.MethodPut, APIPath + route, http.StatusNoContent},
	}
	if got := f.Writes(); !slices.Equal(got, want) {
		t.Fatalf("writes: got %v, want %v", got, want)
	}
}

// TestFakeRepoBranchProtections checks that a rule POSTed is listed, that PATCH changes
// it, and that DELETE removes it, and that a rule whose name holds a slash does all of
// this through its %2F-escaped path.
func TestFakeRepoBranchProtections(t *testing.T) {
	t.Parallel()
	f := New()
	f.AddRepo(fixtureRepo)
	if got := listedRules(t, f); len(got) != 0 {
		t.Fatalf("a new repository lists %d rules, want none", len(got))
	}

	landing := []string{fixtureLandingBot}
	if status, _ := f.Call(t, http.MethodPost, rulesRoute(), JSONBody(RuleBody(fixtureLandRule, landing))); status != http.StatusCreated {
		t.Fatalf("create %s: status %d, want 201", fixtureLandRule, status)
	}
	rule := ruleNamed(t, listedRules(t, f), fixtureLandRule)
	if !slices.Equal(rule.MergeWhitelistUsernames, landing) || !rule.BlockOnOutdatedBranch {
		t.Fatalf("created rule lists as %+v, want the whitelist %v and block_on_outdated_branch", rule, landing)
	}

	if status, _ := f.Call(t, http.MethodPatch, ruleRoute(fixtureLandRule), JSONBody(map[string]any{"block_on_outdated_branch": false})); status != http.StatusOK {
		t.Fatalf("PATCH of %s: status %d, want 200", fixtureLandRule, status)
	}
	if got := ruleNamed(t, listedRules(t, f), fixtureLandRule); got.BlockOnOutdatedBranch {
		t.Fatalf("PATCH did not change block_on_outdated_branch")
	}

	if status, _ := f.Call(t, http.MethodDelete, ruleRoute(fixtureLandRule), ""); status != http.StatusNoContent {
		t.Fatalf("DELETE of %s: status %d, want 204", fixtureLandRule, status)
	}
	if status, _ := f.Call(t, http.MethodDelete, ruleRoute(fixtureLandRule), ""); status != http.StatusNotFound {
		t.Fatalf("DELETE of a rule already gone: status %d, want 404", status)
	}
	if got := listedRules(t, f); len(got) != 0 {
		t.Fatalf("a deleted rule is still listed: %+v", got)
	}

	want := []LoggedCall{
		{http.MethodPost, APIPath + rulesRoute(), http.StatusCreated},
		{http.MethodPatch, APIPath + ruleRoute(fixtureLandRule), http.StatusOK},
		{http.MethodDelete, APIPath + ruleRoute(fixtureLandRule), http.StatusNoContent},
		{http.MethodDelete, APIPath + ruleRoute(fixtureLandRule), http.StatusNotFound},
	}
	if got := f.Writes(); !slices.Equal(got, want) {
		t.Fatalf("writes: got %v, want %v", got, want)
	}
}

// TestFakeRepoRefusesWhatTheModelRefuses checks that each request the fake's model refuses
// is refused here with the status the model gives: a body missing what the route needs, a
// rule name already held, a rule or repository the fake lacks. The statuses are the model's
// own and no live server is consulted, so this checks the fake against its model, not
// against the server.
func TestFakeRepoRefusesWhatTheModelRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(*testing.T, *Fake)
		method string
		path   string
		body   string
		status int
	}{
		{"a collaborator PUT with no permission", nil, http.MethodPut, collaboratorRoute(fixtureBot), `{}`, http.StatusUnprocessableEntity},
		{"a collaborator PUT whose body is not JSON", nil, http.MethodPut, collaboratorRoute(fixtureBot), `not json`, http.StatusUnprocessableEntity},
		{"a rule POST with no rule_name", nil, http.MethodPost, rulesRoute(), `{"enable_push":false}`, http.StatusUnprocessableEntity},
		{"a rule POST whose body is not an object", nil, http.MethodPost, rulesRoute(), `[]`, http.StatusUnprocessableEntity},
		{"a rule POST for a name already held", func(t *testing.T, f *Fake) { holdRule(t, f, fixtureMainRule) },
			http.MethodPost, rulesRoute(), JSONBody(RuleBody(fixtureMainRule, nil)), http.StatusUnprocessableEntity},
		{"a rule PATCH for a rule the repository lacks", nil, http.MethodPatch, ruleRoute(fixtureMainRule), `{}`, http.StatusNotFound},
		{"a rule PATCH whose body is not an object", func(t *testing.T, f *Fake) { holdRule(t, f, fixtureMainRule) },
			http.MethodPatch, ruleRoute(fixtureMainRule), `[]`, http.StatusUnprocessableEntity},
		{"a rule DELETE for a rule the repository lacks", nil, http.MethodDelete, ruleRoute(fixtureMainRule), "", http.StatusNotFound},
		{"a PATCH on the rule list, which takes no PATCH", nil, http.MethodPatch, rulesRoute(), `{}`, http.StatusNotFound},
		{"the rule list of a repository the fake does not hold", nil, http.MethodGet, "/repos/" + fixtureOtherRepo + "/branch_protections", "", http.StatusNotFound},
		{"a collaborator PUT on a repository the fake does not hold", nil, http.MethodPut, "/repos/" + fixtureOtherRepo + "/collaborators/" + fixtureBot,
			JSONBody(map[string]string{"permission": fixturePermission}), http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := New()
			f.AddRepo(fixtureRepo)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			if status, _ := f.Call(t, tc.method, tc.path, tc.body); status != tc.status {
				t.Fatalf("%s %s: status %d, want %d", tc.method, tc.path, status, tc.status)
			}
		})
	}
}

// TestFakeRepoRefusesAnUnescapableSegment checks that a path segment that is not valid
// escaped text is a 400 and is not read as a rule name. The transport never builds such
// a path, so the test calls the router with the segment directly.
func TestFakeRepoRefusesAnUnescapableSegment(t *testing.T) {
	t.Parallel()
	f := New()
	f.AddRepo(fixtureRepo)
	segments := []string{"forgejo-owner", "gastown", "branch_protections", "land%zz"}
	if status, _ := f.serveRepo(http.MethodDelete, segments, nil); status != http.StatusBadRequest {
		t.Fatalf("DELETE of a segment that is not escaped text: status %d, want 400", status)
	}
}

// TestFakeRoutesAndLogsTheEscapedPath checks that every route is answered on its escaped
// path and logged under it: a rule name's "/" is sent as %2F and must stay in its segment,
// and it shows as %2F in the call log and in the violation log. The routes are a repository
// route, a user route, and a request that lacks the admin token.
func TestFakeRoutesAndLogsTheEscapedPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"a rule route with its slash escaped", http.MethodDelete, ruleRoute(fixtureLandRule), http.StatusNoContent},
		{"a user route", http.MethodGet, "/users/" + fixtureBot, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := New()
			f.ProtectedLandBranch = true
			f.AddRepo(fixtureRepo)
			if status, _ := f.Call(t, http.MethodPost, "/admin/users", JSONBody(UserBody(fixtureBot))); status != http.StatusCreated {
				t.Fatalf("create %s: status %d, want 201", fixtureBot, status)
			}
			before := len(f.CallLog())
			if status, _ := f.Call(t, tc.method, tc.path, ""); status != tc.status {
				t.Fatalf("%s %s: status %d, want %d", tc.method, tc.path, status, tc.status)
			}
			want := []LoggedCall{{tc.method, APIPath + tc.path, tc.status}}
			if got := f.CallLog()[before:]; !slices.Equal(got, want) {
				t.Fatalf("call log: got %v, want %v", got, want)
			}
		})
	}

	t.Run("a request without the admin token is logged as a violation under its escaped path", func(t *testing.T) {
		t.Parallel()
		f := New()
		f.ProtectedLandBranch = true
		f.AddRepo(fixtureRepo)
		if status, _ := f.Send(t, "", http.MethodDelete, ruleRoute(fixtureLandRule), ""); status != http.StatusUnauthorized {
			t.Fatalf("DELETE without the admin token: status %d, want 401", status)
		}
		want := []LoggedCall{{http.MethodDelete, APIPath + ruleRoute(fixtureLandRule), http.StatusUnauthorized}}
		if got := f.Violations(); !slices.Equal(got, want) {
			t.Fatalf("violations: got %v, want %v", got, want)
		}
	})
}

// TestFakeRepoSwitches checks that each switch changes exactly the behaviour it names:
// the same requests are made with the switch off and on, and only the named answers
// differ.
func TestFakeRepoSwitches(t *testing.T) {
	t.Parallel()

	t.Run("RepoMissing answers every repository route 404", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			missing bool
			want    []int
		}{
			{"off: the rule list and a collaborator PUT both answer", false, []int{http.StatusOK, http.StatusNoContent}},
			{"on: the same two routes answer 404 for a held repository", true, []int{http.StatusNotFound, http.StatusNotFound}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := New()
				f.AddRepo(fixtureRepo)
				f.RepoMissing = tc.missing
				rules, _ := f.Call(t, http.MethodGet, rulesRoute(), "")
				collab, _ := f.Call(t, http.MethodPut, collaboratorRoute(fixtureBot), JSONBody(map[string]string{"permission": fixturePermission}))
				if got := []int{rules, collab}; !slices.Equal(got, tc.want) {
					t.Fatalf("statuses %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("ProtectedLandBranch seeds a land/* rule on an added repository", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name      string
			protected bool
			wantRules int
			wantLand  int
		}{
			{"off: no rule, and DELETE of land/* is 404", false, 0, http.StatusNotFound},
			{"on: one land/* rule, and DELETE of land/* is 204", true, 1, http.StatusNoContent},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := New()
				f.ProtectedLandBranch = tc.protected
				f.AddRepo(fixtureRepo)
				if got := listedRules(t, f); len(got) != tc.wantRules {
					t.Fatalf("rules listed: %d, want %d", len(got), tc.wantRules)
				}
				if status, _ := f.Call(t, http.MethodDelete, ruleRoute(fixtureLandRule), ""); status != tc.wantLand {
					t.Fatalf("DELETE of %s: status %d, want %d", fixtureLandRule, status, tc.wantLand)
				}
			})
		}
	})

	t.Run("DropWhitelistEntry leaves a PATCH's merge whitelist as it was", func(t *testing.T) {
		t.Parallel()
		landing := []string{fixtureLandingBot}
		cases := []struct {
			name string
			drop bool
			want []string
		}{
			{"off: the PATCH sets the merge whitelist", false, landing},
			{"on: the PATCH's merge whitelist does not take", true, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := New()
				f.DropWhitelistEntry = tc.drop
				f.AddRepo(fixtureRepo)
				if status, _ := f.Call(t, http.MethodPost, rulesRoute(), JSONBody(RuleBody(fixtureMainRule, nil))); status != http.StatusCreated {
					t.Fatalf("create %s: status %d, want 201", fixtureMainRule, status)
				}
				body := JSONBody(map[string]any{"merge_whitelist_usernames": landing})
				if status, _ := f.Call(t, http.MethodPatch, ruleRoute(fixtureMainRule), body); status != http.StatusOK {
					t.Fatalf("PATCH of %s: status %d, want 200", fixtureMainRule, status)
				}
				got := ruleNamed(t, listedRules(t, f), fixtureMainRule).MergeWhitelistUsernames
				if !slices.Equal(got, tc.want) {
					t.Fatalf("merge whitelist after the PATCH: %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("ViewerExists answers the viewer bot's GET 200 and no other bot's", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			exists bool
			want   []int
		}{
			{"off: the viewer bot and the polecat bot are both 404", false, []int{http.StatusNotFound, http.StatusNotFound}},
			{"on: the viewer bot is 200 and the polecat bot is still 404", true, []int{http.StatusOK, http.StatusNotFound}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := New()
				f.ViewerExists = tc.exists
				viewer, _ := f.Call(t, http.MethodGet, "/users/"+fixtureViewerBot, "")
				polecat, _ := f.Call(t, http.MethodGet, "/users/"+fixtureBot, "")
				if got := []int{viewer, polecat}; !slices.Equal(got, tc.want) {
					t.Fatalf("statuses %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("ViewerExists stores the viewer bot, so its tokens list and its create is refused", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name       string
			exists     bool
			wantTokens int
			wantCreate int
		}{
			{"off: the viewer's tokens are 404 and its create is 201", false, http.StatusNotFound, http.StatusCreated},
			{"on: the viewer's tokens list 200 and its create is 422", true, http.StatusOK, http.StatusUnprocessableEntity},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := New()
				f.ViewerExists = tc.exists
				tokens, _ := f.Call(t, http.MethodGet, tokensRoute(fixtureViewerBot), "")
				create, _ := f.Call(t, http.MethodPost, "/admin/users", JSONBody(UserBody(fixtureViewerBot)))
				if got := []int{tokens, create}; !slices.Equal(got, []int{tc.wantTokens, tc.wantCreate}) {
					t.Fatalf("statuses %v, want %v", got, []int{tc.wantTokens, tc.wantCreate})
				}
			})
		}
	})

	t.Run("Adminless refuses only the token mint", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name      string
			Adminless bool
			want      []int
		}{
			{"off: the mint is 201", false, []int{http.StatusCreated, http.StatusNoContent, http.StatusOK}},
			{"on: the mint is 403, and the repository routes are unchanged", true, []int{http.StatusForbidden, http.StatusNoContent, http.StatusOK}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := New()
				f.Adminless = tc.Adminless
				f.AddRepo(fixtureRepo)
				if status, _ := f.Call(t, http.MethodPost, "/admin/users", JSONBody(UserBody(fixtureBot))); status != http.StatusCreated {
					t.Fatalf("create %s: status %d, want 201", fixtureBot, status)
				}
				mint := JSONBody(map[string]any{"name": fixtureTokenName, "scopes": []string{fixtureScope}})
				mintStatus, _ := f.Call(t, http.MethodPost, tokensRoute(fixtureBot), mint)
				grant, _ := f.Call(t, http.MethodPut, collaboratorRoute(fixtureBot), JSONBody(map[string]string{"permission": fixturePermission}))
				rules, _ := f.Call(t, http.MethodGet, rulesRoute(), "")
				if got := []int{mintStatus, grant, rules}; !slices.Equal(got, tc.want) {
					t.Fatalf("statuses %v, want %v", got, tc.want)
				}
			})
		}
	})
}
