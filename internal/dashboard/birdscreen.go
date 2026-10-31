package dashboard

import (
	"regexp"
	"strings"
)

// screenSecret reports whether text a bird wrote looks like it holds a secret,
// so the reports page can withhold it. It is best effort: it is one layer beside
// the redaction rule in the bird-report skill and the overseer's review, and the
// page says so. class names the rule that matched and never contains any matched
// bytes, so a withheld report cannot leak its secret through its label.
func screenSecret(b []byte) (class string, withheld bool) {
	if birdPEMPrivateKey.Match(b) {
		return "private-key", true
	}
	if birdTokenPrefix.Match(b) {
		return "token-prefix", true
	}
	for _, m := range birdCredential.FindAllSubmatch(b, -1) {
		if !birdPlaceholder(m[1]) {
			return "credential-assignment", true
		}
	}
	for _, m := range birdURLPassword.FindAllSubmatch(b, -1) {
		if !birdPlaceholder(m[1]) {
			return "url-credential", true
		}
	}
	return "", false
}

var (
	birdPEMPrivateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

	// The \b keeps a word that ends in a prefix, such as "risk-", from matching.
	birdTokenPrefix = regexp.MustCompile(
		`\b(?:sk-|ghp_|gho_|github_pat_|xoxb-|xoxp-)[A-Za-z0-9_\-]{16,}` +
			`|\bAKIA[0-9A-Z]{16}` +
			`|\bAIza[0-9A-Za-z_\-]{35}`)

	// The keyword may be part of a longer name (db_password, GITHUB_TOKEN), so
	// there is no leading boundary. The value must be 12 or more non-space
	// characters; birdPlaceholder then rejects a masked or elided one.
	birdCredential = regexp.MustCompile(
		`(?i)(?:password|passwd|secret|token|api_key|apikey|pgpassword|authorization:\s*bearer)` +
			`["']?[=: ]+["']?(\S{12,})`)

	birdURLPassword = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s/:@]+:([^\s/@]+)@`)
)

// birdPlaceholder reports whether a matched value is a mask rather than a secret:
// asterisks, dots, a <...> token, or REDACTED.
func birdPlaceholder(v []byte) bool {
	s := strings.Trim(string(v), `"',;`)
	return strings.Trim(s, "*") == "" ||
		strings.Trim(s, ".") == "" ||
		(strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">")) ||
		strings.Contains(strings.ToUpper(s), "REDACTED")
}
