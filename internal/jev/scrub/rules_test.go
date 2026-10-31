package scrub

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// syntheticJWT is a token shaped like a JWT; every segment is test text.
const syntheticJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlLWZvci10ZXN0cy1vbmx5LTAxMjM0NTY3ODk"

// ruleCase is one row of TestScrubRules. A positive row expects count
// replacements and an exact output; a negative row expects the input back and
// no replacement.
type ruleCase struct {
	name  string
	rule  string
	in    string
	want  string
	count int
}

func ruleCases() []ruleCase {
	const (
		pemBegin = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n"
		pemEnd   = "-----END RSA PRIVATE KEY-----"
		hex40    = "0123456789abcdef0123456789abcdef01234567"
		hex64    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		hex128   = hex64 + hex64
	)
	return []ruleCase{
		// private-key
		{
			name: "block", rule: RulePrivateKey,
			in:   pemBegin + pemEnd,
			want: "[REDACTED:private-key]", count: 1,
		},
		{
			name: "unterminated block", rule: RulePrivateKey,
			in:   pemBegin + "b3BlbnNzaC1rZXktdjEAAAAA\n",
			want: "[REDACTED:private-key]", count: 1,
		},
		{
			name: "public key", rule: RulePrivateKey,
			in:   "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkq\n-----END PUBLIC KEY-----",
			want: "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkq\n-----END PUBLIC KEY-----",
		},

		// auth-header
		{
			name: "authorization header", rule: RuleAuthHeader,
			in:   "Authorization: Bearer abcdefgh12345678",
			want: "Authorization: [REDACTED:auth-header]", count: 1,
		},
		{
			name: "bare bearer", rule: RuleAuthHeader,
			in:   "curl -H 'Bearer abcdefgh12345678'",
			want: "curl -H 'Bearer [REDACTED:auth-header]'", count: 1,
		},
		{
			name: "prose", rule: RuleAuthHeader,
			in:   "the authorization decision stands",
			want: "the authorization decision stands",
		},

		// cookie-header
		{
			name: "cookie header", rule: RuleCookieHeader,
			in:   "Cookie: sid=abcdef1234567890",
			want: "Cookie: sid=[REDACTED:cookie-header]", count: 1,
		},
		{
			name: "set cookie keeps its attributes", rule: RuleCookieHeader,
			in:   "Set-Cookie: sid=abcdef1234567890; Path=/; HttpOnly",
			want: "Set-Cookie: sid=[REDACTED:cookie-header]; Path=/; HttpOnly", count: 1,
		},
		{
			name: "two cookies", rule: RuleCookieHeader,
			in:   "Cookie: sid=abcdef1234567890; theme=dark",
			want: "Cookie: sid=[REDACTED:cookie-header]; theme=[REDACTED:cookie-header]", count: 2,
		},
		{
			name: "cookie in a quoted command line", rule: RuleCookieHeader,
			in:   "curl -H 'Cookie: sid=abcdef1234567890' https://example.com",
			want: "curl -H 'Cookie: sid=[REDACTED:cookie-header]' https://example.com", count: 1,
		},
		{
			name: "quoted cookie value", rule: RuleCookieHeader,
			in:   `Set-Cookie: sid="abc def"; Path=/`,
			want: "Set-Cookie: sid=[REDACTED:cookie-header]; Path=/", count: 1,
		},
		{
			name: "prose", rule: RuleCookieHeader,
			in:   "the cookie jar is on the table",
			want: "the cookie jar is on the table",
		},

		// url-userinfo
		{
			name: "user info", rule: RuleURLUserInfo,
			in:   "https://alice:hunter2@example.com/team/repo.git",
			want: "https://[REDACTED:url-userinfo]@example.com/team/repo.git", count: 1,
		},
		{
			name: "plain url", rule: RuleURLUserInfo,
			in:   "https://example.com/team/repo.git",
			want: "https://example.com/team/repo.git",
		},

		// jwt
		{
			name: "token", rule: RuleJWT,
			in:    "Authorization failed for " + syntheticJWT,
			want:  "Authorization failed for [REDACTED:jwt]",
			count: 1,
		},
		{
			name: "two segments", rule: RuleJWT,
			in:   "eyJhbGciOiJIUzI1NiJ9.payload",
			want: "eyJhbGciOiJIUzI1NiJ9.payload",
		},

		// vendor-key
		{
			name: "github token", rule: RuleVendorKey,
			in:   "key ghp_fixturevaluenotarealtoken here",
			want: "key [REDACTED:vendor-key] here", count: 1,
		},
		{
			name: "aws key", rule: RuleVendorKey,
			in:   "AKIAIOSFODNN7EXAMPLE",
			want: "[REDACTED:vendor-key]", count: 1,
		},
		{
			name: "short sk words", rule: RuleVendorKey,
			in:   "sk-learn and sk-short are words",
			want: "sk-learn and sk-short are words",
		},

		// assignment
		{
			name: "env assignment", rule: RuleAssignment,
			in:   "API_TOKEN=abc123def456",
			want: "API_TOKEN=[REDACTED:assignment]", count: 1,
		},
		{
			name: "yaml assignment", rule: RuleAssignment,
			in:   "  password: hunter2",
			want: "  password: [REDACTED:assignment]", count: 1,
		},
		{
			name: "hex beside credential name", rule: RuleAssignment,
			in:   "token=" + hex40,
			want: "token=[REDACTED:assignment]", count: 1,
		},
		{
			name: "unterminated quote", rule: RuleAssignment,
			in:   `password="abc123`,
			want: "password=[REDACTED:assignment]", count: 1,
		},
		{
			name: "unterminated marker", rule: RuleAssignment,
			in:   "token=[REDACTED:vendor-key",
			want: "token=[REDACTED:assignment]", count: 1,
		},
		{
			name: "keyboard is not a key", rule: RuleAssignment,
			in:   "keyboard=us",
			want: "keyboard=us",
		},
		{
			name: "timeout is not a token", rule: RuleAssignment,
			in:   "timeout=30",
			want: "timeout=30",
		},
		{
			name: "tokens used is a count", rule: RuleAssignment,
			in:   "tokens_used=42",
			want: "tokens_used=42",
		},
		{
			name: "token count is a count", rule: RuleAssignment,
			in:   "token_count: 12",
			want: "token_count: 12",
		},
		{
			name: "max tokens is a limit", rule: RuleAssignment,
			in:   "max_tokens=4096",
			want: "max_tokens=4096",
		},
		{
			name: "camel case max tokens", rule: RuleAssignment,
			in:   "maxTokens=4096",
			want: "maxTokens=4096",
		},
		{
			name: "camel case token count", rule: RuleAssignment,
			in:   "tokenCount: 12",
			want: "tokenCount: 12",
		},
		{
			name: "camel case tokens used", rule: RuleAssignment,
			in:   "tokensUsed=42",
			want: "tokensUsed=42",
		},
		{
			name: "input tokens is a count", rule: RuleAssignment,
			in:   "input_tokens=1200",
			want: "input_tokens=1200",
		},
		{
			name: "prompt tokens is a count", rule: RuleAssignment,
			in:   "prompt_tokens: 800",
			want: "prompt_tokens: 800",
		},
		{
			name: "api key len is a credential", rule: RuleAssignment,
			in:   "api_key_len=abcd1234efgh",
			want: "api_key_len=[REDACTED:assignment]", count: 1,
		},
		{
			name: "secret count is a credential", rule: RuleAssignment,
			in:   "secret_count=abcd1234efgh",
			want: "secret_count=[REDACTED:assignment]", count: 1,
		},
		{
			name: "key count is a credential", rule: RuleAssignment,
			in:   "key_count: 12",
			want: "key_count: [REDACTED:assignment]", count: 1,
		},
		{
			name: "bare key is a credential", rule: RuleAssignment,
			in:   "key=cache-key-1",
			want: "key=[REDACTED:assignment]", count: 1,
		},
		{
			name: "cli flag", rule: RuleAssignment,
			in:   "run --api-key=abcd1234efgh --token=zzzz9999",
			want: "run --api-key=[REDACTED:assignment] --token=[REDACTED:assignment]", count: 2,
		},
		{
			name: "query string", rule: RuleAssignment,
			in:   "GET /v1/items?id=7&api_key=abcd1234efgh&page=2",
			want: "GET /v1/items?id=7&api_key=[REDACTED:assignment]&page=2", count: 1,
		},
		{
			name: "run-together names", rule: RuleAssignment,
			in:   "accesskey=AAAA1111 webhooksecret=BBBB2222 sessiontoken=CCCC3333",
			want: "accesskey=[REDACTED:assignment] webhooksecret=[REDACTED:assignment] sessiontoken=[REDACTED:assignment]", count: 3,
		},
		{
			name: "forty hex after credential word", rule: RuleLongToken,
			in:   "token0123456789abcdef0123456789abcdef01234567 and token: 0123456789abcdef0123456789abcdef01234567",
			want: "[REDACTED:long-token] and token: [REDACTED:long-token]", count: 2,
		},
		{
			name: "forty hex commit id kept", rule: RuleLongToken,
			in:   "commit 0123456789abcdef0123456789abcdef01234567",
			want: "commit 0123456789abcdef0123456789abcdef01234567",
		},
		{
			name: "bearer prose kept", rule: RuleAuthHeader,
			in:   "the bearer of good news arrived",
			want: "the bearer of good news arrived",
		},
		{
			name: "home path in backticks brackets and file url", rule: RuleHomePath,
			in:   "see `/Users/lucy/app` and [/home/lucy/x] and file:///Users/lucy/y and <</Users/lucy/z>",
			want: "see `/~/app` and [/~/x] and file:///~/y and <</~/z>", count: 4,
		},
		{
			name: "non-ascii account name removed whole", rule: RuleHomePath,
			in:   "open /Users/josé/app now",
			want: "open /~/app now", count: 1,
		},
		{
			name: "json colon password", rule: RuleAssignment,
			in:   `"password": "hunter2"`,
			want: `"password": "[REDACTED:assignment]"`, count: 1,
		},
		{
			name: "json colon api key", rule: RuleAssignment,
			in:   `"api_key": "0123456789abcdef0123456789abcdef"`,
			want: `"api_key": "[REDACTED:assignment]"`, count: 1,
		},
		{
			name: "api key header", rule: RuleAssignment,
			in:   "X-Api-Key: 0123456789abcdef0123456789abcdef",
			want: "X-Api-Key: [REDACTED:assignment]", count: 1,
		},
		{
			name: "secretary is not a secret", rule: RuleAssignment,
			in:   "secretary=alice",
			want: "secretary=alice",
		},
		{
			name: "passwordless is a flag", rule: RuleAssignment,
			in:   "passwordless=true",
			want: "passwordless=true",
		},
		{
			name: "monkey is not a key", rule: RuleAssignment,
			in:   "monkey=bob",
			want: "monkey=bob",
		},
		{
			name: "author is not a credential", rule: RuleAssignment,
			in:   "author=alice",
			want: "author=alice",
		},
		{
			name: "run together api key", rule: RuleAssignment,
			in:   "apikey=abc123",
			want: "apikey=[REDACTED:assignment]", count: 1,
		},
		{
			name: "camel case key", rule: RuleAssignment,
			in:   "apiKey=abc123",
			want: "apiKey=[REDACTED:assignment]", count: 1,
		},
		{
			name: "underscored pass", rule: RuleAssignment,
			in:   "DB_PASS=hunter2hunter2",
			want: "DB_PASS=[REDACTED:assignment]", count: 1,
		},
		{
			name: "dotted pass", rule: RuleAssignment,
			in:   "db.pass: hunter2hunter2",
			want: "db.pass: [REDACTED:assignment]", count: 1,
		},
		{
			name: "pwd", rule: RuleAssignment,
			in:   "MYSQL_PWD=hunter2hunter2",
			want: "MYSQL_PWD=[REDACTED:assignment]", count: 1,
		},
		{
			name: "fused password", rule: RuleAssignment,
			in:   "PGPASSWORD=hunter2hunter2",
			want: "PGPASSWORD=[REDACTED:assignment]", count: 1,
		},
		{
			name: "fused token", rule: RuleAssignment,
			in:   "GITHUBTOKEN=abcd1234abcd1234",
			want: "GITHUBTOKEN=[REDACTED:assignment]", count: 1,
		},
		{
			name: "flag with its value in the next word", rule: RuleAssignment,
			in:   "mysql --pass hunter2hunter2 -u root",
			want: "mysql --pass [REDACTED:assignment] -u root", count: 1,
		},
		{
			name: "pass flag with a joined value", rule: RuleAssignment,
			in:   "--pass=3",
			want: "--pass=[REDACTED:assignment]", count: 1,
		},
		{
			name: "pass count pair", rule: RuleAssignment,
			in:   "pass=3 fail=1",
			want: "pass=3 fail=1",
		},
		{
			name: "pass count in a test summary", rule: RuleAssignment,
			in:   "tests: pass: 12",
			want: "tests: pass: 12",
		},
		{
			name: "pass count field", rule: RuleAssignment,
			in:   "pass_count=3",
			want: "pass_count=3",
		},
		{
			name: "pass in a list item", rule: RuleAssignment,
			in:   "- pass: 12",
			want: "- pass: 12",
		},
		{
			name: "password length still holds a secret", rule: RuleAssignment,
			in:   "password_len=12",
			want: "password_len=[REDACTED:assignment]", count: 1,
		},
		{
			name: "api key count still holds a key", rule: RuleAssignment,
			in:   "api_key_count=3",
			want: "api_key_count=[REDACTED:assignment]", count: 1,
		},

		// long-token
		{
			name: "base64 run", rule: RuleLongToken,
			in:   "prefix " + strings.Repeat("Q2VjcmV0", 6) + " suffix",
			want: "prefix [REDACTED:long-token] suffix", count: 1,
		},
		{
			name: "base64 blob", rule: RuleLongToken,
			in:   "blob " + strings.Repeat("q8Zk3vX9pL", 4) + " end",
			want: "blob [REDACTED:long-token] end", count: 1,
		},
		{
			name: "long hex", rule: RuleLongToken,
			in:   "value " + hex128,
			want: "value [REDACTED:long-token]", count: 1,
		},
		{
			name: "forty character commit id", rule: RuleLongToken,
			in:   "commit " + hex40,
			want: "commit " + hex40,
		},
		{
			name: "sha256 commit id", rule: RuleLongToken,
			in:   "commit " + hex64,
			want: "commit " + hex64,
		},
		{
			name: "sha256 digest", rule: RuleLongToken,
			in:   "the sha256 digest is " + hex64,
			want: "the sha256 digest is [REDACTED:long-token]", count: 1,
		},
		{
			name: "lower case base64 like", rule: RuleLongToken,
			in:   "blob " + strings.Repeat("qzjxkvzpmw", 4) + "8f3a end",
			want: "blob [REDACTED:long-token] end", count: 1,
		},
		{
			name: "lower case base64url", rule: RuleLongToken,
			in:   "blob " + strings.Repeat("qzjxkvzpmw", 2) + "_" + strings.Repeat("qzjxkvzpmw", 2) + " end",
			want: "blob [REDACTED:long-token] end", count: 1,
		},
		{
			name: "slug", rule: RuleLongToken,
			in:   "a-long-lowercase-slug-with-no-digits-anywhere",
			want: "a-long-lowercase-slug-with-no-digits-anywhere",
		},
		{
			// Kept as designed: a lower-case run of short hyphenated segments
			// reads as a slug, so the run is left as it is.
			name: "short hyphenated segments are a slug", rule: RuleLongToken,
			in:   "the-scrub-keeps-short-hyphenated-slugs-as-they-are",
			want: "the-scrub-keeps-short-hyphenated-slugs-as-they-are",
		},
		{
			// A segment past pathSegmentLimit (16) is a token that holds a
			// separator, so the run redacts. 17 is the first length over it.
			name: "hyphenated run with a long segment redacts", rule: RuleLongToken,
			in:   "blob " + strings.Repeat("q", 17) + "-the-slug-keeps-going-on-and-on end",
			want: "blob [REDACTED:long-token] end", count: 1,
		},
		{
			name: "dated path", rule: RuleLongToken,
			in:   "/2026/10/30/the-fix-for-the-dispatch-clock",
			want: "/2026/10/30/the-fix-for-the-dispatch-clock",
		},
		{
			name: "bead id", rule: RuleLongToken,
			in:   "gt-tpg0p.43",
			want: "gt-tpg0p.43",
		},
		{
			name: "short commit id", rule: RuleLongToken,
			in:   "landed in 1a2b3c4",
			want: "landed in 1a2b3c4",
		},
		{
			name: "version", rule: RuleLongToken,
			in:   "v1.2.3-rc.1",
			want: "v1.2.3-rc.1",
		},

		// email
		{
			name: "address", rule: RuleEmail,
			in:   "contact alice@example.com today",
			want: "contact [REDACTED:email] today", count: 1,
		},
		{
			name: "mention without domain", rule: RuleEmail,
			in:   "user@localhost",
			want: "user@localhost",
		},

		// home-path
		{
			name: "mac home", rule: RuleHomePath,
			in:   "/Users/lucy/Library/Logs/app.log",
			want: "/~/Library/Logs/app.log", count: 1,
		},
		{
			name: "linux home", rule: RuleHomePath,
			in:   "read /home/bob/.config/gt/jev.env",
			want: "read /~/.config/gt/jev.env", count: 1,
		},
		{
			name: "homebrew prefix", rule: RuleHomePath,
			in:   "/opt/homebrew/bin/gt",
			want: "/opt/homebrew/bin/gt",
		},
	}
}

