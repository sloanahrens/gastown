package forgejoprov

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The fixtures name a bot the way the provisioning run does: the default prefix and
// the polecat role, with the default token name and the scope the polecat role uses.
const (
	fixtureBot       = DefaultBotPrefix + "polecat"
	fixtureTokenName = DefaultTokenName
	fixtureScope     = "write:repository"
	// fixtureWrongToken stands in for a credential that is not the admin token.
	fixtureWrongToken = "attacker-guess-5d2e"
)

// tokensRoute is the admin route that lists and mints a user's tokens.
func tokensRoute(user string) string {
	return "/admin/users/" + user + "/tokens"
}

// TestFakeUsersAndTokens creates a bot, mints and lists its token, and deletes it,
// through the routes the provisioning run calls, and checks each answer. It runs once
// for each field a create-token call may return the new value in.
func TestFakeUsersAndTokens(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"sha1", "token"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			f.tokenField = field
			tokens := tokensRoute(fixtureBot)
			tokenPath := tokens + "/" + fixtureTokenName
			mintBody := jsonBody(map[string]any{"name": fixtureTokenName, "scopes": []string{fixtureScope}})

			if status, _ := f.call(t, http.MethodGet, "/users/"+fixtureBot, ""); status != http.StatusNotFound {
				t.Fatalf("GET of a bot before it exists: status %d, want 404", status)
			}
			if status, _ := f.call(t, http.MethodGet, tokens, ""); status != http.StatusNotFound {
				t.Fatalf("GET of tokens for a user that does not exist: status %d, want 404", status)
			}
			if status, _ := f.call(t, http.MethodPost, "/admin/users", jsonBody(userMap(fixtureBot))); status != http.StatusCreated {
				t.Fatalf("create %s: status %d, want 201", fixtureBot, status)
			}
			if status, _ := f.call(t, http.MethodGet, "/users/"+fixtureBot, ""); status != http.StatusOK {
				t.Fatalf("GET of an existing bot: status %d, want 200", status)
			}
			if status, _ := f.call(t, http.MethodPost, "/admin/users", jsonBody(userMap(fixtureBot))); status != http.StatusUnprocessableEntity {
				t.Fatalf("repeat create of %s: status %d, want 422", fixtureBot, status)
			}
			if status, body := f.call(t, http.MethodGet, tokens, ""); status != http.StatusOK || body != "[]" {
				t.Fatalf("list of a new bot's tokens: status %d body %q, want 200 []", status, body)
			}

			status, body := f.call(t, http.MethodPost, tokens, mintBody)
			if status != http.StatusCreated {
				t.Fatalf("mint %s: status %d, want 201", fixtureTokenName, status)
			}
			var minted map[string]any
			if err := json.Unmarshal([]byte(body), &minted); err != nil {
				t.Fatalf("mint reply is not JSON: %v", err)
			}
			value, _ := minted[field].(string)
			if value == "" {
				t.Fatalf("mint reply carries no %q field", field)
			}
			if got := f.token(fixtureBot, fixtureTokenName); got != value {
				t.Fatalf("the fake holds a token value other than the one the mint returned")
			}
			if status, _ := f.call(t, http.MethodPost, tokens, mintBody); status != http.StatusUnprocessableEntity {
				t.Fatalf("mint of a name the bot already has: status %d, want 422", status)
			}

			status, body = f.call(t, http.MethodGet, tokens, "")
			var list []struct {
				Name   string   `json:"name"`
				Scopes []string `json:"scopes"`
			}
			if err := json.Unmarshal([]byte(body), &list); err != nil {
				t.Fatalf("token list is not JSON: %v", err)
			}
			if status != http.StatusOK || len(list) != 1 || list[0].Name != fixtureTokenName || !slices.Equal(list[0].Scopes, []string{fixtureScope}) {
				t.Fatalf("token list: status %d, %+v; want 200 with one %s token of scope %s", status, list, fixtureTokenName, fixtureScope)
			}

			if status, _ := f.call(t, http.MethodDelete, tokenPath, ""); status != http.StatusNoContent {
				t.Fatalf("delete %s: status %d, want 204", fixtureTokenName, status)
			}
			if status, _ := f.call(t, http.MethodDelete, tokenPath, ""); status != http.StatusNotFound {
				t.Fatalf("delete of a token already gone: status %d, want 404", status)
			}
			if got := f.token(fixtureBot, fixtureTokenName); got != "" {
				t.Fatalf("a deleted token still holds a value")
			}

			want := []fakeCall{
				{http.MethodPost, fakeAPIPath + "/admin/users", http.StatusCreated},
				{http.MethodPost, fakeAPIPath + "/admin/users", http.StatusUnprocessableEntity},
				{http.MethodPost, fakeAPIPath + tokens, http.StatusCreated},
				{http.MethodPost, fakeAPIPath + tokens, http.StatusUnprocessableEntity},
				{http.MethodDelete, fakeAPIPath + tokenPath, http.StatusNoContent},
				{http.MethodDelete, fakeAPIPath + tokenPath, http.StatusNotFound},
			}
			if got := f.writes(); !slices.Equal(got, want) {
				t.Fatalf("writes: got %v, want %v", got, want)
			}
		})
	}
}

