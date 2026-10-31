package scrub

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Rule names, used as the key of a Report and inside each marker.
const (
	// RulePrivateKey replaces a PEM private key block, terminated or not.
	RulePrivateKey = "private-key"
	// RuleAuthHeader replaces an Authorization header value and a Bearer token.
	RuleAuthHeader = "auth-header"
	// RuleCookieHeader replaces the value of each cookie in a Cookie or
	// Set-Cookie header and keeps the cookie's name.
	RuleCookieHeader = "cookie-header"
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
		{Name: RuleCookieHeader, Apply: scrubCookieHeaders},
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
		sm := bearerValue.FindStringSubmatch(m)
		if !tokenish(m[len(sm[1]):]) {
			return m // ordinary prose after the word "bearer"
		}
		n++
		return sm[1] + marker(RuleAuthHeader)
	})
	return out, n
}

// tokenish reports whether a word after "Bearer" reads as a token rather than
// prose: it holds a digit, a token symbol, or both letter cases.
func tokenish(v string) bool {
	var upper, lower bool
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9', r == '.' || r == '_' || r == '~' || r == '+' || r == '/' || r == '=' || r == '-':
			return true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		}
	}
	return upper && lower
}

// cookieHeader matches a Cookie or Set-Cookie header and its value, which runs
// to the end of the line.
var cookieHeader = regexp.MustCompile(`(?i)\b(set-cookie|cookie)([ \t]*:[ \t]*)([^\r\n]+)`)

// cookiePair matches one name=value pair inside a cookie header's value. The
// value ends at a ';', a space or a quote, so the text after a header quoted
// inside a command line stays, and base64 padding inside a value is kept.
var cookiePair = regexp.MustCompile(`([A-Za-z0-9_!#$%&*+.^` + "`" + `|~-]+)=("[^"]*"|[^; \t\r\n"']*)`)

// cookieAttributes name a Set-Cookie attribute, whose value is not a secret, so
// the line still says how long the cookie lives and where it applies.
var cookieAttributes = map[string]bool{
	"comment":     true,
	"domain":      true,
	"expires":     true,
	"httponly":    true,
	"max-age":     true,
	"partitioned": true,
	"path":        true,
	"priority":    true,
	"samesite":    true,
	"secure":      true,
	"version":     true,
}

// scrubCookieHeaders keeps the header and each cookie's name and replaces the
// value, so the line still says which cookie was set.
func scrubCookieHeaders(text string) (string, int) {
	n := 0
	out := cookieHeader.ReplaceAllStringFunc(text, func(m string) string {
		sm := cookieHeader.FindStringSubmatch(m)
		value, k := scrubCookieValue(sm[3], strings.EqualFold(sm[1], "set-cookie"))
		n += k
		return sm[1] + sm[2] + value
	})
	return out, n
}