// TestScrubRules runs every rule in isolation against its rows and checks every
// default rule has both a positive and a negative row.
func TestScrubRules(t *testing.T) {
	t.Parallel()
	cases := ruleCases()
	cover := map[string][2]bool{}
	for _, c := range cases {
		entry := cover[c.rule]
		entry[0] = entry[0] || c.count > 0
		entry[1] = entry[1] || c.count == 0
		cover[c.rule] = entry
	}
	for _, rule := range DefaultRules() {
		entry := cover[rule.Name]
		if !entry[0] {
			t.Errorf("rule %q has no positive row", rule.Name)
		}
		if !entry[1] {
			t.Errorf("rule %q has no negative row", rule.Name)
		}
	}

	for _, c := range cases {
		c := c
		t.Run(c.rule+"/"+c.name, func(t *testing.T) {
			t.Parallel()
			s := New(ruleNamed(t, c.rule))
			got, report := s.Scrub(c.in)
			if got != c.want {
				t.Errorf("Scrub(%q) = %q, want %q", c.in, got, c.want)
			}
			if n := report.Count(c.rule); n != c.count {
				t.Errorf("report counts %q %d times, want %d", c.rule, n, c.count)
			}
			if total := report.Total(); total != c.count {
				t.Errorf("report total %d, want %d; the rule counted another name", total, c.count)
			}
		})
	}
}

