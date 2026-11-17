package bdgateway

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beadsql"
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
		{"unknown flag after the verb, next flag still refused", TokenRead, []string{"show", "--thread", "--db"}, ErrFlagRefused, "refused: --db on show"},
		{"unknown flag after the verb is boolean before another flag", TokenRead, []string{"show", "--include-infra", "--json"}, nil, ""},
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

// TestUnknownCommandFlagAfterVerb: bd's command flags are not in the root flag
// set, so once the verb is settled their arity cannot change the verb or hide a
// flag. The scan reads such a flag as a boolean that consumes nothing, which is
// what lets the escalation collector's and the plugin run ledger's calls
// through, while before the verb is settled the same flag is still refused and
// a refused flag behind it is still caught.
func TestUnknownCommandFlagAfterVerb(t *testing.T) {
	t.Parallel()

	deploy := loadDeployPolicy(t)
	authorized := []struct {
		name string
		argv []string
		want string
	}{
		{"escalation collector list", []string{"list", "--json", "--status=open", "--label=gt:escalation", "--include-infra", "--limit=0", "--flat"}, "list"},
		{"plugin run ledger query", []string{"query", "--json", "ephemeral=true", "--all", "--limit=0"}, "query"},
		{"unknown flag before the last flag", []string{"list", "--include-infra", "--flat"}, "list"},
	}
	for _, tc := range authorized {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := deploy.Authorize(TokenRead, tc.argv)
			if err != nil {
				t.Fatalf("Authorize(read, %q) = %v, want it allowed", tc.argv, err)
			}
			if v.Name() != tc.want || v.Class != ClassRead {
				t.Errorf("Authorize(read, %q) matched %q (%s), want %s (read)", tc.argv, v.Name(), v.Class, tc.want)
			}
		})
	}

	// Before the verb is settled the flag could be taking the word it is
	// followed by, so the call is still refused rather than guessed.
	for _, argv := range [][]string{
		{"--bogus", "--json", "list"},
		{"--include-infra", "--all", "query"},
	} {
		if _, err := deploy.Authorize(TokenRead, argv); !errors.Is(err, ErrAmbiguousVerb) {
			t.Errorf("Authorize(read, %q) = %v, want ErrAmbiguousVerb", argv, err)
		}
	}

	// A refused command flag behind an unknown one is still caught: the unknown
	// flag is boolean, so --force is scanned as a flag in its own right.
	p, err := ParsePolicy(strings.NewReader(`{
		"refused_flags": ["force"],
		"verbs": [{"path": "list", "class": "read"}]
	}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	argv := []string{"list", "--include-infra", "--force"}
	if _, err := p.Authorize(TokenRead, argv); !errors.Is(err, ErrFlagRefused) {
		t.Fatalf("Authorize(read, %q) = %v, want ErrFlagRefused", argv, err)
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

// TestAuthorizeRootHelp: bd's root help probe — the argv beads.sampleOffersVerb
// runs before a sample write — is authorized as the policy's own read row for
// every token class, and a help request carrying a word or flag is refused the
// way an unknown verb is.
func TestAuthorizeRootHelp(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)

	classes := []TokenClass{TokenRead, TokenDaemon, TokenOperator}
	for _, class := range classes {
		for _, argv := range [][]string{{"--help"}, {"help"}} {
			v, err := p.Authorize(class, argv)
			if err != nil {
				t.Fatalf("Authorize(%q, %q) = %v, want the help probe allowed", class, argv, err)
			}
			if v.Name() != "help" || v.Class != ClassRead {
				t.Errorf("Authorize(%q, %q) matched %q (%s), want help (read)", class, argv, v.Name(), v.Class)
			}
		}
	}

	// An unknown verb is refused with ErrVerbNotAllowed; the help requests
	// below must get that same refusal.
	if _, err := p.Authorize(TokenOperator, []string{"dolt", "push"}); !errors.Is(err, ErrVerbNotAllowed) {
		t.Fatalf("Authorize(dolt push) = %v, want ErrVerbNotAllowed", err)
	}
	for _, argv := range [][]string{
		{"--help", "show"},
		{"help", "show"},
		{"--help", "--json"},
	} {
		for _, class := range classes {
			if _, err := p.Authorize(class, argv); !errors.Is(err, ErrVerbNotAllowed) {
				t.Errorf("Authorize(%q, %q) = %v, want ErrVerbNotAllowed", class, argv, err)
			}
		}
	}

	if _, err := p.Authorize(TokenClass("root"), []string{"--help"}); !errors.Is(err, ErrClassDenied) {
		t.Errorf("Authorize(unknown class, --help) = %v, want ErrClassDenied", err)
	}
}

// TestAuthorizeRootHelpDoesNotWiden: the probe is the only verb the class check
// gains. "help" is no row of the file, every listed verb still authorizes
// exactly when its class allows the token, and an argv naming no listed verb is
// still refused for every class.
func TestAuthorizeRootHelpDoesNotWiden(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	if _, listed := p.verbs["help"]; listed {
		t.Fatal(`deploy/bd/verbs.json lists "help": the probe must be the policy's own row`)
	}

	classes := []TokenClass{TokenRead, TokenDaemon, TokenOperator}
	for _, class := range classes {
		for name, verb := range p.verbs {
			// sql carries its own rule on the operand (checkSQLStatement), so
			// the argv that exercises its class is a read-only statement, not
			// the bare path: the class check is what this loop holds.
			argv := append([]string(nil), verb.Path...)
			if name == sqlVerb {
				argv = append(argv, "SELECT 1")
			}
			v, err := p.Authorize(class, argv)
			if !Allowed(class, verb.Class) {
				if !errors.Is(err, ErrClassDenied) {
					t.Errorf("%s token, %q = %v, want ErrClassDenied", class, name, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s token, %q = %v, want it allowed", class, name, err)
				continue
			}
			if v.Name() != name {
				t.Errorf("%s token, %q matched %q, want it unchanged", class, name, v.Name())
			}
		}

		for _, argv := range [][]string{
			{},
			{"dolt", "push"},
			{"sample", "purge"},
			{"new", "gt-abc"},
			{"help", "show"},
			{"--help", "show"},
			{"--help", "--json"},
			{"--json", "--help"},
			{"--help=true"},
		} {
			if _, err := p.Authorize(class, argv); err == nil {
				t.Errorf("%s token, %q = allowed, want it refused", class, argv)
			}
		}
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

// TestAuthorizeSQLReadOnly: deploy/bd/verbs.json serves bd sql at the write
// class, the narrowest class the daemon and operator tokens hold and the read
// token does not, so the schema-level read internal/deps runs at startup and
// the externalMolecules read are authorized for those classes and refused for
// a read token.
func TestAuthorizeSQLReadOnly(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	// The argv the handshake sends: internal/beads/version.go builds it the
	// same way for its bd sql call.
	handshake, err := beadsql.SchemaLevel().BdArgs("--json")
	if err != nil {
		t.Fatalf("beadsql.SchemaLevel().BdArgs: %v", err)
	}
	cases := []struct {
		name    string
		class   TokenClass
		argv    []string
		wantErr error
	}{
		{"daemon token runs the schema-level read", TokenDaemon, handshake, nil},
		{"operator token runs the schema-level read", TokenOperator, handshake, nil},
		{"read token cannot run sql", TokenRead, handshake, ErrClassDenied},
		{"daemon token runs a plain SELECT", TokenDaemon, []string{"sql", "SELECT 1"}, nil},
		{"operator token runs a SELECT behind bd's flags", TokenOperator, []string{"sql", "--json", "SELECT 1"}, nil},
		{"a trailing semicolon is one statement", TokenDaemon, []string{"sql", "--csv", "SELECT 1;"}, nil},
		// sql's only own flag is the bool --csv, so it consumes no operand
		// whichever side of the statement it is written: the SELECT stays the
		// one argument bd runs.
		{"--csv before the statement", TokenDaemon, []string{"sql", "--csv", "SELECT 1"}, nil},
		{"--csv after the statement", TokenDaemon, []string{"sql", "SELECT 1", "--csv"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := p.Authorize(tc.class, tc.argv)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Authorize(%q, %q) = %v, want %v", tc.class, tc.argv, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authorize(%q, %q) = %v, want it allowed", tc.class, tc.argv, err)
			}
			if v.Name() != "sql" || v.Class != ClassWrite {
				t.Errorf("Authorize(%q, %q) matched %q (%s), want sql (write)", tc.class, tc.argv, v.Name(), v.Class)
			}
		})
	}
}

// TestSQLRefusesWrites: bd's sql verb runs the statement it is handed, so an
// INSERT, an UPDATE, a DELETE, DDL, a multi-statement string, an empty
// statement and a call naming two arguments are each refused with a refusal
// naming the read-only rule. The cases that put a value-taking root flag before
// the statement pin the operand placement: the flag consumes its value, so the
// query cannot be smuggled into that position. The gateway handler drives the
// same calls and sees the runner never asked to start bd for a refusal, while
// a read reaches it with its argv intact.
func TestSQLRefusesWrites(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	cases := []struct {
		name string
		argv []string
	}{
		{"an INSERT", []string{"sql", "INSERT INTO beads (id) VALUES ('gt-x')"}},
		{"an UPDATE", []string{"sql", "UPDATE beads SET status = 'closed'"}},
		{"a DELETE", []string{"sql", "DELETE FROM beads"}},
		{"DDL", []string{"sql", "DROP TABLE beads"}},
		{"a multi-statement string", []string{"sql", "SELECT 1; DROP TABLE beads"}},
		{"an empty statement", []string{"sql"}},
		{"an empty string argument", []string{"sql", ""}},
		{"an empty statement behind bd's flags", []string{"sql", "--json"}},
		{"two statements as two arguments", []string{"sql", "SELECT 1", "DROP TABLE beads"}},
		// --format takes a value, so the SELECT is its argument and the call
		// names no operand: refused rather than read as a statement.
		{"a flag value mistaken for the query", []string{"sql", "--format", "SELECT 1"}},
		{"no statement behind a sql flag", []string{"sql", "--csv"}},
		{"two statements behind a sql flag", []string{"sql", "--csv", "SELECT 1", "SELECT 2"}},
		// An attached value is never an operand either, so a query cannot ride
		// in it whatever flag bd learns next.
		{"a statement in an attached flag value", []string{"sql", "--query=DELETE FROM issues"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.Authorize(TokenDaemon, tc.argv)
			if !errors.Is(err, ErrSQLNotReadOnly) {
				t.Fatalf("Authorize(daemon, %q) = %v, want ErrSQLNotReadOnly", tc.argv, err)
			}
			if !strings.Contains(err.Error(), "read-only") {
				t.Errorf("Authorize(daemon, %q) error = %q, want it to name the read-only rule", tc.argv, err)
			}
		})
	}

	gatewayCases := []struct {
		name    string
		argv    []string
		want    int
		wantRan bool
	}{
		{"a read is served", []string{"sql", "--json", "SELECT 1"}, http.StatusOK, true},
		{"an INSERT is refused", []string{"sql", "INSERT INTO beads (id) VALUES ('gt-x')"}, http.StatusForbidden, false},
		{"DDL is refused", []string{"sql", "DROP TABLE beads"}, http.StatusForbidden, false},
		{"a multi-statement string is refused", []string{"sql", "SELECT 1; DROP TABLE beads"}, http.StatusForbidden, false},
		{"an empty statement is refused", []string{"sql"}, http.StatusForbidden, false},
		{"two operands are refused", []string{"sql", "SELECT 1", "DROP TABLE beads"}, http.StatusForbidden, false},
	}
	for _, tc := range gatewayCases {
		t.Run("gateway: "+tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubRunner{}
			g, err := NewGateway(GatewayOptions{
				Auth:       testAuthenticator(t),
				Policy:     p,
				Workspaces: mustStore(t),
				Log:        newTestCallLog(t),
				Runner:     stub,
			})
			if err != nil {
				t.Fatalf("NewGateway: %v", err)
			}
			if code := postExec(t, g, testDaemonToken, Request{Argv: tc.argv}); code != tc.want {
				t.Fatalf("sql call %q through the gateway = %d, want %d", tc.argv, code, tc.want)
			}
			if stub.ran != tc.wantRan {
				t.Fatalf("sql call %q started the runner = %v, want %v", tc.argv, stub.ran, tc.wantRan)
			}
			if tc.wantRan && !slices.Equal(stub.req.Argv, tc.argv) {
				t.Errorf("runner got argv %q, want %q", stub.req.Argv, tc.argv)
			}
		})
	}
}
