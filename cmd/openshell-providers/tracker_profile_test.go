package main

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The two shipped provider profiles this file reads. The forgejo profile is the
// reference: it is the shape the gateway imports today, so the tracker profile
// is held to it (gt-0f305).
const (
	trackerProfilePath = "../../deploy/polecat/profiles/tracker-polecat.yaml"
	forgejoProfilePath = "../../deploy/polecat/profiles/forgejo-polecat.yaml"
)

// profileFile is the part of an openshell provider profile these tests read.
type profileFile struct {
	ID          string              `yaml:"id"`
	DisplayName string              `yaml:"display_name"`
	Description string              `yaml:"description"`
	Category    string              `yaml:"category"`
	Credentials []profileCredential `yaml:"credentials"`
	Discovery   struct {
		Credentials []string `yaml:"credentials"`
	} `yaml:"discovery"`
	Endpoints []profileEndpoint `yaml:"endpoints"`
	Binaries  []string          `yaml:"binaries"`
}

type profileCredential struct {
	Name       string   `yaml:"name"`
	EnvVars    []string `yaml:"env_vars"`
	Required   bool     `yaml:"required"`
	AuthStyle  string   `yaml:"auth_style"`
	HeaderName string   `yaml:"header_name"`
}

type profileEndpoint struct {
	Host        string        `yaml:"host"`
	Port        int           `yaml:"port"`
	Protocol    string        `yaml:"protocol"`
	Enforcement string        `yaml:"enforcement"`
	Rules       []profileRule `yaml:"rules"`
}

type profileRule struct {
	Allow struct {
		Method string `yaml:"method"`
		Path   string `yaml:"path"`
	} `yaml:"allow"`
}

// httpMethods are the methods a rule may name.
var httpMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}

// trackerTokenPattern is the tracker's token shape (tracker/store/tokens.go), so
// a real token pasted into the profile instead of a placeholder is refused.
var trackerTokenPattern = regexp.MustCompile(`trk_[0-9a-f]{16,}`)

// readProfileSource returns a shipped profile's bytes.
func readProfileSource(t *testing.T, file string) []byte {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	return src
}

// readProfile parses a shipped profile.
func readProfile(t *testing.T, file string) profileFile {
	t.Helper()
	var p profileFile
	if err := yaml.Unmarshal(readProfileSource(t, file), &p); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	return p
}

// TestTrackerPolecatProfileMatchesTheForgejoProfileShape checks the tracker
// profile has the shape forgejo-polecat.yaml has: the same required fields, one
// credential set out the same way, and endpoints carrying rules. It is a
// structural check, not the profile lint. `openshell profile lint -f`, which the
// import command runs before every import (internal/openshellprov/importprofiles.go:158),
// is the authority on the file's grammar; the live run of it stays the
// overseer's proof (gt-0f305).
func TestTrackerPolecatProfileMatchesTheForgejoProfileShape(t *testing.T) {
	t.Parallel()
	ref := readProfile(t, forgejoProfilePath)
	got := readProfile(t, trackerProfilePath)

	// The top-level fields a profile needs, read from the reference so this
	// check follows it rather than restating its grammar.
	for _, f := range []struct{ name, ref, got string }{
		{"id", ref.ID, got.ID},
		{"display_name", ref.DisplayName, got.DisplayName},
		{"description", ref.Description, got.Description},
		{"category", ref.Category, got.Category},
	} {
		if strings.TrimSpace(f.ref) == "" {
			t.Fatalf("%s sets no %s, so it cannot be the reference for it", forgejoProfilePath, f.name)
		}
		if strings.TrimSpace(f.got) == "" {
			t.Errorf("%s sets no %s, and the forgejo profile does", trackerProfilePath, f.name)
		}
	}

	if len(ref.Credentials) == 0 {
		t.Fatalf("%s declares no credential, so it cannot be the reference for one", forgejoProfilePath)
	}
	if len(got.Credentials) != len(ref.Credentials) {
		t.Errorf("%s declares %d credentials, and the forgejo profile declares %d", trackerProfilePath, len(got.Credentials), len(ref.Credentials))
	}
	declared := make(map[string]bool, len(got.Credentials))
	for i, c := range got.Credentials {
		where := fmt.Sprintf("credentials[%d]", i)
		if strings.TrimSpace(c.Name) == "" {
			t.Errorf("%s has no name", where)
		}
		declared[c.Name] = true
		if len(c.EnvVars) == 0 {
			t.Errorf("%s names no env_var", where)
		}
		if !c.Required {
			t.Errorf("%s is not required, so a sandbox without the token would start", where)
		}
		if strings.TrimSpace(c.AuthStyle) == "" || strings.TrimSpace(c.HeaderName) == "" {
			t.Errorf("%s sets auth_style %q and header_name %q, and the forgejo profile sets both", where, c.AuthStyle, c.HeaderName)
		}
	}
	if len(got.Discovery.Credentials) == 0 {
		t.Errorf("%s discovery names no credential, and the forgejo profile does", trackerProfilePath)
	}
	for _, name := range got.Discovery.Credentials {
		if !declared[name] {
			t.Errorf("discovery names %q, which no credential declares", name)
		}
	}

	if len(ref.Endpoints) == 0 {
		t.Fatalf("%s declares no endpoint, so it cannot be the reference for one", forgejoProfilePath)
	}
	if len(got.Endpoints) == 0 {
		t.Errorf("%s declares no endpoint", trackerProfilePath)
	}
	for i, e := range got.Endpoints {
		where := fmt.Sprintf("endpoints[%d]", i)
		if strings.TrimSpace(e.Host) == "" {
			t.Errorf("%s has no host", where)
		}
		if e.Port < 1 || e.Port > 65535 {
			t.Errorf("%s port %d is not 1-65535", where, e.Port)
		}
		if strings.TrimSpace(e.Protocol) == "" {
			t.Errorf("%s has no protocol", where)
		}
		if e.Enforcement != ref.Endpoints[0].Enforcement {
			t.Errorf("%s enforcement is %q, and the forgejo profile's is %q: an endpoint the supervisor does not inspect hands the token to every request on it", where, e.Enforcement, ref.Endpoints[0].Enforcement)
		}
		if e.Protocol == "rest" && len(e.Rules) == 0 {
			t.Errorf("%s is a rest endpoint with no rules, and the forgejo profile's carries one", where)
		}
		for j, r := range e.Rules {
			rw := fmt.Sprintf("%s.rules[%d]", where, j)
			if !slices.Contains(httpMethods, r.Allow.Method) {
				t.Errorf("%s method %q is not an HTTP method", rw, r.Allow.Method)
			}
			if !strings.HasPrefix(r.Allow.Path, "/") {
				t.Errorf("%s path %q is not an absolute path", rw, r.Allow.Path)
			}
		}
	}

	if len(got.Binaries) == 0 {
		t.Errorf("%s declares no binary, and the forgejo profile does", trackerProfilePath)
	}
	for _, b := range got.Binaries {
		if !strings.HasPrefix(b, "/") {
			t.Errorf("binary %q is not an absolute path", b)
		}
	}
}