// ruleNamed returns the default rule called name, so each row runs the rule the
// default list holds.
func ruleNamed(t *testing.T, name string) Rule {
	t.Helper()
	for _, rule := range DefaultRules() {
		if rule.Name == name {
			return rule
		}
	}
	t.Fatalf("no default rule named %q", name)
	return Rule{}
}

// TestScrubDefaultCredentialShapes drives the nine credential shapes the
// scrubber let through (gt-3rq7v) through Default, the rule list the jev
// batteries run, and checks each value is replaced and its name kept.
func TestScrubDefaultCredentialShapes(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, want string }{
		{"env pass", "DB_PASS=hunter2hunter2", "DB_PASS=[REDACTED:assignment]"},
		{"yaml pass", "REDIS_PASS: hunter2hunter2", "REDIS_PASS: [REDACTED:assignment]"},
		{"dotted pass", "db.pass=hunter2hunter2", "db.pass=[REDACTED:assignment]"},
		{"flag pass", "--pass hunter2hunter2", "--pass [REDACTED:assignment]"},
		{"mysql pwd", "MYSQL_PWD=hunter2hunter2", "MYSQL_PWD=[REDACTED:assignment]"},
		{"fused password", "PGPASSWORD=hunter2hunter2", "PGPASSWORD=[REDACTED:assignment]"},
		{"fused token", "GITHUBTOKEN=abcd1234abcd1234", "GITHUBTOKEN=[REDACTED:assignment]"},
		{"cookie", "Cookie: sid=abcdef1234567890", "Cookie: sid=[REDACTED:cookie-header]"},
		{"set cookie", "Set-Cookie: sid=abcdef1234567890; Path=/", "Set-Cookie: sid=[REDACTED:cookie-header]; Path=/"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, report := Default().Scrub(c.in)
			if got != c.want {
				t.Errorf("Scrub(%q) = %q, want %q", c.in, got, c.want)
			}
			if report.Total() != 1 {
				t.Errorf("Scrub(%q) made %d replacements, want 1", c.in, report.Total())
			}
		})
	}
}

