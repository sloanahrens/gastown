package dashboard

import (
	"strings"
	"testing"
)

// Positive inputs are joined from pieces so that no secret-looking literal sits
// in this file for the repository's own scanners to flag.
var birdSecretRows = []struct {
	name  string
	text  string
	class string
}{
	{"PEM private key header", "-----BEGIN " + "RSA " + "PRIVATE KEY-----\nMIIEpAIBAAKCAQEA", "private-key"},
	{"sk- prefix at the 16-character floor", "sk-" + strings.Repeat("a", 16), "token-prefix"},
	{"ghp_ prefix", "ghp_" + strings.Repeat("B", 36), "token-prefix"},
	{"gho_ prefix", "gho_" + strings.Repeat("c", 36), "token-prefix"},
	{"github_pat_ prefix", "github_pat_" + strings.Repeat("D", 22), "token-prefix"},
	{"xoxb- prefix", "xoxb-" + strings.Repeat("1", 20), "token-prefix"},
	{"xoxp- prefix", "xoxp-" + strings.Repeat("2", 20), "token-prefix"},
	{"AWS access key id", "AKIA" + strings.Repeat("A", 8) + strings.Repeat("2", 8), "token-prefix"},
	{"Google API key", "AIza" + strings.Repeat("x", 35), "token-prefix"},
	{"password assignment", "password=" + strings.Repeat("k", 16), "credential-assignment"},
	{"passwd with colon", "passwd: " + strings.Repeat("q", 16), "credential-assignment"},
	{"secret with space", "secret " + strings.Repeat("9", 16), "credential-assignment"},
	{"token assignment", "token=" + strings.Repeat("t", 16), "credential-assignment"},
	{"api_key with colon", "api_key: " + strings.Repeat("m", 16), "credential-assignment"},
	{"apikey assignment", "apikey=" + strings.Repeat("n", 16), "credential-assignment"},
	{"PGPASSWORD assignment", "PGPASSWORD=" + strings.Repeat("p", 16), "credential-assignment"},
	{"Authorization Bearer header", "Authorization: Bearer " + strings.Repeat("z", 16), "credential-assignment"},
	{"spaced credential with digits", "password " + "hunter2" + "hunter2", "credential-assignment"},
	{"spaced credential of one all-letter word", "password " + "correcthorse" + "batterystaple", "credential-assignment"},
	{"spaced credential of letters, more words after", "password " + "correcthorse" + "batterystaple" + " horse", "credential-assignment"},
	{"REDACTED inside a real value", "password=" + strings.Repeat("k", 4) + "REDACTED" + strings.Repeat("k", 4), "credential-assignment"},
	{"redaction marker inside a real value", "password=" + strings.Repeat("k", 4) + "[REDACTED:db-password]" + strings.Repeat("k", 4), "credential-assignment"},
	{"URL with a password", "https://deploy:" + strings.Repeat("p", 12) + "@git.example.com", "url-credential"},
	{"postgres URL with a password", "postgres://app:" + strings.Repeat("9", 12) + "@db.internal:5432/main", "url-credential"},
}

var birdOrdinaryRows = []struct {
	name string
	text string
}{
	{"40-hex git sha", "3c9a1f0e7b2d54a6c8e91f7b0d3a2c6e4f8b1d59"},
	{"64-hex digest", "9f2c4b7a1e8d0c3f6a5b2d9e4c7f1a8b3d6e0c2f5a9b4d7e1c8f3a6b0d2e5c79"},
	{"bead id", "gt-cqpz8"},
	{"uuid", "3f2b9c8e-1a4d-4e7f-9b6a-2c5d8e1f0a3b"},
	{"password reset flow", "password reset flow"},
	{"token budget", "token budget"},
	{"masked PGPASSWORD", "PGPASSWORD=***"},
	{"redacted password", "password=<redacted>"},
	{"angle-bracket placeholder value", "password=<redacted-by-the-overseer>"},
	{"asterisk-only value", "PGPASSWORD=********************"},
	{"masked URL password", "https://deploy:***@git.example.com"},
	{"sk- prefix one character short", "sk-" + strings.Repeat("a", 15)},
	{"word that ends in sk-", "risk-assessment-framework"},
	{"masked REDACTED value", "password=***REDACTED***"},
	{"redaction marker value", "PGPASSWORD=[REDACTED:db-password]"},
}

// birdProseWithheldByDesign is prose the spaced rule withholds on purpose. A
// keyword followed by a space and a long word cannot be told from a passphrase,
// so the rule withholds it. These rows pin that trade-off; see birdSpaced.
var birdProseWithheldByDesign = []struct {
	name string
	text string
}{
	{"keyword before a long word", "token authentication flow"},
	{"keyword before a long word and a comma", "token authentication, then retry"},
	{"keyword before a long word and a period", "token authentication."},
	{"keyword before a hyphenated word", "token non-expiring-session"},
	{"keyword before a word with an apostrophe", "secret author's-notebook"},
	{"Bearer header before a prose word", "Authorization: Bearer authentication-flow"},
}

func TestBirdScreenWithholdsProseByDesign(t *testing.T) {
	t.Parallel()

	for _, row := range birdProseWithheldByDesign {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			if _, withheld := screenSecret([]byte(row.text)); !withheld {
				t.Errorf("screenSecret(%q) withheld = false, want true (accepted false positive)", row.text)
			}
		})
	}
}

func TestBirdScreenWithholdsEachSecretShape(t *testing.T) {
	t.Parallel()

	for _, row := range birdSecretRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			class, withheld := screenSecret([]byte(row.text))
			if !withheld {
				t.Fatalf("screenSecret(%s) withheld = false, want true", row.name)
			}
			if class != row.class {
				t.Errorf("screenSecret(%s) class = %q, want %q", row.name, class, row.class)
			}
		})
	}
}

func TestBirdScreenLeavesOrdinaryReportText(t *testing.T) {
	t.Parallel()

	for _, row := range birdOrdinaryRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			if _, withheld := screenSecret([]byte(row.text)); withheld {
				t.Errorf("screenSecret(%q) withheld = true, want false", row.text)
			}
		})
	}
}

// birdClasses is the closed set of labels screenSecret may return. Set membership
// catches a label that carries any input bytes, because such a label is not in
// the set.
var birdClasses = map[string]bool{
	"private-key":           true,
	"token-prefix":          true,
	"credential-assignment": true,
	"url-credential":        true,
}

func TestBirdScreenNeverEchoesTheMatch(t *testing.T) {
	t.Parallel()

	const runLen = 8
	for _, row := range birdSecretRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			class, _ := screenSecret([]byte(row.text))
			if !birdClasses[class] {
				t.Errorf("screenSecret(%s) class = %q, not a known label; it may echo the input", row.name, class)
			}
			if strings.Contains(class, row.text) {
				t.Fatalf("class %q contains the whole input", class)
			}
			for i := 0; i+runLen <= len(row.text); i++ {
				run := row.text[i : i+runLen]
				if strings.Contains(class, run) {
					t.Errorf("class %q contains the %d-byte run %q of the input", class, runLen, run)
				}
			}
		})
	}
}