// TestTrackerPolecatProfileCarriesTheSeatToken holds the file to the contract
// it exists for: one TRACKER_TOKEN sent as the authorization header to the
// tracker through the control-plane alias, with no token in the file and no GT_
// environment read (gt-0f305).
func TestTrackerPolecatProfileCarriesTheSeatToken(t *testing.T) {
	t.Parallel()
	p := readProfile(t, trackerProfilePath)
	src := readProfileSource(t, trackerProfilePath)

	if p.ID != "tracker-polecat" {
		t.Errorf("id = %q, want tracker-polecat: the gateway keys the profile and its providers on it", p.ID)
	}
	if len(p.Credentials) != 1 || !slices.Equal(p.Credentials[0].EnvVars, []string{"TRACKER_TOKEN"}) {
		t.Fatalf("credentials = %+v, want one credential holding exactly TRACKER_TOKEN", p.Credentials)
	}
	if c := p.Credentials[0]; c.AuthStyle != "bearer" || c.HeaderName != "authorization" {
		t.Errorf("credential auth = %q in %q, want the Bearer authorization header the tracker client sends", c.AuthStyle, c.HeaderName)
	}
	if len(p.Endpoints) != 1 || p.Endpoints[0].Host != "host.docker.internal" || p.Endpoints[0].Port != 7433 {
		t.Errorf("endpoints = %+v, want one on host.docker.internal:7433, the control-plane alias on the tracker's default port", p.Endpoints)
	}
	for _, v := range p.Credentials[0].EnvVars {
		if strings.HasPrefix(v, "GT_") {
			t.Errorf("credential env var %s is a GT_ read, which the config kernel refuses", v)
		}
	}
	if tok := trackerTokenPattern.Find(src); tok != nil {
		t.Errorf("%s holds a tracker token (%s...); the sandbox gets a placeholder and the supervisor substitutes the real one", trackerProfilePath, tok[:len("trk_")+4])
	}
}

// ruleMatches reports whether a rule's path pattern admits a request path. A
// pattern is a glob whose * stays inside one path segment: the forgejo profile's
// /*/*.git/info/refs is an owner, a repository and a literal tail, so a * that
// reached across a slash would grant more than the file says.
func ruleMatches(pattern, route string) bool {
	ok, err := path.Match(pattern, route)
	return err == nil && ok
}

