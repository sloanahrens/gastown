package scrub

import (
	"regexp"
	"strings"
	"unicode"
)

// Rule names, used as the key of a Report and inside each marker.
const (
	// RulePrivateKey replaces a PEM private key block, terminated or not.
	RulePrivateKey = "private-key"
	// RuleAuthHeader replaces an Authorization header value and a Bearer token.
	RuleAuthHeader = "auth-header"
	// RuleURLUserInfo replaces the user:password part before a URL's host.
	RuleURLUserInfo = "url-userinfo"
	// RuleJWT replaces a JWT-shaped token.
	RuleJWT = "jwt"
	// RuleVendorKey replaces a key in a known vendor's format (sk-, ghp_, ...).
	RuleVendorKey = "vendor-key"
	// RuleAssignment replaces the value of a credential-named assignment and
	// keeps the name, so the line still reads as the field it used to hold.
	RuleAssignment = "assignment"
	// RuleLongToken replaces a long base64-like or hex string that is not a
	// git commit id.
	RuleLongToken = "long-token"
	// RuleEmail replaces an email address.
	RuleEmail = "email"
	// RuleHomePath rewrites /Users/NAME/ and /home/NAME/ to /~/ so the name on
	// the account does not leave the machine.
	RuleHomePath = "home-path"
)

// marker is the fixed text a rule leaves behind; the name keeps a report
// readable without keeping the secret.
func marker(rule string) string {
	return "[REDACTED:" + rule + "]"
}

// DefaultRules returns the rules in the order Default applies them.
//
// Order is load-bearing: an assignment runs before the long-token rule so a
// hex value beside a credential name is replaced rather than kept as a commit
// id, and the URL rule runs before the email rule so a URL's user info is not
// half-matched as an address.
func DefaultRules() []Rule {
	return []Rule{
		{Name: RulePrivateKey, Apply: scrubPrivateKeys},
		{Name: RuleAuthHeader, Apply: scrubAuthHeaders},
		{Name: RuleURLUserInfo, Apply: scrubURLUserInfo},
		{Name: RuleJWT, Apply: scrubJWT},
		{Name: RuleVendorKey, Apply: scrubVendorKeys},
		{Name: RuleAssignment, Apply: scrubAssignments},
		{Name: RuleLongToken, Apply: scrubLongTokens},
		{Name: RuleEmail, Apply: scrubEmails},
		{Name: RuleHomePath, Apply: scrubHomePaths},
	}
}

// privateKeyBlock matches a PEM private key through its END line, or through
// the end of the text when the END line is missing: a truncated paste leaks the
// key material in its body just the same.
var privateKeyBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)

// scrubPrivateKeys replaces every private key block with one marker.
func scrubPrivateKeys(text string) (string, int) {
	n := 0
	out := privateKeyBlock.ReplaceAllStringFunc(text, func(string) string {
		n++
		return marker(RulePrivateKey)
	})
	return out, n
}

// authHeaderValue matches an Authorization (or Proxy-Authorization) header and
// its value, which runs to the end of the line.
var authHeaderValue = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization[ \t]*:[ \t]*)[^\r\n]+`)

// bearerValue matches a Bearer token that does not sit under an Authorization
// header, such as the argument of a curl -H flag.
var bearerValue = regexp.MustCompile(`(?i)\b(bearer[ \t]+)[A-Za-z0-9._~+/=-]{8,}`)

// scrubAuthHeaders keeps the header or scheme name and replaces the value.
func scrubAuthHeaders(text string) (string, int) {
	n := 0
	out := authHeaderValue.ReplaceAllStringFunc(text, func(m string) string {
		n++
		return authHeaderValue.FindStringSubmatch(m)[1] + marker(RuleAuthHeader)
	})
	out = bearerValue.ReplaceAllStringFunc(out, func(m string) string {
		n++
		return bearerValue.FindStringSubmatch(m)[1] + marker(RuleAuthHeader)
	})
	return out, n
}

// urlUserInfo matches the user:password part between a URL scheme and its host.
var urlUserInfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]+@`)