// TestScrubDefaultKeepsCounters checks that the pass word does not claim a
// count, and that prose about a cookie is left alone, through Default.
func TestScrubDefaultKeepsCounters(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"pass=3 fail=1",
		"tests: pass: 12",
		"pass_count=3",
		"tokens_used=42",
		"the cookie jar is on the table",
		"cookie: a treat with no pairs",
	} {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, report := Default().Scrub(in)
			if got != in {
				t.Errorf("Scrub(%q) = %q, want it unchanged", in, got)
			}
			if report.Total() != 0 {
				t.Errorf("Scrub(%q) made %d replacements, want 0", in, report.Total())
			}
		})
	}
}

// TestScrubDefaultOneMarkerPerValue checks that a value an earlier rule already
// replaced keeps that marker's bracket instead of gaining a second one.
func TestScrubDefaultOneMarkerPerValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want, rule string
	}{
		{
			name: "vendor key under a credential name",
			in:   "export DEEPSEEK_API_KEY=sk-fixture-value-not-a-real-credential",
			want: "export DEEPSEEK_API_KEY=[REDACTED:vendor-key]",
			rule: RuleVendorKey,
		},
		{
			name: "jwt under a credential name",
			in:   "API_TOKEN=" + syntheticJWT,
			want: "API_TOKEN=[REDACTED:jwt]",
			rule: RuleJWT,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, report := Default().Scrub(c.in)
			if got != c.want {
				t.Errorf("Scrub(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.HasSuffix(got, "]]") {
				t.Errorf("Scrub(%q) = %q ends with two brackets", c.in, got)
			}
			if n := report.Count(c.rule); n != 1 {
				t.Errorf("report counts %q %d times, want 1", c.rule, n)
			}
			if n := report.Count(RuleAssignment); n != 0 {
				t.Errorf("report counts %q %d times, want 0: the value was already a marker", RuleAssignment, n)
			}
		})
	}
}