// TestFakeRequiresAuthorization checks that a request without the admin token is 401
// and is recorded as a violation, and that an authorized request is recorded as none.
func TestFakeRequiresAuthorization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		auth string
	}{
		{"no Authorization header", ""},
		{"a token that is not the admin token", "token " + fixtureWrongToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			if status, _ := f.send(t, tc.auth, http.MethodGet, "/users/"+fixtureBot, ""); status != http.StatusUnauthorized {
				t.Fatalf("request %s: status %d, want 401", tc.name, status)
			}
			want := []fakeCall{{http.MethodGet, fakeAPIPath + "/users/" + fixtureBot, http.StatusUnauthorized}}
			if got := f.violations(); !slices.Equal(got, want) {
				t.Fatalf("violations: got %v, want %v", got, want)
			}
		})
	}

	t.Run("an authorized request is not a violation", func(t *testing.T) {
		t.Parallel()
		f := newFakeForgejo()
		if status, _ := f.call(t, http.MethodGet, "/users/"+fixtureBot, ""); status != http.StatusNotFound {
			t.Fatalf("authorized GET of a missing bot: status %d, want 404", status)
		}
		if status, _ := f.call(t, http.MethodPost, "/admin/users", jsonBody(userMap(fixtureBot))); status != http.StatusCreated {
			t.Fatalf("authorized create: status %d, want 201", status)
		}
		if status, _ := f.call(t, http.MethodGet, "/nope", ""); status != http.StatusNotFound {
			t.Fatalf("authorized request to a route the fake lacks: status %d, want 404", status)
		}
		if got := f.violations(); len(got) != 0 {
			t.Fatalf("authorized requests recorded as violations: %v", got)
		}
	})
}

// TestFakeCallLogHidesTheAuthorizationValue checks that no field of the call log holds
// the admin token, a wrong token, or a minted token value, though the calls carried
// them. The log must also hold the calls, so an empty log cannot pass.
func TestFakeCallLogHidesTheAuthorizationValue(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	if status, _ := f.call(t, http.MethodPost, "/admin/users", jsonBody(userMap(fixtureBot))); status != http.StatusCreated {
		t.Fatalf("create %s: status %d, want 201", fixtureBot, status)
	}
	mintBody := jsonBody(map[string]any{"name": fixtureTokenName, "scopes": []string{fixtureScope}})
	if status, _ := f.call(t, http.MethodPost, tokensRoute(fixtureBot), mintBody); status != http.StatusCreated {
		t.Fatalf("mint %s: status %d, want 201", fixtureTokenName, status)
	}
	if status, _ := f.send(t, "token "+fixtureWrongToken, http.MethodGet, "/users/"+fixtureBot, ""); status != http.StatusUnauthorized {
		t.Fatalf("request with a wrong token: status %d, want 401", status)
	}

	log := f.callLog()
	if len(log) != 3 {
		t.Fatalf("call log holds %d calls, want 3", len(log))
	}
	secrets := []string{fixtureToken, fixtureWrongToken, f.token(fixtureBot, fixtureTokenName)}
	if secrets[2] == "" {
		t.Fatalf("no token was minted, so the log has nothing to hide")
	}
	for _, c := range log {
		shown := fmt.Sprintf("%+v %#v", c, c)
		for _, secret := range secrets {
			if strings.Contains(shown, secret) {
				t.Fatalf("call log entry %s %s shows a secret", c.method, c.path)
			}
		}
	}
}