// allowRule builds the rule a profile line `- allow: { method: M, path: P }`
// yields.
func allowRule(method, route string) profileRule {
	var r profileRule
	r.Allow.Method, r.Allow.Path = method, route
	return r
}

// profileAllows reports whether some allow rule admits method and route.
func profileAllows(p profileFile, method, route string) bool {
	for _, e := range p.Endpoints {
		for _, r := range e.Rules {
			if r.Allow.Method == method && ruleMatches(r.Allow.Path, route) {
				return true
			}
		}
	}
	return false
}

// refusedRoutes are the nine routes the research doc's refuse list names
// (docs/research/tracker/K1b-seat-token-as-provider-placeholder.md, question 3):
// the admin-gated ones, and the four no seat has been shown to call. The
// profile leaves them out so a seat token minted with admin scope still cannot
// reach them from a sandbox.
var refusedRoutes = []struct{ method, path string }{
	{"POST", "/v1/tokens"},
	{"POST", "/v1/tokens:revoke"},
	{"PUT", "/v1/config/rig.gt.types.custom"},
	{"GET", "/v1/config/rig.gt.types.custom"},
	{"GET", "/v1/stats"},
	{"POST", "/v1/wisps:purge"},
	{"POST", "/v1/export"},
	{"GET", "/v1/snapshot/town"},
	{"POST", "/v1/board/dispatch"},
}

// seatRoutes are the routes the research doc lists under reads and writes, with
// a sample id in each {id}: what a seat does to its own issues.
var seatRoutes = []struct{ method, path string }{
	{"GET", "/v1/issues/gt-0f305"},
	{"POST", "/v1/issues:show-many"},
	{"POST", "/v1/issues:list"},
	{"POST", "/v1/issues:count"},
	{"POST", "/v1/assigned"},
	{"POST", "/v1/ready"},
	{"POST", "/v1/blocked"},
	{"POST", "/v1/issues:children-of"},
	{"GET", "/v1/events?after=42"},
	{"GET", "/v1/whoami"},
	{"GET", "/v1/issues/gt-0f305/comments"},
	{"GET", "/v1/issues/gt-0f305/dependencies"},
	{"POST", "/v1/issues"},
	{"PATCH", "/v1/issues/gt-0f305"},
	{"POST", "/v1/issues:close"},
	{"POST", "/v1/issues:delete"},
	{"POST", "/v1/graphs"},
	{"POST", "/v1/issues/gt-0f305/claim"},
	{"POST", "/v1/issues/gt-0f305/release"},
	{"POST", "/v1/issues/gt-0f305/transfer"},
	{"POST", "/v1/batch"},
	{"POST", "/v1/issues/gt-0f305/comments"},
	{"POST", "/v1/issues/gt-0f305/dependencies"},
	{"DELETE", "/v1/issues/gt-0f305/dependencies/external:om:om-7"},
}

// routedForms returns the request forms a server resolves to p: the plain path,
// its last segment percent-encoded, and its last segment reached through a dot
// segment. A supervisor matches a rule against the request it receives, so a
// refusal test that only asks the plain form proves nothing about the others.
func routedForms(p string) []string {
	i := strings.LastIndexByte(p, '/')
	head, last := p[:i+1], p[i+1:]
	return []string{
		p,
		head + escapeAll(last),
		head + "x/../" + last,
	}
}

// escapeAll percent-encodes every byte of s.
func escapeAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

// requestPath returns the path a server routes route to: its escapes decoded and
// its dot segments resolved, so /v1/%73tats and /v1/x/../stats are the same
// /v1/stats the plain form is.
func requestPath(t *testing.T, route string) string {
	t.Helper()
	decoded, err := url.PathUnescape(route)
	if err != nil {
		t.Fatalf("unescaping %q: %v", route, err)
	}
	return path.Clean(decoded)
}

// TestTrackerPolecatProfileAllowsNoAdminRoute proves the profile refuses every
// route the research doc's refuse list names, in each form a server resolves to
// it, so the sandbox cannot reach an admin route even with a token minted for
// admin scope.
func TestTrackerPolecatProfileAllowsNoAdminRoute(t *testing.T) {
	t.Parallel()
	p := readProfile(t, trackerProfilePath)
	for _, r := range refusedRoutes {
		for _, form := range routedForms(r.path) {
			route := requestPath(t, form)
			if profileAllows(p, r.method, route) {
				t.Errorf("%s %s (requested as %s) matches an allow rule in %s, and the profile must refuse it", r.method, route, form, trackerProfilePath)
			}
		}
	}
	// A matcher that admits nothing would pass the loop above whatever the file
	// says, so it is shown to admit an admin route when a rule names one, in
	// every form: a normalization that did not resolve these forms would make
	// the loop above vacuous.
	canary := profileFile{Endpoints: []profileEndpoint{{Rules: []profileRule{allowRule("GET", "/v1/*")}}}}
	for _, form := range routedForms("/v1/stats") {
		if route := requestPath(t, form); !profileAllows(canary, "GET", route) {
			t.Fatalf("%s resolves to %s, which the canary rule GET /v1/* does not admit, so the refusal check above would pass whatever the profile says", form, route)
		}
	}
}

