package main

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The two shipped provider profiles this file reads. trackerProfilePath is the
// seat's, and forgejoProfilePath is the one the gateway already imports, which
// calibrates the lint: a lint that refuses the forgejo profile is wrong about
// what `openshell profile lint` takes (gt-0f305).
const (
	trackerProfilePath = "../../deploy/polecat/profiles/tracker-polecat.yaml"
	forgejoProfilePath = "../../deploy/polecat/profiles/forgejo-polecat.yaml"
)

// profileFile is the part of an openshell provider profile the lint reads.
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

// profileIDPattern is the id shape the gateway keys a profile and its providers
// by.
var profileIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// envVarPattern is the shape of an environment variable a credential names.
var envVarPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

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

// profileProblems lists every way p breaks the shape `openshell profile lint`
// accepts: the fields the shipped profiles set, present and in range. The lint
// command is a subprocess and this tier starts none, so the live
// `openshell profile lint -f <file>` stays the authority on the file's grammar
// (gt-0f305); this lint is the same contract stated in Go, so a regression in
// the file fails a test that runs everywhere.
func profileProblems(p profileFile) []string {
	var out []string
	if !profileIDPattern.MatchString(p.ID) {
		out = append(out, "id "+strconv.Quote(p.ID)+" is not a lower-case hyphenated name")
	}
	for _, f := range []struct{ name, value string }{
		{"display_name", p.DisplayName}, {"description", p.Description}, {"category", p.Category},
	} {
		if strings.TrimSpace(f.value) == "" {
			out = append(out, f.name+" is empty")
		}
	}
	if len(p.Credentials) == 0 {
		out = append(out, "no credential is declared")
	}
	declared := make(map[string]bool, len(p.Credentials))
	for i, c := range p.Credentials {
		where := fmt.Sprintf("credentials[%d]", i)
		if strings.TrimSpace(c.Name) == "" {
			out = append(out, where+" has no name")
		}
		declared[c.Name] = true
		if len(c.EnvVars) == 0 {
			out = append(out, where+" names no env_var")
		}
		for _, v := range c.EnvVars {
			if !envVarPattern.MatchString(v) {
				out = append(out, where+" env var "+strconv.Quote(v)+" is not an environment variable name")
			}
		}
		if c.AuthStyle != "header" && c.AuthStyle != "bearer" {
			out = append(out, where+" auth_style "+strconv.Quote(c.AuthStyle)+" is neither header nor bearer")
		}
		if strings.TrimSpace(c.HeaderName) == "" {
			out = append(out, where+" has no header_name")
		}
		if !c.Required {
			out = append(out, where+" is not required, so a sandbox without the token would start")
		}
	}
	if len(p.Discovery.Credentials) == 0 {
		out = append(out, "discovery names no credential")
	}
	for _, name := range p.Discovery.Credentials {
		if !declared[name] {
			out = append(out, "discovery names "+strconv.Quote(name)+", which no credential declares")
		}
	}
	if len(p.Endpoints) == 0 {
		out = append(out, "no endpoint is declared")
	}
	for i, e := range p.Endpoints {
		where := fmt.Sprintf("endpoints[%d]", i)
		if strings.TrimSpace(e.Host) == "" || strings.ContainsAny(e.Host, " /\\:") {
			out = append(out, where+" host "+strconv.Quote(e.Host)+" is not a bare host name")
		}
		if e.Port < 1 || e.Port > 65535 {
			out = append(out, where+" port is not 1-65535")
		}
		if e.Protocol != "rest" && e.Protocol != "tcp" {
			out = append(out, where+" protocol "+strconv.Quote(e.Protocol)+" is neither rest nor tcp")
		}
		if e.Enforcement != "enforce" {
			out = append(out, where+" enforcement is "+strconv.Quote(e.Enforcement)+", want enforce: an endpoint the supervisor does not inspect hands the token to every request on it")
		}
		if e.Protocol == "rest" && len(e.Rules) == 0 {
			out = append(out, where+" is a rest endpoint with no rules, so the token reaches every request on it")
		}
		for j, r := range e.Rules {
			rw := fmt.Sprintf("%s.rules[%d]", where, j)
			if !slices.Contains(httpMethods, r.Allow.Method) {
				out = append(out, rw+" method "+strconv.Quote(r.Allow.Method)+" is not an HTTP method")
			}
			if !strings.HasPrefix(r.Allow.Path, "/") {
				out = append(out, rw+" path "+strconv.Quote(r.Allow.Path)+" is not an absolute path")
			}
		}
	}
	if len(p.Binaries) == 0 {
		out = append(out, "no binary is declared")
	}
	for _, b := range p.Binaries {
		if !strings.HasPrefix(b, "/") || strings.ContainsAny(b, " \t") {
			out = append(out, "binary "+strconv.Quote(b)+" is not an absolute path")
		}
	}
	return out
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

// TestTrackerPolecatProfileLints holds the shipped tracker profile to the lint
// the forgejo profile passes, and to the contract the profile exists for: one
// TRACKER_TOKEN sent as the authorization header to the tracker's control-plane
// alias, with no token in the file.
func TestTrackerPolecatProfileLints(t *testing.T) {
	t.Parallel()
	if probs := profileProblems(readProfile(t, forgejoProfilePath)); len(probs) != 0 {
		t.Fatalf("the lint refuses %s, which the gateway imports today: %s", forgejoProfilePath, strings.Join(probs, "; "))
	}
	src := readProfileSource(t, trackerProfilePath)
	p := readProfile(t, trackerProfilePath)
	if probs := profileProblems(p); len(probs) != 0 {
		t.Fatalf("%s does not pass the lint: %s", trackerProfilePath, strings.Join(probs, "; "))
	}

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

// TestTrackerPolecatProfileAllowsNoAdminRoute proves the profile refuses every
// route the research doc's refuse list names, so the sandbox cannot reach an
// admin route even with a token minted for admin scope.
func TestTrackerPolecatProfileAllowsNoAdminRoute(t *testing.T) {
	t.Parallel()
	p := readProfile(t, trackerProfilePath)
	for _, r := range refusedRoutes {
		if profileAllows(p, r.method, r.path) {
			t.Errorf("%s %s matches an allow rule in %s, and the profile must refuse it", r.method, r.path, trackerProfilePath)
		}
	}
	// A matcher that admits nothing would pass the loop above whatever the file
	// says, so it is shown to admit an admin route when a rule names one.
	canary := profileFile{Endpoints: []profileEndpoint{{Rules: []profileRule{allowRule("GET", "/v1/*")}}}}
	if !profileAllows(canary, "GET", "/v1/stats") {
		t.Fatal("the rule matcher admits nothing, so it would pass this test whatever the profile says")
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

// TestProfileLintRefusesEachBreak drives every refusal the lint has, so a check
// that stopped working shows up here rather than as a profile that quietly
// imports.
func TestProfileLintRefusesEachBreak(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*profileFile){
		"no id":                       func(p *profileFile) { p.ID = "" },
		"unhyphenated id":             func(p *profileFile) { p.ID = "Tracker Polecat" },
		"no display name":             func(p *profileFile) { p.DisplayName = "" },
		"no description":              func(p *profileFile) { p.Description = "" },
		"no category":                 func(p *profileFile) { p.Category = "" },
		"no credential":               func(p *profileFile) { p.Credentials = nil },
		"credential with no env var":  func(p *profileFile) { p.Credentials[0].EnvVars = nil },
		"lower-case env var":          func(p *profileFile) { p.Credentials[0].EnvVars = []string{"tracker_token"} },
		"unknown auth style":          func(p *profileFile) { p.Credentials[0].AuthStyle = "query" },
		"no header name":              func(p *profileFile) { p.Credentials[0].HeaderName = "" },
		"optional credential":         func(p *profileFile) { p.Credentials[0].Required = false },
		"discovery names none":        func(p *profileFile) { p.Discovery.Credentials = nil },
		"discovery names a stranger":  func(p *profileFile) { p.Discovery.Credentials = []string{"api_token"} },
		"no endpoint":                 func(p *profileFile) { p.Endpoints = nil },
		"host with a port":            func(p *profileFile) { p.Endpoints[0].Host = "host.docker.internal:7433" },
		"port out of range":           func(p *profileFile) { p.Endpoints[0].Port = 0 },
		"unknown protocol":            func(p *profileFile) { p.Endpoints[0].Protocol = "http" },
		"unenforced endpoint":         func(p *profileFile) { p.Endpoints[0].Enforcement = "" },
		"rest endpoint with no rules": func(p *profileFile) { p.Endpoints[0].Rules = nil },
		"unknown method":              func(p *profileFile) { p.Endpoints[0].Rules[0].Allow.Method = "FETCH" },
		"relative rule path":          func(p *profileFile) { p.Endpoints[0].Rules[0].Allow.Path = "v1/whoami" },
		"no binary":                   func(p *profileFile) { p.Binaries = nil },
		"relative binary":             func(p *profileFile) { p.Binaries = []string{"gt"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := readProfile(t, trackerProfilePath)
			if probs := profileProblems(p); len(probs) != 0 {
				t.Fatalf("the shipped profile already breaks the lint: %v", probs)
			}
			mutate(&p)
			if probs := profileProblems(p); len(probs) == 0 {
				t.Errorf("%s: the lint accepted a profile that breaks the contract", name)
			}
		})
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