// scrubURLUserInfo keeps the scheme and host and replaces the credentials.
func scrubURLUserInfo(text string) (string, int) {
	n := 0
	out := urlUserInfo.ReplaceAllStringFunc(text, func(m string) string {
		n++
		return urlUserInfo.FindStringSubmatch(m)[1] + marker(RuleURLUserInfo) + "@"
	})
	return out, n
}

// jwtToken matches a JWT: a base64url header that starts eyJ (the encoding of
// `{"`), a payload, and a signature, each at least eight characters.
var jwtToken = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)

// scrubJWT replaces every JWT-shaped token.
func scrubJWT(text string) (string, int) {
	n := 0
	out := jwtToken.ReplaceAllStringFunc(text, func(string) string {
		n++
		return marker(RuleJWT)
	})
	return out, n
}

// vendorKey matches the key formats the sandbox skeleton treats as credentials
// (internal/sandbox/skeleton.go, knownKeyShape). The shapes are copied as data
// rather than imported, so this package does not depend on the sandbox.
var vendorKey = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|sk_[A-Za-z0-9_]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|dckr_pat_[A-Za-z0-9_-]{20,}|hvs\.[A-Za-z0-9_-]{20,})`)

// scrubVendorKeys replaces every known vendor key shape.
func scrubVendorKeys(text string) (string, int) {
	n := 0
	out := vendorKey.ReplaceAllStringFunc(text, func(string) string {
		n++
		return marker(RuleVendorKey)
	})
	return out, n
}

// assignmentStart matches the NAME and the separator of a NAME=value or
// NAME: value assignment. The value is consumed separately, so an innocent
// name whose quoted value holds a secret ("config": "password: x") does not
// swallow it.
var assignmentStart = regexp.MustCompile(`(^|[\s;,&("'])(["']?)([A-Za-z0-9_][A-Za-z0-9_.\-]*)(["']?)([ \t]*[:=][ \t]*)`)

// scrubAssignments replaces the value of an assignment whose name contains a
// credential word, and keeps the name.
func scrubAssignments(text string) (string, int) {
	matches := assignmentStart.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, 0
	}
	var b strings.Builder
	n := 0
	last := 0
	for _, m := range matches {
		if m[0] < last {
			continue // inside a value this rule already replaced
		}
		b.WriteString(text[last:m[1]])
		last = m[1]
		if !credentialName(text[m[6]:m[7]]) {
			continue
		}
		if end, quoted := assignmentValue(text, last); end > last {
			if quoted {
				b.WriteString(text[last : last+1])
				b.WriteString(marker(RuleAssignment))
				b.WriteString(text[end-1 : end])
			} else {
				b.WriteString(marker(RuleAssignment))
			}
			n++
			last = end
		}
	}
	b.WriteString(text[last:])
	return b.String(), n
}

// assignmentValue returns the end of the value that starts at pos and whether
// it is quoted; end equals pos when no value follows.
func assignmentValue(text string, pos int) (end int, quoted bool) {
	if pos >= len(text) {
		return pos, false
	}
	if q := text[pos]; q == '"' || q == '\'' {
		for i := pos + 1; i < len(text); i++ {
			switch {
			case text[i] == '\\' && q == '"':
				i++
			case text[i] == q:
				if i == pos+1 {
					return pos, false // an empty value holds nothing
				}
				return i + 1, true
			case text[i] == '\n' || text[i] == '\r':
				return i, false // an unterminated quote still hides its value
			}
		}
		return len(text), false
	}
	for i := pos; i < len(text); i++ {
		if strings.ContainsRune(" \t\r\n\"';&,)]}", rune(text[i])) {
			return i, false
		}
	}
	return len(text), false
}

// credentialWords name a credential on their own; a name carrying one of them
// is an assignment to a credential.
var credentialWords = map[string]bool{
	"credential":  true,
	"credentials": true,
	"key":         true,
	"keys":        true,
	"passwd":      true,
	"password":    true,
	"passwords":   true,
	"secret":      true,
	"secrets":     true,
	"token":       true,
	"tokens":      true,
}

// credentialSuffixes catch a run-together name ("apikey", "authtoken") whose
// words never got separated. "keyboard" ends in none of them.
var credentialSuffixes = []string{
	"credential", "credentials", "key", "keys", "passwd", "password",
	"passwords", "secret", "secrets", "token", "tokens",
}

// credentialName reports whether a field name names a credential, however its
// words are separated: api_token, APIKey, db.password, apikey.
func credentialName(name string) bool {
	words := nameWords(name)
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		if credentialWords[w] {
			return true
		}
	}
	runTogether := strings.Join(words, "")
	for _, suffix := range credentialSuffixes {
		if strings.HasSuffix(runTogether, suffix) {
			return true
		}
	}
	return false
}

// nameWords splits a field name on non-alphanumeric runes and at camelCase
// boundaries, returning lower-case words.
func nameWords(name string) []string {
	chunks := strings.FieldsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var words []string
	for _, chunk := range chunks {
		start := 0
		runes := []rune(chunk)
		for i := 1; i < len(runes); i++ {
			prev, cur := runes[i-1], runes[i]
			boundary := unicode.IsLower(prev) && unicode.IsUpper(cur)
			if !boundary && unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
				boundary = true
			}
			if boundary {
				words = append(words, strings.ToLower(string(runes[start:i])))
				start = i
			}
		}
		words = append(words, strings.ToLower(string(runes[start:])))
	}
	return words
}

// longRun matches a run long enough to be a bare key: 40 or more characters of
// base64, base64url or hex alphabet.
var longRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{40,}`)