// TestFakeRefusesWhatLiveRefuses checks that a token naming a scope no role uses, or
// a repository scope with write:admin, is 422 and mints nothing, and that the role's own
// scopes are accepted. A refusal must also give its own reason, so the repository pair is
// refused by its rule and not only because write:admin is unknown.
func TestFakeRefusesWhatLiveRefuses(t *testing.T) {
	t.Parallel()
	const unknownWriteAdmin = "scope write:admin is not one the roles use"
	cases := []struct {
		name   string
		scopes []string
		status int
		reason string
	}{
		{"a scope no role uses", []string{"write:admin"}, http.StatusUnprocessableEntity, unknownWriteAdmin},
		{"a repository list with write:admin", []string{"read:repository", "write:admin"}, http.StatusUnprocessableEntity, refusalRepositoryAdmin},
		{"an unused scope after a valid non-repository one", []string{"read:user", "write:admin"}, http.StatusUnprocessableEntity, unknownWriteAdmin},
		{"the scopes of a role", []string{"write:repository", "read:user"}, http.StatusCreated, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			if status, _ := f.call(t, http.MethodPost, "/admin/users", jsonBody(userMap(fixtureBot))); status != http.StatusCreated {
				t.Fatalf("create %s: status %d, want 201", fixtureBot, status)
			}
			mintBody := jsonBody(map[string]any{"name": fixtureTokenName, "scopes": tc.scopes})
			status, body := f.call(t, http.MethodPost, tokensRoute(fixtureBot), mintBody)
			if status != tc.status {
				t.Fatalf("mint with scopes %v: status %d, want %d", tc.scopes, status, tc.status)
			}
			if tc.reason != "" && !strings.Contains(body, tc.reason) {
				t.Fatalf("mint with scopes %v: reply does not give the reason %q", tc.scopes, tc.reason)
			}
			minted := f.token(fixtureBot, fixtureTokenName)
			if tc.status == http.StatusCreated && minted == "" {
				t.Fatalf("an allowed mint left no token")
			}
			if tc.status != http.StatusCreated && minted != "" {
				t.Fatalf("a refused mint left a token behind")
			}
		})
	}
}

// scopeDrift reports how a role table and a known scope list disagree: a role with no
// scope, a scope a role uses that the list lacks, and a listed scope no role uses. An
// empty table is a disagreement too, so the check cannot pass on nothing.
func scopeDrift(table []roleScopes, known []string) []string {
	if len(table) == 0 {
		return []string{"the role table is empty"}
	}
	var problems []string
	used := map[string]bool{}
	for _, r := range table {
		if len(r.scopes) == 0 {
			problems = append(problems, fmt.Sprintf("role %s has no scopes", r.name))
		}
		for _, s := range r.scopes {
			used[s] = true
			if !slices.Contains(known, s) {
				problems = append(problems, fmt.Sprintf("role %s uses scope %s, which the fake does not accept", r.name, s))
			}
		}
	}
	for _, s := range known {
		if !used[s] {
			problems = append(problems, fmt.Sprintf("the fake accepts scope %s, which no role uses", s))
		}
	}
	return problems
}

// TestFakeKnowsEveryRoleScope checks that the fake's scope list and the production role
// table agree both ways: every scope a role uses is one the fake accepts, and every scope
// the fake accepts is one a role uses. The table must not be empty.
func TestFakeKnowsEveryRoleScope(t *testing.T) {
	t.Parallel()
	table := roleTable()
	if len(table) == 0 {
		t.Fatalf("the production role table is empty")
	}
	if problems := scopeDrift(table, knownScopes); len(problems) != 0 {
		t.Errorf("the fake and the role table disagree: %v", problems)
	}
}

