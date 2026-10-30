package scrub

import (
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
			in:   "key ghp_0123456789abcdefghijABCDEFGHIJ012345 here",
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
			name: "slug", rule: RuleLongToken,
			in:   "a-long-lowercase-slug-with-no-digits-anywhere",
			want: "a-long-lowercase-slug-with-no-digits-anywhere",
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

	t.Run("five megabytes", func(t *testing.T) {
		t.Parallel()
		const size = 5 << 20
		line := "Authorization: Bearer abcdefgh12345678 mail alice@example.com at /Users/lucy/app\n"
		in := strings.Repeat(line, size/len(line)+1)[:size]
		start := time.Now()
		got, report := s.Scrub(in)
		checkDeadlineBudget(t, start)
		if report.Total() == 0 {
			t.Error("5 MB input reported no replacements")
		}
		for _, leaked := range []string{"abcdefgh12345678", "alice@example.com", "/Users/lucy"} {
			if strings.Contains(got, leaked) {
				t.Errorf("5 MB output still holds %q", leaked)
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
	if !ok {
		t.Fatal("the test has no deadline, so its time budget went unchecked; run go test with its default timeout")
	}
	if elapsed := time.Since(start); elapsed > deadline.Sub(start)/2 {
		t.Fatalf("scrubbing took %s, more than half the %s left on the test deadline", elapsed, deadline.Sub(start))
	}
}
