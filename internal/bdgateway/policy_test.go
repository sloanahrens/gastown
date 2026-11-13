package bdgateway

import (
	"errors"
	"strings"
	"testing"
)

// fixturePolicy is the policy the unit tables drive: a verb of each class, a
// two-word verb, a global refused flag and a per-verb one. It is parsed through
// ParsePolicy, so the tables exercise the same loader production reads.
const fixturePolicy = `{
	"refused_flags": ["db", "C", "sandbox"],
	"verbs": [
		{"path": "comments", "class": "read"},
		{"path": "show", "class": "read"},
		{"path": "version", "class": "read"},
		{"path": "comments add", "class": "write"},
		{"path": "create", "class": "write", "flags_refused": ["graph"]},
		{"path": "update", "class": "write"},
		{"path": "config get", "class": "read"},
		{"path": "config set", "class": "admin"}
	]
}`

func testPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := ParsePolicy(strings.NewReader(fixturePolicy))
	if err != nil {
		t.Fatalf("ParsePolicy(fixture): %v", err)
	}
	return p
}

// TestPolicyClassCheck: a read verb runs for every token class, a write verb is
// refused to the read class, and an admin verb is refused to everything below
// operator. The unknown class is refused rather than defaulted.
func TestPolicyClassCheck(t *testing.T) {
	t.Parallel()
	p := testPolicy(t)

	cases := []struct {
		name    string
		class   TokenClass
		argv    []string
		wantErr error
	}{
		{"read token runs a read verb", TokenRead, []string{"show"}, nil},
		{"daemon token runs a read verb", TokenDaemon, []string{"show"}, nil},
		{"operator token runs a read verb", TokenOperator, []string{"show"}, nil},
		{"read token cannot run a write verb", TokenRead, []string{"update"}, ErrClassDenied},
		{"daemon token runs a write verb", TokenDaemon, []string{"update"}, nil},
		{"operator token runs a write verb", TokenOperator, []string{"update"}, nil},
		{"read token cannot run an admin verb", TokenRead, []string{"config", "set"}, ErrClassDenied},
		{"daemon token cannot run an admin verb", TokenDaemon, []string{"config", "set"}, ErrClassDenied},
		{"operator token runs an admin verb", TokenOperator, []string{"config", "set"}, nil},
		{"unknown token class is refused", TokenClass("root"), []string{"show"}, ErrClassDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.Authorize(tc.class, tc.argv)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authorize(%q, %q) = %v, want error %v", tc.class, tc.argv, err, tc.wantErr)
			}
		})
	}

	// Allowed is the check by itself, including a verb class no file can hold.
	for _, class := range []TokenClass{TokenRead, TokenDaemon, TokenOperator} {
		if !Allowed(class, ClassRead) {
			t.Errorf("Allowed(%q, read) = false, want the read verb allowed", class)
		}
	}
	if Allowed(TokenRead, ClassWrite) {
		t.Error("Allowed(read, write) = true, want refused")
	}
	if Allowed(TokenDaemon, ClassAdmin) {
		t.Error("Allowed(daemon, admin) = true, want refused")
	}
	if Allowed(TokenOperator, VerbClass("root")) {
		t.Error("Allowed(operator, unknown verb class) = true, want refused")
	}
	if Allowed(TokenClass("root"), ClassRead) {
		t.Error("Allowed(unknown token class, read) = true, want refused")
	}
}

// TestPolicyRefusedFlags: a flag on the global list is refused on an allowed
// verb, a per-verb flag is refused only for its verb, an allowed flag passes,
// and nothing after "--" is read as a flag.
func TestPolicyRefusedFlags(t *testing.T) {
	t.Parallel()
	p := testPolicy(t)

	cases := []struct {
		name    string
		class   TokenClass
		argv    []string
		wantErr error
		wantMsg string
	}{
		{"refused flag, attached value", TokenRead, []string{"show", "--db=beads"}, ErrFlagRefused, "--db"},
		{"refused flag, separate value", TokenRead, []string{"show", "--db", "beads"}, ErrFlagRefused, "--db"},
		{"refused shorthand", TokenRead, []string{"show", "-C", "gastown"}, ErrFlagRefused, "--C"},
		{"refused shorthand with attached value", TokenRead, []string{"show", "-Cgastown"}, ErrFlagRefused, "--C"},
		{"refused shorthand in a group", TokenRead, []string{"show", "-qC"}, ErrFlagRefused, "--C"},
		{"per-verb refused flag", TokenDaemon, []string{"create", "--graph", "g.dot"}, ErrFlagRefused, "--graph"},
		{"per-verb flag is not global", TokenRead, []string{"show", "--graph", "g.dot"}, nil, ""},
		{"allowed flag", TokenRead, []string{"show", "--json"}, nil, ""},
		{"nothing after the separator is a flag", TokenRead, []string{"show", "--", "--db"}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.Authorize(tc.class, tc.argv)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authorize(%q) = %v, want error %v", tc.argv, err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Authorize(%q) error = %q, want it to name %s", tc.argv, err, tc.wantMsg)
			}
		})
	}
}

