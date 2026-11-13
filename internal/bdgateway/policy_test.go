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
		{"read token cannot reach a write verb behind a value flag", TokenRead, []string{"--format", "show", "create"}, ErrClassDenied},
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
		{"refused flag, attached value", TokenRead, []string{"show", "--db=beads"}, ErrFlagRefused, "refused: --db on show"},
		{"refused flag, separate value", TokenRead, []string{"show", "--db", "beads"}, ErrFlagRefused, "refused: --db on show"},
		{"refused shorthand", TokenRead, []string{"show", "-C", "gastown"}, ErrFlagRefused, "refused: -C on show"},
		{"refused shorthand with attached value", TokenRead, []string{"show", "-Cgastown"}, ErrFlagRefused, "refused: -C on show"},
		{"refused shorthand in a group", TokenRead, []string{"show", "-qC"}, ErrFlagRefused, "refused: -C on show"},
		{"per-verb refused flag", TokenDaemon, []string{"create", "--graph", "g.dot"}, ErrFlagRefused, "refused: --graph on create"},
		{"per-verb flag is not global", TokenRead, []string{"show", "--graph", "g.dot"}, nil, ""},
		{"allowed flag", TokenRead, []string{"show", "--json"}, nil, ""},
		{"nothing after a real separator is a flag", TokenRead, []string{"show", "--", "--db"}, nil, ""},
		{"refused flag after a separator that is a value", TokenRead, []string{"--format", "--", "--db", "/x", "show"}, ErrFlagRefused, "refused: --db on show"},
		{"refused shorthand after a separator that is a value", TokenRead, []string{"--format", "--", "-C", "/x", "show"}, ErrFlagRefused, "refused: -C on show"},
		{"refused flag attached after a separator that is a value", TokenRead, []string{"--format", "--", "--db=/x", "show"}, ErrFlagRefused, "refused: --db on show"},
		{"refused shorthand whose value is a separator", TokenRead, []string{"show", "-C", "--", "dir"}, ErrFlagRefused, "refused: -C on show"},
		{"unknown flag before a dash argument is ambiguous", TokenRead, []string{"show", "--thread", "--db"}, ErrAmbiguousVerb, ""},
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

// TestPolicyVerbResolution: the resolver reads argv the way bd's root parse
// does, so the verb it class-checks is the verb bd dispatches. The longest
// listed path wins, a value-taking flag's value is never a verb word, a flag
// the policy cannot place before the verb is refused rather than guessed, and a
// verb the file omits is refused by name.
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
		{"leading value flag and its value", []string{"--actor", "town/crew/dipper", "show"}, "show", nil},
		{"value flag takes a verb-shaped value", []string{"--format", "show", "create"}, "create", nil},
		{"attached value is the flag's own", []string{"--format=show", "create"}, "create", nil},
		{"boolean flag leaves the next word", []string{"--json", "show"}, "show", nil},
		{"unknown flag before the verb is refused", []string{"--allow-stale", "version"}, "", ErrAmbiguousVerb},
		{"unknown shorthand before the verb is refused", []string{"-q", "show"}, "", ErrAmbiguousVerb},
		{"command flag before the verb is refused", []string{"--graph", "show", "create"}, "", ErrAmbiguousVerb},
		{"flag after the verb cannot change it", []string{"update", "--assignee", "bob", "show"}, "update", nil},
		{"empty argument is not a word", []string{"", "show"}, "show", nil},
		{"separator ends the command words", []string{"--", "show"}, "", ErrVerbNotAllowed},
		{"an alias the file does not list is refused", []string{"new", "gt-abc"}, "", ErrVerbNotAllowed},
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

// TestPolicyShorthandValue: -C takes the next argument as its value, so a
// directory named like a verb is not read as one. The policy here does not
// refuse -C, so the matched verb shows how the scan read it.
func TestPolicyShorthandValue(t *testing.T) {
	t.Parallel()
	p, err := ParsePolicy(strings.NewReader(`{
		"refused_flags": ["db"],
		"verbs": [
			{"path": "show", "class": "read"},
			{"path": "town", "class": "read"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"separate value", []string{"-C", "town", "show"}, "show"},
		{"glued value", []string{"-Ctown", "show"}, "show"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := p.Authorize(TokenRead, tc.argv)
			if err != nil {
				t.Fatalf("Authorize(%q) = %v, want %q", tc.argv, err, tc.want)
			}
			if got := v.Name(); got != tc.want {
				t.Errorf("Authorize(%q) matched %q, want %q", tc.argv, got, tc.want)
			}
		})
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