// scrubLongTokens replaces long runs that look like key material. A run is kept
// when it is a bare git commit id (40 or 64 hex characters), because the id is
// signal a battery reads, and when it reads as a word or a slug.
func scrubLongTokens(text string) (string, int) {
	n := 0
	out := longRun.ReplaceAllStringFunc(text, func(m string) string {
		if !looksLikeLongSecret(m) {
			return m
		}
		n++
		return marker(RuleLongToken)
	})
	return out, n
}

// looksLikeLongSecret reports whether a long run is shaped like key material.
func looksLikeLongSecret(s string) bool {
	if isHex(s) {
		return len(s) != 40 && len(s) != 64
	}
	var upper, lower, digits int
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= 'a' && r <= 'z':
			lower++
		case r >= '0' && r <= '9':
			digits++
		}
	}
	if upper > 0 && lower > 0 {
		return true
	}
	// An all-lower-case run is a word or a slug unless many digits say it is
	// not; an all-upper-case run is a word too, and the vendor rule holds the
	// upper-case key formats.
	return upper == 0 && lower > 0 && digits >= 8
}

// isHex reports whether s is non-empty and made only of hex digits.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// emailAddress matches an address with a dotted domain; a bare word@host is
// left alone because it is a mention, not a person.
var emailAddress = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z]{2,})+`)

// scrubEmails replaces every email address.
func scrubEmails(text string) (string, int) {
	n := 0
	out := emailAddress.ReplaceAllStringFunc(text, func(string) string {
		n++
		return marker(RuleEmail)
	})
	return out, n
}

// homePath matches an absolute home-directory path only at a boundary, so a
// URL's /home/ segment is not mistaken for a home directory.
var homePath = regexp.MustCompile(`(^|[\s"'(=:,;])/(?:Users|home)/[A-Za-z0-9._-]+/?`)

// scrubHomePaths rewrites a home-directory path to /~/ and drops the account
// name, keeping the rest of the path readable.
func scrubHomePaths(text string) (string, int) {
	n := 0
	out := homePath.ReplaceAllStringFunc(text, func(m string) string {
		n++
		return homePath.FindStringSubmatch(m)[1] + "/~/"
	})
	return out, n
}