// TestScrubReport checks the per-rule counts, the sorted rule list, and that a
// clean input reports zero for every rule.
func TestScrubReport(t *testing.T) {
	t.Parallel()
	s := Default()
	in := strings.Join([]string{
		"Authorization: Bearer abcdefgh12345678",
		"first " + syntheticJWT,
		"second eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiI5ODc2NTQzMjEwIn0.abcdefghijklmnopqrstuvwxyz0123456789012345",
		"mail alice@example.com",
	}, "\n")
	_, report := s.Scrub(in)

	want := map[string]int{RuleAuthHeader: 1, RuleJWT: 2, RuleEmail: 1}
	for _, rule := range DefaultRules() {
		if got := report.Count(rule.Name); got != want[rule.Name] {
			t.Errorf("report counts %q %d times, want %d", rule.Name, got, want[rule.Name])
		}
	}
	if got, wantTotal := report.Total(), 4; got != wantTotal {
		t.Errorf("report total %d, want %d", got, wantTotal)
	}
	if got, wantRules := strings.Join(report.Rules(), ","), "auth-header,email,jwt"; got != wantRules {
		t.Errorf("report rules %q, want %q", got, wantRules)
	}

	clean, cleanReport := s.Scrub("the quick brown fox jumps over the lazy dog")
	if clean != "the quick brown fox jumps over the lazy dog" {
		t.Errorf("clean input changed: %q", clean)
	}
	for _, rule := range DefaultRules() {
		if got := cleanReport.Count(rule.Name); got != 0 {
			t.Errorf("clean input reports %q %d times, want 0", rule.Name, got)
		}
	}
	if names := cleanReport.Rules(); len(names) != 0 {
		t.Errorf("clean input reports rules %v, want none", names)
	}
}