// TestTrackerPolecatProfileAllowsSeatRoutes proves each read and write the
// research doc lists matches an allow rule, so a seat's ordinary issue traffic
// keeps its real token through the supervisor.
func TestTrackerPolecatProfileAllowsSeatRoutes(t *testing.T) {
	t.Parallel()
	p := readProfile(t, trackerProfilePath)
	for _, r := range seatRoutes {
		if !profileAllows(p, r.method, r.path) {
			t.Errorf("%s %s matches no allow rule in %s, so the tracker would answer it without the seat's token", r.method, r.path, trackerProfilePath)
		}
	}
}

// TestTrackerPolecatProfileIsLintedByTheImportCommand runs the command over a
// checkout holding the shipped profiles and checks the tracker profile reaches
// `openshell profile lint -f` before its import. The import path lints every
// profile it has not seen, and this test pins that for the tracker file. No
// process starts here; the live lint and import stay the overseer's proof
// (gt-0f305).
func TestTrackerPolecatProfileIsLintedByTheImportCommand(t *testing.T) {
	t.Parallel()
	h := fakeHost(t, map[string]string{"XDG_CONFIG_HOME": keyDir(t)})
	root, err := h.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	dir := filepath.Join(root, "deploy", "polecat", "profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, name := range []string{"forgejo-polecat.yaml", "tracker-polecat.yaml"} {
		src, err := os.ReadFile(filepath.Join("../../deploy/polecat/profiles", name))
		if err != nil {
			t.Fatalf("reading the shipped %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	var calls []string
	h.Exec = recordingExec{calls: &calls}
	var out, errw bytes.Buffer
	if code := run(nil, &out, &errw, h); code != 0 {
		t.Fatalf("run() = %d (stderr %q), want 0", code, errw.String())
	}

	file := filepath.Join(dir, "tracker-polecat.yaml")
	lint := "openshell profile lint -f " + file
	imprt := "openshell profile import -f " + file + " --global"
	lintAt, importAt := slices.Index(calls, lint), slices.Index(calls, imprt)
	if lintAt < 0 {
		t.Fatalf("the command never linted the tracker profile: calls %q", calls)
	}
	if importAt < 0 {
		t.Fatalf("the command never imported the tracker profile: calls %q", calls)
	}
	if lintAt > importAt {
		t.Errorf("the command imported the tracker profile before linting it: calls %q", calls)
	}
}

// recordingExec records every command a run makes and answers each one as
// acceptAll does, so no process starts.
type recordingExec struct {
	calls *[]string
}

func (r recordingExec) Run(name string, args []string, _ []string, _ io.Reader) ([]byte, []byte, error) {
	*r.calls = append(*r.calls, strings.Join(append([]string{name}, args...), " "))
	out, err := acceptAll(name, args)
	return out, nil, err
}

// TestRequestPathResolvesEscapesAndDotSegments pins the normalization the
// refusal test rests on: a server routes an escaped or dot-segment path to the
// plain path it names.
func TestRequestPathResolvesEscapesAndDotSegments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ route, want string }{
		{"/v1/stats", "/v1/stats"},
		{"/v1/%73tats", "/v1/stats"},
		{"/v1/stats/", "/v1/stats"},
		{"/v1/x/../stats", "/v1/stats"},
		{"//v1//stats", "/v1/stats"},
		{"/v1/issues/gt-a", "/v1/issues/gt-a"},
	} {
		if got := requestPath(t, tc.route); got != tc.want {
			t.Errorf("requestPath(%q) = %q, want %q", tc.route, got, tc.want)
		}
	}
}

// TestRuleMatches pins the matcher the two route tests rest on: a * stays inside
// one segment, and a colon names a method on a path rather than a prefix.
func TestRuleMatches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, route string
		want           bool
	}{
		{"/v1/issues/*", "/v1/issues/gt-a", true},
		{"/v1/issues/*", "/v1/issues/gt-a/comments", false},
		{"/v1/issues/*/comments", "/v1/issues/gt-a/comments", true},
		{"/v1/issues", "/v1/issues/gt-a", false},
		{"/v1/events*", "/v1/events?after=42", true},
		{"/v1/issues:list", "/v1/issues/gt-a", false},
		{"/v1/issu*", "/v1/issues", true},
	} {
		if got := ruleMatches(tc.pattern, tc.route); got != tc.want {
			t.Errorf("ruleMatches(%q, %q) = %v, want %v", tc.pattern, tc.route, got, tc.want)
		}
	}
}
