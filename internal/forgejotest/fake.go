package forgejotest

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// TB is the part of testing.TB the helpers use. It is declared here so this package does not
// import testing; *testing.T satisfies it.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// AdminToken is the one admin token the fake accepts: a placeholder, never a real credential.
const AdminToken = "fixture-admin-7f3a9c"

// botPrefix and botEmailDomain are the bot naming defaults the provisioning run uses. The
// fake keeps its own copy, because this package cannot import the provisioner it serves;
// TestFakeMirrorsProductionDefaults holds the two together.
const (
	botPrefix      = "bot-"
	botEmailDomain = "bots.invalid"
)

// APIRoot is the host the fake answers for. Its name is reserved (.invalid),
// so a request that escaped the fake could not reach a real server.
const APIRoot = "http://forgejo.fake.invalid"

// APIPath is the API root every route sits under, as the live server has it.
const APIPath = "/api/v1"

// LoggedCall is one request the fake answered: its method, its escaped path (never the
// query or the Authorization header) and the status it sent back.
type LoggedCall struct {
	Method string
	Path   string
	Status int
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

// Fake is an in-process Forgejo: a stateful http.RoundTripper that answers
// the admin routes the provisioning run calls, and refuses what the live server
// refuses. It opens no listening socket and runs no subprocess.
type Fake struct {
	mu sync.Mutex
	// adminToken is the one value the Authorization header may carry.
	adminToken string
	// Adminless makes token creation answer 403, as a token that is not a site
	// administrator's does.
	Adminless bool
	// TokenField names the JSON field a create-token call returns the new value
	// in: "sha1", or "token" for a version that renames it.
	TokenField string
	// NewToken is the random seam: it returns the value of the next token minted.
	NewToken func() string
	// RepoMissing makes every repository route answer 404, as a token that cannot see
	// the repository does, whether or not the fake holds it.
	RepoMissing bool
	// DropWhitelistEntry makes a branch-protection write leave merge_whitelist_usernames
	// as it was (a PATCH) or empty (a POST), so the write does not take.
	DropWhitelistEntry bool
	// ProtectedLandBranch makes each repository the fake adds start with a land/* rule.
	ProtectedLandBranch bool
	// ViewerExists makes the viewer bot exist before any create call, as on an instance
	// a viewer run has already provisioned.
	ViewerExists bool

	users          map[string]*fakeUser
	repos          map[string]*fakeRepo
	calls          []LoggedCall
	violationCalls []LoggedCall
}

// New returns an empty fake that accepts AdminToken and returns the
// new token's value in the "sha1" field. The random seam counts up from 1, so a
// test reads the same values on every run.
func New() *Fake {
	minted := 0
	return &Fake{
		adminToken: AdminToken,
		TokenField: "sha1",
		NewToken: func() string {
			minted++
			return fmt.Sprintf("%040x", minted)
		},
		users: map[string]*fakeUser{},
		repos: map[string]*fakeRepo{},
	}
}

// RoundTrip answers one request in-process. A request that does not carry the admin
// token is answered 401 and recorded as a violation; an authorized one is served.
func (f *Fake) RoundTrip(req *http.Request) (*http.Response, error) {
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
	call := LoggedCall{Method: req.Method, Path: req.URL.EscapedPath()}
	var reply string
	if req.Header.Get("Authorization") == "token "+f.adminToken {
		call.Status, reply = f.serve(call.Method, call.Path, body)
	} else {
		call.Status, reply = unauthorized()
		f.violationCalls = append(f.violationCalls, call)
	}
	f.calls = append(f.calls, call)
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", call.Status, http.StatusText(call.Status)),
		StatusCode: call.Status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(reply)),
		Request:    req,
	}, nil
}

