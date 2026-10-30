package forgejoprov

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeAPIRoot is the host the fake answers for. Its name is reserved (.invalid),
// so a request that escaped the fake could not reach a real server.
const fakeAPIRoot = "http://forgejo.fake.invalid"

// fakeAPIPath is the API root every route sits under, as the live server has it.
const fakeAPIPath = "/api/v1"

// fakeCall is one request the fake answered: its method, its escaped path (never the
// query or the Authorization header) and the status it sent back.
type fakeCall struct {
	method string
	path   string
	status int
}

// fakeToken is a token minted through the fake: its scopes, and the value the
// create call returned once.
type fakeToken struct {
	scopes []string
	value  string
}

// fakeUser is a bot account: its email, and its tokens by name.
type fakeUser struct {
	email  string
	tokens map[string]fakeToken
}

// fakeForgejo is an in-process Forgejo: a stateful http.RoundTripper that answers
// the admin routes the provisioning run calls, and refuses what the live server
// refuses. It opens no listening socket and runs no subprocess.
type fakeForgejo struct {
	mu sync.Mutex
	// adminToken is the one value the Authorization header may carry.
	adminToken string
	// adminless makes token creation answer 403, as a token that is not a site
	// administrator's does.
	adminless bool
	// tokenField names the JSON field a create-token call returns the new value
	// in: "sha1", or "token" for a version that renames it.
	tokenField string
	// newToken is the random seam: it returns the value of the next token minted.
	newToken func() string
	// repoMissing makes every repository route answer 404, as a token that cannot see
	// the repository does, whether or not the fake holds it.
	repoMissing bool
	// dropWhitelistEntry makes a branch-protection PATCH leave merge_whitelist_usernames
	// as it was, so the write does not take.
	dropWhitelistEntry bool
	// protectedLandBranch makes each repository the fake adds start with a land/* rule.
	protectedLandBranch bool
	// viewerExists makes the viewer bot exist before any create call, as on an instance
	// a viewer run has already provisioned.
	viewerExists bool

	users          map[string]*fakeUser
	repos          map[string]*fakeRepo
	calls          []fakeCall
	violationCalls []fakeCall
}

// newFakeForgejo returns an empty fake that accepts fixtureToken and returns the
// new token's value in the "sha1" field. The random seam counts up from 1, so a
// test reads the same values on every run.
func newFakeForgejo() *fakeForgejo {
	minted := 0
	return &fakeForgejo{
		adminToken: fixtureToken,
		tokenField: "sha1",
		newToken: func() string {
			minted++
			return fmt.Sprintf("%040x", minted)
		},
		users: map[string]*fakeUser{},
		repos: map[string]*fakeRepo{},
	}
}

// RoundTrip answers one request in-process. A request that does not carry the admin
// token is answered 401 and recorded as a violation; an authorized one is served.
func (f *fakeForgejo) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		data, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = data
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// The escaped path is routed on: a rule name's "/" arrives as %2F, which Path decodes.
	call := fakeCall{method: req.Method, path: req.URL.EscapedPath()}
	var reply string
	if req.Header.Get("Authorization") == "token "+f.adminToken {
		call.status, reply = f.serve(call.method, call.path, body)
	} else {
		call.status, reply = unauthorized()
		f.violationCalls = append(f.violationCalls, call)
	}
	f.calls = append(f.calls, call)
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", call.status, http.StatusText(call.status)),
		StatusCode: call.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(reply)),
		Request:    req,
	}, nil
}

// serve answers an authorized request from the fake's state, with the status and
// body the live server sends. A route the fake does not know is a 404.
func (f *fakeForgejo) serve(method, path string, body []byte) (int, string) {
	route, ok := strings.CutPrefix(path, fakeAPIPath+"/")
	if !ok {
		return notFound()
	}
	parts := strings.Split(route, "/")
	switch {
	case method == http.MethodGet && len(parts) == 2 && parts[0] == "users":
		return f.getUser(parts[1])
	case method == http.MethodPost && route == "admin/users":
		return f.createUser(body)
	case len(parts) == 4 && parts[0] == "admin" && parts[1] == "users" && parts[3] == "tokens":
		switch method {
		case http.MethodGet:
			return f.listTokens(parts[2])
		case http.MethodPost:
			return f.mintToken(parts[2], body)
		}
	case len(parts) == 5 && parts[0] == "admin" && parts[1] == "users" && parts[3] == "tokens" && method == http.MethodDelete:
		return f.revokeToken(parts[2], parts[4])
	case parts[0] == "repos":
		return f.serveRepo(method, parts[1:], body)
	}
	return notFound()
}

// getUser answers whether a bot exists, with its email when it does. The viewer bot
// exists before any create call when viewerExists is set.
func (f *fakeForgejo) getUser(name string) (int, string) {
	u, ok := f.users[name]
	if !ok && f.viewerExists && name == DefaultBotPrefix+"viewer" {
		return reply(http.StatusOK, map[string]string{"username": name, "email": name + "@" + DefaultBotEmailDomain})
	}
	if !ok {
		return refusal(http.StatusNotFound, "user does not exist")
	}
	return reply(http.StatusOK, map[string]string{"username": name, "email": u.email})
}