// scrubCookieValue replaces the value of every cookie in a header value. In a
// Set-Cookie header it keeps an attribute, which is not a secret; a Cookie
// request header holds only cookies, so a cookie named Path is still one.
func scrubCookieValue(value string, setCookie bool) (string, int) {
	n := 0
	out := cookiePair.ReplaceAllStringFunc(value, func(m string) string {
		sm := cookiePair.FindStringSubmatch(m)
		if sm[2] == "" || (setCookie && cookieAttributes[strings.ToLower(sm[1])]) {
			return m
		}
		n++
		return sm[1] + "=" + marker(RuleCookieHeader)
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
// NAME: value assignment. The name may follow a space, a delimiter, a query
// string's ? or &, a bracket or backtick, or the dashes of a command-line flag
// (--api-key=VALUE). The value is consumed separately, so an innocent
// name whose quoted value holds a secret ("config": "password: x") does not
// swallow it.
var assignmentStart = regexp.MustCompile("(^|[\\s;,&?(\\[{`\"'-])([\"']?)([A-Za-z0-9_][A-Za-z0-9_.\\-]*)([\"']?)([ \\t]*[:=][ \\t]*)")

// flagValueStart matches a command-line flag whose value is the next word
// (--pass hunter2), the form an = or : does not cover.
var flagValueStart = regexp.MustCompile("(^|[\\s;,&?(\\[{`\"'])(--?)([A-Za-z0-9_][A-Za-z0-9_.\\-]*)([ \\t]+)")

// assignmentSite is one place a name is followed by a value: valueStart is the
// first byte of the value, name the field or flag, and flag reports whether a
// dash run introduced the name.
type assignmentSite struct {
	valueStart int
	name       string
	flag       bool
}

// credentialSites returns the sites whose name names a credential, in the order
// their values appear.
func credentialSites(text string) []assignmentSite {
	var sites []assignmentSite
	for _, m := range assignmentStart.FindAllStringSubmatchIndex(text, -1) {
		sites = append(sites, assignmentSite{
			valueStart: m[1],
			name:       text[m[6]:m[7]],
			flag:       m[3]-m[2] == 1 && text[m[2]] == '-',
		})
	}
	for _, m := range flagValueStart.FindAllStringSubmatchIndex(text, -1) {
		sites = append(sites, assignmentSite{
			valueStart: m[1],
			name:       text[m[6]:m[7]],
			flag:       true,
		})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].valueStart < sites[j].valueStart })
	var out []assignmentSite
	for _, site := range sites {
		if credentialName(site.name) || (site.flag && barePassName(site.name)) {
			out = append(out, site)
		}
	}
	return out
}

// scrubAssignments replaces the value of an assignment whose name contains a
// credential word, and keeps the name.
func scrubAssignments(text string) (string, int) {
	sites := credentialSites(text)
	if len(sites) == 0 {
		return text, 0
	}
	var b strings.Builder
	n := 0
	last := 0
	for _, site := range sites {
		if site.valueStart < last {
			continue // inside a value this rule already replaced
		}
		b.WriteString(text[last:site.valueStart])
		last = site.valueStart
		if end := markerValue(text, last); end > last {
			// An earlier rule already replaced this value; a second marker
			// would put a stray bracket after the first one.
			b.WriteString(text[last:end])
			last = end
			continue
		}
		if end := quotedMarkerValue(text, last); end > last {
			// A quoted value that holds an earlier rule's marker keeps it whole.
			b.WriteString(text[last:end])
			last = end
			continue
		}
		end, quoted := assignmentValue(text, last)
		if end <= last {
			continue
		}
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
	b.WriteString(text[last:])
	return b.String(), n
}

// markerPattern matches one complete marker: a lower-case rule name between the
// prefix and the closing bracket, with nothing else in between.
var markerPattern = regexp.MustCompile(`^\[REDACTED:[a-z-]+\]`)

// markerValue returns the end of the [REDACTED:rule] marker at pos, and pos
// when no complete marker starts there.
func markerValue(text string, pos int) int {
	m := markerPattern.FindStringIndex(text[pos:])
	if m == nil {
		return pos
	}
	return pos + m[1]
}

// quotedMarkerValue returns the end of a quoted value at pos that holds exactly
// one marker, and pos when the value is not a quoted marker. The earlier rule's
// marker stays whole, so the assignment rule does not write a second one.
func quotedMarkerValue(text string, pos int) int {
	if pos >= len(text) || (text[pos] != '"' && text[pos] != '\'') {
		return pos
	}
	inner := markerValue(text, pos+1)
	if inner == pos+1 || inner >= len(text) || text[inner] != text[pos] {
		return pos
	}
	return inner + 1
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

// credentialWords name a credential on their own; a field name that contains
// one of them as a word holds a credential. A bare "key" is a credential too
// (keyWords): a map key or cache key loses its value to a marker, which is the
// cost of never letting a secret named "key" leave the machine.
var credentialWords = map[string]bool{
	"credential":  true,
	"credentials": true,
	"passwd":      true,
	"password":    true,
	"passwords":   true,
	"secret":      true,
	"secrets":     true,
	"token":       true,
	"tokens":      true,
}

// compoundCredentialNames catch a run-together name ("apikey", "authtoken")
// whose words never got separated. The list is explicit because a suffix test
// would claim "monkey" for ending in "key".
var compoundCredentialNames = map[string]bool{
	"accesskey":       true,
	"apisecret":       true,
	"apitoken":        true,
	"authsecret":      true,
	"bearertoken":     true,
	"clientkey":       true,
	"encryptionkey":   true,
	"secretaccesskey": true,
	"sessiontoken":    true,
	"signingsecret":   true,
	"sshkey":          true,
	"webhooksecret":   true,
	"accesstoken":     true,
	"apikey":          true,
	"apikeys":         true,
	"appsecret":       true,
	"authkey":         true,
	"authtoken":       true,
	"clientsecret":    true,
	"dbpassword":      true,
	"idtoken":         true,
	"masterkey":       true,
	"passphrase":      true,
	"privatekey":      true,
	"refreshtoken":    true,
	"secretkey":       true,
	"signingkey":      true,
	"userpassword":    true,
}

// passWords name a password, but each is also an ordinary word: "pass" counts
// passing tests (pass=3 fail=1, "tests: pass: 12") and "pwd" is a directory in
// a shell, so a name holding one is a credential only beside a second,
// non-counter word (db_pass, mysql_pwd) or as a flag (--pass VALUE) (gt-3rq7v).
var passWords = map[string]bool{"pass": true, "pwd": true}

// credentialSuffixes end a run-together name whose vendor prefix would hide the
// credential word itself (PGPASSWORD, GITHUBTOKEN).
var credentialSuffixes = []string{"password", "passwd", "secret", "token"}

// keyWords name a key: "key", "keys" and any name that holds one as a word
// ("api_key", "secret-key") are credentials.
var keyWords = map[string]bool{"key": true, "keys": true}

// counterWords count something rather than hold it. A name built from a
// credential word plus one of these is a quantity ("tokens_used", "maxTokens",
// "input_tokens"), not a secret, so redacting it would replace signal with a
// marker.
var counterWords = map[string]bool{
	"budget":     true,
	"completion": true,
	"count":      true,
	"counts":     true,
	"input":      true,
	"len":        true,
	"length":     true,
	"limit":      true,
	"limits":     true,
	"max":        true,
	"min":        true,
	"num":        true,
	"number":     true,
	"output":     true,
	"prompt":     true,
	"size":       true,
	"total":      true,
	"used":       true,
}

// countedName reports whether words name a quantity: a counter word beside
// only token(s) ("token_count", "maxTokens", "input_tokens"). Any other
// credential word, and any key word, makes it a credential again: "api_key_len"
// and "key_count" hold a key, so their values are redacted.
func countedName(words []string) bool {
	counted := false
	for _, w := range words {
		if counterWords[w] {
			counted = true
		}
	}
	if !counted {
		return false
	}
	for _, w := range words {
		if (credentialWords[w] || keyWords[w]) && !tokenWords[w] {
			return false
		}
	}
	return true
}

// tokenWords are the credential words a counter may count.
var tokenWords = map[string]bool{"token": true, "tokens": true}

// credentialName reports whether a field name names a credential, however its
// words are separated: api_token, APIKey, db.password, apikey.
//
// A name made of a token word and a counter is a count and is kept; every other
// shape holding a credential or key word is redacted.
func credentialName(name string) bool {
	words := nameWords(name)
	if len(words) == 0 {
		return false
	}
	if countedName(words) {
		return false
	}
	for _, w := range words {
		if credentialWord(w) {
			return true
		}
	}
	return len(words) > 1 && passWordName(words)
}

// credentialWord reports whether one word of a name names a credential: a
// credential, key or run-together name, or a fused name ending in a whole
// credential word after a vendor prefix.
func credentialWord(word string) bool {
	if credentialWords[word] || keyWords[word] || compoundCredentialNames[word] {
		return true
	}
	for _, suffix := range credentialSuffixes {
		if len(word) > len(suffix) && strings.HasSuffix(word, suffix) {
			return true
		}
	}
	return false
}

// passWordName reports whether words hold a pass word beside a word that is not
// a counter.
func passWordName(words []string) bool {
	pass, other := false, false
	for _, w := range words {
		switch {
		case passWords[w]:
			pass = true
		case counterWords[w]:
		default:
			other = true
		}
	}
	return pass && other
}

// barePassName reports whether a flag's name is exactly a pass word, which in a
// flag is the password it takes.
func barePassName(name string) bool {
	words := nameWords(name)
	return len(words) == 1 && passWords[words[0]]
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
// base64, base64url or hex alphabet, or the separators a path or slug holds.
var longRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{40,}`)

// scrubLongTokens replaces long runs shaped like key material and keeps the
// ones that are signal: a path or slug, a bead id, and a git commit id (a bare
// 40-hex run, or a 64-hex run the surrounding line presents as a commit).
func scrubLongTokens(text string) (string, int) {
	matches := longRun.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text, 0
	}
	var b strings.Builder
	b.Grow(len(text))
	n := 0
	last := 0
	for _, m := range matches {
		run := text[m[0]:m[1]]
		if isHex(run) {
			if (len(run) == 40 || (len(run) == 64 && commitContext(text, m[0]))) && !credentialBefore.MatchString(text[:m[0]]) {
				continue
			}
		} else if !looksLikeLongSecret(run) {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(marker(RuleLongToken))
		last = m[1]
		n++
	}
	if n == 0 {
		return text, 0
	}
	b.WriteString(text[last:])
	return b.String(), n
}

// credentialBefore matches a credential word directly before a run, with only
// separators between them: a commit-id-shaped run after "token: " or "key " is
// a secret, not a commit.
var credentialBefore = regexp.MustCompile(`(?i)(?:key|token|secret|password|passwd|credential|auth)[\s"'=:_-]*$`)

// looksLikeLongSecret reports whether a long non-hex run is key material rather
// than a path, slug or id. Mixed case or a base64 symbol is key material, and
// so is any lower-case run that is not a path of short word segments: a
// lower-case-only base64 blob carries no upper case, no base64 symbol and
// possibly few digits, and a secret that reaches the hosted service cannot be
// recovered, so the run redacts.
func looksLikeLongSecret(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r == '+' || r == '=':
			return true
		}
	}
	return !pathShape(s)
}

// pathSegmentLimit is the longest segment a path, slug or id may hold. A longer
// unbroken segment is base64-like, not a word.
const pathSegmentLimit = 16

// pathShape reports whether a run reads as a path, slug or id: lower-case
// letters and digits in short segments joined by separators. A run with a
// segment longer than pathSegmentLimit is a token that happens to hold a
// separator, not a path.
func pathShape(s string) bool {
	separator := false
	segment := 0
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			segment++
			if segment > pathSegmentLimit {
				return false
			}
		case r == '/' || r == '-' || r == '_':
			separator = true
			segment = 0
		default:
			return false
		}
	}
	return separator
}

// commitWords make a 64-character hex run a git object id rather than a digest.
var commitWords = map[string]bool{
	"blob":     true,
	"branch":   true,
	"checkout": true,
	"cherry":   true,
	"commit":   true,
	"commits":  true,
	"git":      true,
	"head":     true,
	"merge":    true,
	"object":   true,
	"oid":      true,
	"parent":   true,
	"pick":     true,
	"rebase":   true,
	"rev":      true,
	"revision": true,
	"tag":      true,
	"tree":     true,
}

// commitContext reports whether the line holding pos presents the run at pos as
// a git object id. A digest written "sha256: <hex>" is not one: its word is
// "sha256", which is not in commitWords.
func commitContext(text string, pos int) bool {
	start := pos - 80
	if start < 0 {
		start = 0
	}
	window := text[start:pos]
	if i := strings.LastIndexByte(window, '\n'); i >= 0 {
		window = window[i+1:]
	}
	for _, word := range nameWords(window) {
		if commitWords[word] {
			return true
		}
	}
	return false
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
// URL's /home/ segment is not mistaken for a home directory. The boundary is
// white space, an opening quote, bracket, backtick or angle bracket, a pipe, a
// separator, or a file:// scheme; the account name is every character up to the
// next delimiter, so a non-ASCII name is removed whole.
var homePath = regexp.MustCompile("(^|[\\s\"'(=:,;`\\[{<>|]|file://)/(?:Users|home)/[^/\\s\"'`)\\]}>,;:]+/?")

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