// TestScrubSafe checks that odd input neither panics nor takes superlinear
// time, under a bound taken from the test's own deadline.
func TestScrubSafe(t *testing.T) {
	t.Parallel()
	s := Default()

	t.Run("invalid utf8", func(t *testing.T) {
		t.Parallel()
		in := "\xff\xfe token=hunter2 \x80\x81"
		got, report := s.Scrub(in)
		if !strings.Contains(got, marker(RuleAssignment)) {
			t.Errorf("invalid UTF-8 text was not scrubbed: %q", got)
		}
		if strings.Contains(got, "hunter2") {
			t.Errorf("secret survived: %q", got)
		}
		if report.Count(RuleAssignment) != 1 {
			t.Errorf("assignment count %d, want 1", report.Count(RuleAssignment))
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		got, report := s.Scrub("")
		if got != "" || report.Total() != 0 {
			t.Errorf("empty input gave %q with total %d, want empty and 0", got, report.Total())
		}
	})

	t.Run("ten megabytes", func(t *testing.T) {
		t.Parallel()
		const size = 10 << 20
		line := "Authorization: Bearer abcdefgh12345678 mail alice@example.com at /Users/lucy/app\n"
		in := strings.Repeat(line, size/len(line)+1)[:size]
		start := time.Now()
		got, report := s.Scrub(in)
		checkDeadlineBudget(t, start)
		if report.Total() == 0 {
			t.Error("10 MB input reported no replacements")
		}
		for _, leaked := range []string{"abcdefgh12345678", "alice@example.com", "/Users/lucy"} {
			if strings.Contains(got, leaked) {
				t.Errorf("10 MB output still holds %q", leaked)
			}
		}
	})

	t.Run("pathological", func(t *testing.T) {
		t.Parallel()
		in := strings.Repeat("-----BEGIN PRIVATE KEY----- token=", 50000) +
			strings.Repeat("a@", 300000) +
			strings.Repeat("Q2VjcmV0", 100000)
		start := time.Now()
		s.Scrub(in)
		checkDeadlineBudget(t, start)
	})
}

// checkDeadlineBudget fails when the work that started at start has used half
// the time left before the test's deadline. A run without a deadline cannot
// enforce the bound, so it fails rather than passing the check silently.
func checkDeadlineBudget(t *testing.T, start time.Time) {
	t.Helper()
	deadline, ok := t.Deadline()
	if err := deadlineBudgetError(start, time.Now(), deadline, ok); err != nil {
		t.Fatal(err)
	}
}

// deadlineBudgetError is the decision checkDeadlineBudget makes, split out so
// both of its failing branches have a test without needing a real deadline.
func deadlineBudgetError(start, now, deadline time.Time, hasDeadline bool) error {
	if !hasDeadline {
		return errors.New("the test has no deadline, so its time budget went unchecked; run go test with its default timeout")
	}
	elapsed := now.Sub(start)
	if budget := deadline.Sub(start); elapsed > budget/2 {
		return fmt.Errorf("scrubbing took %s, more than half the %s left on the test deadline", elapsed, budget)
	}
	return nil
}

// TestDeadlineBudgetError drives both failing branches of the deadline check,
// so a broken check cannot pass TestScrubSafe silently.
func TestDeadlineBudgetError(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0)
	cases := []struct {
		name        string
		now         time.Time
		deadline    time.Time
		hasDeadline bool
		wantErr     string
	}{
		{
			name:        "no deadline",
			now:         start.Add(time.Second),
			hasDeadline: false,
			wantErr:     "no deadline",
		},
		{
			name:        "over half the budget",
			now:         start.Add(60 * time.Second),
			deadline:    start.Add(100 * time.Second),
			hasDeadline: true,
			wantErr:     "more than half",
		},
		{
			name:        "within the budget",
			now:         start.Add(10 * time.Second),
			deadline:    start.Add(100 * time.Second),
			hasDeadline: true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := deadlineBudgetError(start, c.now, c.deadline, c.hasDeadline)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("deadlineBudgetError() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("deadlineBudgetError() = nil, want an error holding %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("deadlineBudgetError() = %q, want it to hold %q", err, c.wantErr)
			}
		})
	}
}