// createUser makes a bot account. The body must carry a username, an email, a
// password and must_change_password false; a repeat create is 422.
func (f *fakeForgejo) createUser(body []byte) (int, string) {
	var in struct {
		Username           string `json:"username"`
		Email              string `json:"email"`
		Password           string `json:"password"`
		MustChangePassword *bool  `json:"must_change_password"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Username == "" || in.Email == "" || in.Password == "" || in.MustChangePassword == nil || *in.MustChangePassword {
		return refusal(http.StatusUnprocessableEntity, "the body must carry username, email, password and must_change_password false")
	}
	if _, exists := f.users[in.Username]; exists {
		return refusal(http.StatusUnprocessableEntity, "user already exists")
	}
	f.users[in.Username] = &fakeUser{email: in.Email, tokens: map[string]fakeToken{}}
	return reply(http.StatusCreated, map[string]string{"username": in.Username, "email": in.Email})
}

// listTokens lists a user's tokens by name and scopes. The values are never listed.
func (f *fakeForgejo) listTokens(user string) (int, string) {
	u, ok := f.users[user]
	if !ok {
		return refusal(http.StatusNotFound, "user does not exist")
	}
	out := []map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(u.tokens)) {
		out = append(out, map[string]any{"name": name, "scopes": u.tokens[name].scopes})
	}
	return reply(http.StatusOK, out)
}

// mintToken creates a token for a user. Only a site administrator's token may do it,
// so an adminless fake answers 403. A body with no name or no scopes, a scope no
// role uses, a repository scope with write:admin, and a name the user already has
// are all 422, and mint nothing.
func (f *fakeForgejo) mintToken(user string, body []byte) (int, string) {
	if f.adminless {
		return refusal(http.StatusForbidden, "the token needs site-administrator rights to mint a token")
	}
	u, ok := f.users[user]
	if !ok {
		return refusal(http.StatusNotFound, "user does not exist")
	}
	var in struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Name == "" || len(in.Scopes) == 0 {
		return refusal(http.StatusUnprocessableEntity, "the body must carry a token name and at least one scope")
	}
	if why := refusedScopes(in.Scopes); why != "" {
		return refusal(http.StatusUnprocessableEntity, why)
	}
	if _, taken := u.tokens[in.Name]; taken {
		return refusal(http.StatusUnprocessableEntity, "access token name has been used already")
	}
	value := f.newToken()
	u.tokens[in.Name] = fakeToken{scopes: in.Scopes, value: value}
	return reply(http.StatusCreated, map[string]any{"name": in.Name, "scopes": in.Scopes, f.tokenField: value})
}

// refusedScopes names why a scope list is refused, or returns "" when it is allowed.
// Every scope must be one a role uses, so write:admin, which no role uses, is refused
// too, and so is a repository list that names it. roleTable is the production table,
// so the fake cannot drift from it.
func refusedScopes(scopes []string) string {
	allowed := map[string]bool{}
	for _, r := range roleTable() {
		for _, s := range r.scopes {
			allowed[s] = true
		}
	}
	for _, s := range scopes {
		if !allowed[s] {
			return fmt.Sprintf("scope %s is not one the roles use", s)
		}
	}
	return ""
}

// revokeToken deletes one token by name: 204, or 404 when the user or the token is
// absent.
func (f *fakeForgejo) revokeToken(user, name string) (int, string) {
	u, ok := f.users[user]
	if !ok {
		return refusal(http.StatusNotFound, "user does not exist")
	}
	if _, ok := u.tokens[name]; !ok {
		return refusal(http.StatusNotFound, "token does not exist")
	}
	delete(u.tokens, name)
	return http.StatusNoContent, ""
}

// reply encodes a successful answer. The values passed here always marshal.
func reply(status int, v any) (int, string) {
	b, _ := json.Marshal(v)
	return status, string(b)
}

// refusal is the JSON error body the live server sends with a refusal.
func refusal(status int, text string) (int, string) {
	return reply(status, map[string]string{"message": text})
}

// unauthorized answers a request that lacks the admin token.
func unauthorized() (int, string) {
	return refusal(http.StatusUnauthorized, "unauthorized: the Authorization header must carry the admin token")
}

// notFound answers a route the fake does not serve.
func notFound() (int, string) {
	return refusal(http.StatusNotFound, "not found")
}

// callLog returns a copy of every call, in the order the fake answered them.
func (f *fakeForgejo) callLog() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// writes returns the calls other than GET, in order.
func (f *fakeForgejo) writes() []fakeCall {
	var out []fakeCall
	for _, c := range f.callLog() {
		if c.method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// violations returns the requests that did not carry the admin token, in order.
func (f *fakeForgejo) violations() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.violationCalls)
}

// reset forgets every user, repository, token, call and violation. The admin token and
// the seams are kept.
func (f *fakeForgejo) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = map[string]*fakeUser{}
	f.repos = map[string]*fakeRepo{}
	f.calls = nil
	f.violationCalls = nil
}

// token returns the value minted for the user's token name, or "" when there is none.
func (f *fakeForgejo) token(user, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[user]
	if !ok {
		return ""
	}
	return u.tokens[name].value
}

// send makes one request through the fake, with the Authorization header value given
// (empty sends none), and returns the status and the body. path is relative to the
// API root, as the provisioning run writes it: "/users/bot-polecat".
func (f *fakeForgejo) send(t *testing.T, auth, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, fakeAPIRoot+fakeAPIPath+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := (&http.Client{Transport: f}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read reply to %s %s: %v", method, path, err)
	}
	return resp.StatusCode, string(data)
}

// call sends one request carrying the admin token, as the provisioning run does.
func (f *fakeForgejo) call(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	return f.send(t, "token "+f.adminToken, method, path, body)
}

// jsonBody encodes a request body. The fixtures are plain maps and always marshal.
func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// userMap is the create-user body the provisioning run sends for a bot.
func userMap(name string) map[string]any {
	return map[string]any{
		"username":             name,
		"email":                name + "@" + DefaultBotEmailDomain,
		"password":             "fixture-password-0c1e",
		"must_change_password": false,
		"send_notify":          false,
	}
}