// TestScopeDriftReports drives each way the role table and the known list can disagree,
// and the agreeing case, so the check is seen to report a problem and to pass when there
// is none.
func TestScopeDriftReports(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		table []roleScopes
		known []string
		want  string
	}{
		{"agreeing table and list", []roleScopes{{"polecat", []string{"write:repository"}}}, []string{"write:repository"}, ""},
		{"a role uses a scope the list lacks", []roleScopes{{"admin", []string{"write:admin"}}}, []string{"write:repository"}, "role admin uses scope write:admin"},
		{"the list holds a scope no role uses", []roleScopes{{"polecat", []string{"write:repository"}}}, []string{"write:repository", "read:user"}, "scope read:user, which no role uses"},
		{"a role with no scopes", []roleScopes{{"empty", nil}}, []string{"write:repository"}, "role empty has no scopes"},
		{"an empty role table", nil, []string{"write:repository"}, "the role table is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := scopeDrift(tc.table, tc.known)
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("agreeing table reported problems: %v", problems)
				}
				return
			}
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Fatalf("problems %v do not name %q", problems, tc.want)
			}
		})
	}
}

// TestFakeRefusesAnIncompleteUserBody checks that a create-user body missing a field
// the live server requires, or asking for a password change, is 422 and creates no bot.
func TestFakeRefusesAnIncompleteUserBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(map[string]any)
	}{
		{"no username", func(m map[string]any) { delete(m, "username") }},
		{"no email", func(m map[string]any) { delete(m, "email") }},
		{"no password", func(m map[string]any) { delete(m, "password") }},
		{"must_change_password true", func(m map[string]any) { m["must_change_password"] = true }},
		{"must_change_password absent", func(m map[string]any) { delete(m, "must_change_password") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeForgejo()
			body := userMap(fixtureBot)
			tc.edit(body)
			if status, _ := f.call(t, http.MethodPost, "/admin/users", jsonBody(body)); status != http.StatusUnprocessableEntity {
				t.Fatalf("create with %s: status %d, want 422", tc.name, status)
			}
			if status, _ := f.call(t, http.MethodGet, "/users/"+fixtureBot, ""); status != http.StatusNotFound {
				t.Fatalf("a refused create left the bot in place: GET status %d, want 404", status)
			}
		})
	}
}

// TestFakeAdminlessMintIsForbidden checks that a fake set adminless answers a token
// create 403 and mints nothing, while creating a user is still allowed.
func TestFakeAdminlessMintIsForbidden(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.adminless = true
	if status, _ := f.call(t, http.MethodPost, "/admin/users", jsonBody(userMap(fixtureBot))); status != http.StatusCreated {
		t.Fatalf("create %s while adminless: status %d, want 201", fixtureBot, status)
	}
	mintBody := jsonBody(map[string]any{"name": fixtureTokenName, "scopes": []string{fixtureScope}})
	if status, _ := f.call(t, http.MethodPost, tokensRoute(fixtureBot), mintBody); status != http.StatusForbidden {
		t.Fatalf("mint while adminless: status %d, want 403", status)
	}
	if got := f.token(fixtureBot, fixtureTokenName); got != "" {
		t.Fatalf("an adminless mint left a token behind")
	}
}

// TestFakeResetForgetsState checks that reset clears the users, the call log and the
// violations, so one fake can serve several cases in turn.
func TestFakeResetForgetsState(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	if status, _ := f.call(t, http.MethodPost, "/admin/users", jsonBody(userMap(fixtureBot))); status != http.StatusCreated {
		t.Fatalf("create %s: status %d, want 201", fixtureBot, status)
	}
	if status, _ := f.send(t, "", http.MethodGet, "/users/"+fixtureBot, ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous request: status %d, want 401", status)
	}
	f.reset()
	if status, _ := f.call(t, http.MethodGet, "/users/"+fixtureBot, ""); status != http.StatusNotFound {
		t.Fatalf("GET of a bot after reset: status %d, want 404", status)
	}
	if got := f.violations(); len(got) != 0 {
		t.Fatalf("violations survived reset: %v", got)
	}
	if got := f.callLog(); len(got) != 1 {
		t.Fatalf("call log after reset holds %d calls, want the 1 made since", len(got))
	}
}