// TestPolicyVerbResolution: the longest listed path wins, a leading bd
// persistent flag and its value are read past, and a verb the file omits is
// refused by name.
func TestPolicyVerbResolution(t *testing.T) {
	t.Parallel()
	p := testPolicy(t)

	cases := []struct {
		name    string
		argv    []string
		want    string
		wantErr error
	}{
		{"one-word verb with an id", []string{"show", "gt-abc", "--json"}, "show", nil},
		{"id is not a second verb word", []string{"comments", "gt-abc"}, "comments", nil},
		{"two-word verb", []string{"comments", "add", "gt-abc"}, "comments add", nil},
		{"longest prefix wins", []string{"config", "get", "issue_prefix"}, "config get", nil},
		{"leading flag before the verb", []string{"--allow-stale", "version"}, "version", nil},
		{"leading value flag and its value", []string{"--actor", "town/crew/dipper", "show"}, "show", nil},
		{"unknown verb is refused by name", []string{"dolt", "push"}, "", ErrVerbNotAllowed},
		{"no verb at all", []string{"--json"}, "", ErrVerbNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := p.Authorize(TokenOperator, tc.argv)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authorize(%q) = %v, want error %v", tc.argv, err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if got := v.Name(); got != tc.want {
				t.Errorf("Authorize(%q) matched %q, want %q", tc.argv, got, tc.want)
			}
		})
	}

	// The refusal names the verb word, so an operator can see what was refused.
	if _, err := p.Authorize(TokenOperator, []string{"dolt", "push"}); err == nil || !strings.Contains(err.Error(), "\"dolt\"") {
		t.Errorf("unknown-verb error = %v, want it to name dolt", err)
	}
}

// TestParsePolicyRefusals: every way a document can be untrustworthy is an
// error, never a policy with a narrower or wider surface than its author meant.
func TestParsePolicyRefusals(t *testing.T) {
	t.Parallel()

	valid := `{"refused_flags": ["db"], "verbs": [{"path": "show", "class": "read"}]}`
	cases := []struct {
		name string
		doc  string
	}{
		{"empty document", ``},
		{"not an object", `[]`},
		{"unknown field", `{"refused_flags": ["db"], "verbs": [{"path": "show", "class": "read"}], "extra": 1}`},
		{"misspelt verb field", `{"refused_flags": ["db"], "verbs": [{"path": "show", "clas": "read"}]}`},
		{"trailing data", valid + `{}`},
		{"no verbs", `{"refused_flags": ["db"], "verbs": []}`},
		{"no refused flags", `{"verbs": [{"path": "show", "class": "read"}]}`},
		{"unknown class", `{"refused_flags": ["db"], "verbs": [{"path": "show", "class": "root"}]}`},
		{"missing class", `{"refused_flags": ["db"], "verbs": [{"path": "show"}]}`},
		{"duplicate path", `{"refused_flags": ["db"], "verbs": [{"path": "show", "class": "read"}, {"path": "show", "class": "write"}]}`},
		{"path with a flag word", `{"refused_flags": ["db"], "verbs": [{"path": "show --json", "class": "read"}]}`},
		{"path too deep", `{"refused_flags": ["db"], "verbs": [{"path": "a b c d", "class": "read"}]}`},
		{"refused flag keeps its dashes", `{"refused_flags": ["--db"], "verbs": [{"path": "show", "class": "read"}]}`},
		{"duplicate refused flag", `{"refused_flags": ["db", "db"], "verbs": [{"path": "show", "class": "read"}]}`},
		{"verb flag keeps its dashes", `{"refused_flags": ["db"], "verbs": [{"path": "show", "class": "read", "flags_refused": ["--graph"]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParsePolicy(strings.NewReader(tc.doc)); !errors.Is(err, ErrBadVerbsFile) {
				t.Fatalf("ParsePolicy(%s) = %v, want ErrBadVerbsFile", tc.name, err)
			}
		})
	}

	if _, err := ParsePolicy(strings.NewReader(valid)); err != nil {
		t.Fatalf("ParsePolicy(valid) = %v, want it to load", err)
	}
}