// serve answers an authorized request from the fake's state, with the status and
// body the live server sends. A route the fake does not know is a 404.
func (f *Fake) serve(method, path string, body []byte) (int, string) {
	f.seedViewer()
	route, ok := strings.CutPrefix(path, APIPath+"/")
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

// seedViewer stores the viewer bot when ViewerExists is set, so every route agrees that it
// exists: a GET answers it, a create is refused, and its tokens are listed. It runs before
// each authorized request, so a reset brings the bot back for the next one.
func (f *Fake) seedViewer() {
	if !f.ViewerExists {
		return
	}
	name := botPrefix + "viewer"
	if _, ok := f.users[name]; !ok {
		f.users[name] = &fakeUser{email: name + "@" + botEmailDomain, tokens: map[string]fakeToken{}}
	}
}

// getUser answers whether a bot exists, with its email when it does.
func (f *Fake) getUser(name string) (int, string) {
	u, ok := f.users[name]
	if !ok {
		return refusal(http.StatusNotFound, "user does not exist")
	}
	return reply(http.StatusOK, map[string]string{"username": name, "email": u.email})
}

// createUser makes a bot account. The body must carry a username, an email, a
// password and must_change_password false; a repeat create is 422.
func (f *Fake) createUser(body []byte) (int, string) {
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
func (f *Fake) listTokens(user string) (int, string) {
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
// so an Adminless fake answers 403. A body with no name or no scopes, a scope no
// role uses, a repository scope with write:admin, and a name the user already has
// are all 422, and mint nothing.
func (f *Fake) mintToken(user string, body []byte) (int, string) {
	if f.Adminless {
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
	value := f.NewToken()
	u.tokens[in.Name] = fakeToken{scopes: in.Scopes, value: value}
	return reply(http.StatusCreated, map[string]any{"name": in.Name, "scopes": in.Scopes, f.TokenField: value})
}

// knownScopes is the token scopes the fake accepts, written out rather than read from
// the production role table: the table is the thing under test, so the fake must not
// agree with it by construction. forgejoprov's TestFakeKnowsEveryRoleScope holds the two together.
var knownScopes = []string{"read:repository", "read:user", "write:repository", "read:package", "write:package"}

// repositoryScopes are the token scopes that reach a repository. The live server refuses
// one of them minted together with write:admin.
var repositoryScopes = []string{"read:repository", "write:repository"}

// refusalRepositoryAdmin is the reason the fake gives for a repository scope minted with
// write:admin. It is a rule of its own, so that pair is refused for this reason and not
// only because write:admin is an unknown scope.
const refusalRepositoryAdmin = "a repository scope cannot be minted with write:admin"

// refusedScopes names why a scope list is refused, or returns "" when it is allowed. A
// repository scope with write:admin is refused by its own rule first. Every other scope
// must be one in knownScopes, so write:admin alone, which no role uses, is refused as an
// unknown scope wherever it sits in the list.
func refusedScopes(scopes []string) string {
	hasRepository := slices.ContainsFunc(scopes, func(s string) bool { return slices.Contains(repositoryScopes, s) })
	if hasRepository && slices.Contains(scopes, "write:admin") {
		return refusalRepositoryAdmin
	}
	for _, s := range scopes {
		if !slices.Contains(knownScopes, s) {
			return fmt.Sprintf("scope %s is not one the roles use", s)
		}
	}
	return ""
}

// revokeToken deletes one token by name: 204, or 404 when the user or the token is
// absent.
func (f *Fake) revokeToken(user, name string) (int, string) {
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

// CallLog returns a copy of every call, in the order the fake answered them.
func (f *Fake) CallLog() []LoggedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Writes returns the calls other than GET, in order.
func (f *Fake) Writes() []LoggedCall {
	var out []LoggedCall
	for _, c := range f.CallLog() {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// Violations returns the requests that did not carry the admin token, in order.
func (f *Fake) Violations() []LoggedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.violationCalls)
}

// Reset forgets every user, repository, token, call and violation. The admin token and
// the seams are kept.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = map[string]*fakeUser{}
	f.repos = map[string]*fakeRepo{}
	f.calls = nil
	f.violationCalls = nil
}

// Token returns the value minted for the user's token name, or "" when there is none.
func (f *Fake) Token(user, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[user]
	if !ok {
		return ""
	}
	return u.tokens[name].value
}

// Send makes one request through the fake, with the Authorization header value given
// (empty sends none), and returns the status and the body. path is relative to the
// API root, as the provisioning run writes it: "/users/bot-polecat".
func (f *Fake) Send(t TB, auth, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, APIRoot+APIPath+path, strings.NewReader(body))
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

// Call sends one request carrying the admin token, as the provisioning run does.
func (f *Fake) Call(t TB, method, path, body string) (int, string) {
	t.Helper()
	return f.Send(t, "token "+f.adminToken, method, path, body)
}

// JSONBody encodes a request body. The fixtures are plain maps and always marshal.
func JSONBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// UserBody is the create-user body the provisioning run sends for a bot.
func UserBody(name string) map[string]any {
	return map[string]any{
		"username":             name,
		"email":                name + "@" + botEmailDomain,
		"password":             "fixture-password-0c1e",
		"must_change_password": false,
		"send_notify":          false,
	}
}

// KnownScopes returns the token scopes the fake accepts, as a copy the caller may keep.
func KnownScopes() []string {
	return slices.Clone(knownScopes)
}

// SeedUser stores a bot account, as an earlier run would have left it, with no tokens.
func (f *Fake) SeedUser(name, email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[name] = &fakeUser{email: email, tokens: map[string]fakeToken{}}
}

// UserEmail returns the email of the named bot and whether the fake holds that bot.
func (f *Fake) UserEmail(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok {
		return "", false
	}
	return u.email, true
}

// MintedValues returns every token value the fake holds, in no particular order.
func (f *Fake) MintedValues() []string {
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

// ClearCalls forgets the call log, so a case can count only the calls it sends next.
func (f *Fake) ClearCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// TokenScopes returns the scopes of the token the user holds under name, and whether the
// fake holds one.
func (f *Fake) TokenScopes(user, name string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[user]
	if !ok {
		return nil, false
	}
	tok, ok := u.tokens[name]
	return tok.scopes, ok
}
