package townstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/steveyegge/gastown/internal/config"
)

// redactedMarker replaces a credential value; a value that is already the
// marker is not a finding, so RedactJSON can check its own output.
const redactedMarker = "<redacted>"

// A name marks a credential when it contains key, token, secret or password in
// any case, or when one of its words, split on underscores, hyphens, dots and
// camel-case humps, equals passwd, pwd, authorization, credentials or dsn. The
// whole-word rule keeps author, authority, oauth_scope and pwdx out. An exempt
// suffix (a place or an indirection, not the secret) wins over both.
var (
	credentialWords      = []string{"key", "token", "secret", "password"}
	credentialWholeWords = []string{"passwd", "pwd", "authorization", "credentials", "dsn"}
	exemptSuffixes       = []string{"_file", "_path", "_env", "_name"}
)

var (
	envLine = regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	// jsonMember scans text that is not valid JSON, so a truncated document is never skipped.
	jsonMember = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"[ \t\r\n]*:[ \t\r\n]*"((?:[^"\\]|\\.)*)"`)
	// yamlLine matches a mapping entry in text that is not valid JSON: an
	// optional list marker and a key, a colon and a space, then the rest of the
	// line as the value. The key starts with a letter or underscore, so a JSON
	// member, whose key is quoted, is left to jsonMember.
	yamlLine = regexp.MustCompile(`(?m)^[ \t]*(?:-[ \t]+)?([A-Za-z_][A-Za-z0-9_.-]*)[ \t]*:[ \t]+(.*)$`)
	// tokensSegment: the plural word as a whole name segment; countOrFlag: a number or a switch.
	tokensSegment = regexp.MustCompile(`(?i)(^|[^a-z0-9])tokens([^a-z0-9]|$)`)
	countOrFlag   = regexp.MustCompile(`(?i)^(\d+|on|off|true|false)$`)
	// envRefOnly: the whole value is one environment reference, the form the town
	// config uses to name where a token lives instead of holding it. The name
	// follows config's env var rule, so a leading digit (a shell positional) is
	// not a reference (gt-t5pxk8).
	envRefOnly = regexp.MustCompile(`^\$(?:\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)$`)
	// userinfoPasswordShape: scheme://user:password@ at the start of the value. It
	// is the fail-closed reading of a URL url.Parse REJECTS, so a literal password
	// in the text is still a finding: the user has no @, /, colon or space, and
	// the password is non-empty and may hold a slash or a space (the characters
	// that make url.Parse fail). It is never applied to a URL that parses.
	userinfoPasswordShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^@/\s:]+:[^@]+@`)
)

// lastSegment returns the last dotted segment of a YAML key. Only that segment
// decides whether the key is a credential, so dolt.password is a password while
// password.count is a count.
func lastSegment(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// yamlValue returns the value of a mapping entry and whether it was quoted: a
// quoted string without its quotes, or the text up to a trailing # comment. A
// line in a block scalar is read like any other, so a continuation line shaped
// like an entry is a finding on its own.
func yamlValue(raw string) (value string, quoted bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", false
	}
	if q := v[0]; q == '"' || q == '\'' {
		for i := 1; i < len(v); i++ {
			if q == '"' && v[i] == '\\' {
				i++
				continue
			}
			if v[i] == q {
				return v[1:i], true
			}
		}
		return v[1:], true
	}
	for i := 0; i < len(v); i++ {
		if v[i] == '#' && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t') {
			v = v[:i]
			break
		}
	}
	return strings.TrimSpace(v), false
}

// splitNameWords splits a name into lower-case words at an underscore, a hyphen
// or a dot, at a lower-case or digit followed by an upper-case letter (apiKey is
// api and key), and before the last upper-case letter of an upper-case run that
// a lower-case letter follows (DBPasswd is db and passwd, HTTPSProxy is https and
// proxy). X-Authorization is x and authorization.
func splitNameWords(name string) []string {
	runes := []rune(name)
	var (
		words []string
		cur   []rune
	)
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	for i, r := range runes {
		if r == '_' || r == '-' || r == '.' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			lowerToUpper := unicode.IsLower(prev) || unicode.IsDigit(prev)
			acronymBreak := unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if lowerToUpper || acronymBreak {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

func credentialName(name string) bool {
	lower := strings.ToLower(name)
	for _, s := range exemptSuffixes {
		if strings.HasSuffix(lower, s) {
			return false
		}
	}
	for _, w := range credentialWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	for _, word := range splitNameWords(name) {
		for _, w := range credentialWholeWords {
			if word == w {
				return true
			}
		}
	}
	return false
}

// urlCarriesPassword reports whether a string value is a URL whose userinfo holds
// a password (scheme://user:password@host). It judges the value alone, so the
// finding holds under any key; a URL with a user and no password, or no userinfo,
// is not one. It fails closed: url.Parse rejects an invalid percent escape and
// ends the authority at an unescaped slash, so a value shaped like a userinfo
// password is a finding even when the parsed URL shows none (gt-vvuewi).
func urlCarriesPassword(s string) bool {
	if !strings.Contains(s, "://") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return userinfoPasswordShape.MatchString(s)
	}
	if u.User != nil {
		if password, ok := u.User.Password(); ok && password != "" {
			return true
		}
	}
	// A URL that parses is judged by what it parsed to: https://host:443/@scope/pkg
	// has a port and an @ in its path and no userinfo, so it is not a finding.
	return false
}

// isSecret reports whether value, held under a credential name, is reportable.
// Empty values and placeholders are not. A value that is only an environment
// reference names where the secret lives, so it is not one. A field named
// exactly keys lists names, not a credential, unless its value is token-shaped
// by config.LooksLikeSecret; a longer name (api_keys) gets no such exemption.
// The one shape exemption is a setting that counts or switches tokens: the
// plural word "tokens" as a whole name segment (MAX_OUTPUT_TOKENS) with a
// number or on/off/true/false, and no other credential word in the name
// (API_KEY_TOKENS is not exempt). Under key, secret, password or the singular
// token, no value is exempt by shape.
func isSecret(name, value string) bool {
	if value == "" || strings.EqualFold(value, redactedMarker) || strings.EqualFold(value, "redacted") {
		return false
	}
	if envRefOnly.MatchString(value) {
		return false
	}
	if strings.EqualFold(name, "keys") && !config.LooksLikeSecret("", value) {
		return false
	}
	return !(tokensSegment.MatchString(name) && countOrFlag.MatchString(value) &&
		!credentialName(tokensSegment.ReplaceAllString(name, "_")))
}

// nameRuleExempt reports whether the string value held under top's current key
// is a field whose name reads as a credential but is not one: a Kubernetes
// toleration's key under any parent, and the daemon's om_token_meta descriptor at
// the document root. The path is matched exactly, by the last three steps and the
// depth, never by walking from the root; no other key, depth or name shape is
// exempt. Only the name rule is skipped, so a token-shaped value in either field
// is still a value:shape finding. By design a real password that is not
// token-shaped, held in one of those two fields, passes the name rule.
func nameRuleExempt(top *jsonFrame) bool {
	if top.key == "om_token_meta" && top.depth == 1 && top.seg == (pathSeg{key: "daemon"}) {
		return true
	}
	return top.key == "key" && top.seg == (pathSeg{index: true}) && top.parent != nil && top.parent.seg == (pathSeg{key: "tolerations"})
}

// pathSeg is one step of a JSON path: an object member's key, or an array
// position. They are told apart by type, not by spelling: an object whose member
// is named "[*]" is a key, and no key equals an index.
type pathSeg struct {
	key   string
	index bool
}

// hit is one credential value in valid JSON: its rule and the byte span of its
// literal, a string or a number.
type hit struct {
	rule     string
	from, to int
}

// valueShapeRule names a token-shaped word in a string value, whatever its key.
// RedactJSON never rewrites it: the document is refused instead.
const valueShapeRule = "value:shape"

// isValueShapeSplit separates the words of a string value that a token can hide in.
// A token joined to its name by = or :, or set in brackets, is its own word.
func isValueShapeSplit(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(`,;"'()=:[]{}<>`, r)
}

// appendValueShape adds one hit per word of s that is a token by shape. The span
// is the string literal that ends before end, and no word is kept, so no part of a
// token can reach a finding.
func appendValueShape(out []hit, data []byte, s string, end int) []hit {
	for _, word := range strings.FieldsFunc(s, isValueShapeSplit) {
		if config.LooksLikeSecret("", word) {
			out = append(out, hit{rule: valueShapeRule, from: stringStart(data, end), to: end})
		}
	}
	return out
}

// jsonFrame is one object or array the walk is inside. Its place in the document
// is a parent link, its depth and the step that led into it, not a copied path:
// opening a container is O(1), so a document nested to the decoder's limit is
// scanned in time proportional to its size.
type jsonFrame struct {
	object  bool
	wantKey bool
	key     string // the current key (object) or the member key that opened the array
	cred    bool   // the current key of an object names a credential
	owned   bool   // the value this container holds sits under a credential-named key
	owner   string // that credential key, when owned
	parent  *jsonFrame
	depth   int     // containers above this one
	seg     pathSeg // the step from the parent into this container
}

// ownerKey reports the credential-named key that owns the value f holds next, and
// whether there is one. That is the member key an object is positioned on, or, for
// a value inside a credential's value, the key that owns the whole value. A nil f
// (the top level) owns nothing.
func (f *jsonFrame) ownerKey() (string, bool) {
	if f == nil {
		return "", false
	}
	if f.owned {
		return f.owner, true
	}
	if f.object && f.cred && !nameRuleExempt(f) {
		return f.key, true
	}
	return "", false
}

// stepInto is the step a value held by f takes: its member key in an object, an
// index in an array.
func (f *jsonFrame) stepInto() pathSeg {
	if f.object {
		return pathSeg{key: f.key}
	}
	return pathSeg{index: true}
}

// jsonHits walks valid JSON token by token, so every occurrence of a key is seen
// (duplicates included). A string under a credential name is a key hit, and so is
// every string leaf of the value that key owns: each string in a nested object or
// array under it, at any depth, is a hit under that key, unless the name rule is
// exempt at the key's path. A number in either place is a key hit too, under the
// value rules isSecret applies; a boolean and a null are not. Every other string
// value, in an object, an array or at the top, is a key hit when it is a URL with a
// userinfo password, and is otherwise checked by shape. The walk is linear in the
// document: opening a container costs O(1) and the path test reads three steps.
func jsonHits(data []byte) ([]hit, error) {
	var stack []*jsonFrame
	var out []hit
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		end := int(dec.InputOffset())
		var top *jsonFrame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				f := &jsonFrame{object: d == '{', wantKey: d == '{', parent: top}
				if top != nil {
					f.depth, f.seg = top.depth+1, top.stepInto()
				}
				// An array's elements name the member key that opened it, carried
				// through nested arrays, so a URL with a password in an element is
				// a finding under that key.
				if d == '[' && top != nil {
					f.key = top.key
				}
				// A credential-named key owns the whole value under it, so a nested
				// object or array inside that value is owned by the same key and its
				// leaves are findings like a direct value; without this the key list
				// was copied into the backup unredacted (gt-ourmlj, gt-pwfy1z). The
				// name exemption is applied by ownerKey at the key's position, so an
				// exempt key owns nothing and its nested values are not owned either.
				if owner, ok := top.ownerKey(); ok {
					f.owned, f.owner = true, owner
				}
				stack = append(stack, f)
			default:
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].wantKey = true
				}
			}
			continue
		}
		s, isString := tok.(string)
		// The decoder is built without UseNumber, so a JSON number decodes to a float64.
		_, isNumber := tok.(float64)
		switch {
		case top == nil || !top.object:
			owner, owned := top.ownerKey()
			if isString {
				switch {
				case owned && isSecret(owner, s):
					out = append(out, hit{rule: "json:" + owner, from: stringStart(data, end), to: end})
				case urlCarriesPassword(s):
					// A top-level string has no key, so its rule names an empty one.
					key := ""
					if top != nil {
						key = top.key
					}
					out = append(out, hit{rule: "json:" + key, from: stringStart(data, end), to: end})
				default:
					out = appendValueShape(out, data, s, end)
				}
			} else if isNumber && owned {
				if h, ok := numberHit(owner, data, end); ok {
					out = append(out, h)
				}
			}
		case top.wantKey:
			top.key, top.cred, top.wantKey = s, credentialName(s), false
		default:
			owner, owned := top.ownerKey()
			if isString {
				switch {
				case owned && isSecret(owner, s):
					out = append(out, hit{rule: "json:" + owner, from: stringStart(data, end), to: end})
				case urlCarriesPassword(s):
					out = append(out, hit{rule: "json:" + top.key, from: stringStart(data, end), to: end})
				default:
					out = appendValueShape(out, data, s, end)
				}
			} else if isNumber && owned {
				if h, ok := numberHit(owner, data, end); ok {
					out = append(out, h)
				}
			}
			top.wantKey = true
		}
	}
}

// numberStart returns the offset of the first byte of the JSON number literal that
// ends at end. A number holds only digits, a sign, a decimal point and an exponent
// marker, so the scan stops at the delimiter before it (: , [ { or a space).
func numberStart(data []byte, end int) int {
	start := end
	for start > 0 {
		c := data[start-1]
		if !(c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E') {
			break
		}
		start--
	}
	return start
}

// numberHit reports a number literal held under the credential key owner as a
// finding, unless isSecret refuses the value. It is judged on the literal text, so a
// number is exempt exactly where a string under the same key is (a token count, a
// field named keys). Booleans and null never reach it (gt-6seq32).
func numberHit(owner string, data []byte, end int) (hit, bool) {
	start := numberStart(data, end)
	if !isSecret(owner, string(data[start:end])) {
		return hit{}, false
	}
	return hit{rule: "json:" + owner, from: start, to: end}, true
}

// stringStart returns the offset of the opening quote of the string ending before end.
func stringStart(data []byte, end int) int {
	for i := end - 2; i >= 0; i-- {
		if data[i] != '"' {
			continue
		}
		slashes := 0
		for j := i - 1; j >= 0 && data[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			return i
		}
	}
	return 0
}

// FindCredentials returns one rule name per credential in data, never a value.
// Valid JSON is walked: a string or a number under a credential-named key, or nested
// anywhere inside the value that key owns, is a json:key finding unless its path is
// exempt, a string value that is a URL with a userinfo password is a json:key finding
// under any key, and a token-shaped word in any other string value is a value:shape
// finding. Anything else is scanned for
// NAME=value, "key": "value" and a YAML name: value entry, whose last dotted segment
// decides the name; an env value or a quoted JSON member that is a URL with a userinfo
// password is a finding under env:name or json:key as well.
func FindCredentials(data []byte) []string {
	var found []string
	if hits, err := jsonHits(data); err == nil {
		for _, h := range hits {
			found = append(found, h.rule)
		}
		return found
	}
	for _, m := range envLine.FindAllSubmatch(data, -1) {
		name := string(m[1])
		value := strings.Trim(strings.TrimSpace(string(m[2])), `"'`)
		if (credentialName(name) && isSecret(name, value)) || urlCarriesPassword(value) {
			found = append(found, "env:"+name)
		}
	}
	for _, m := range jsonMember.FindAllSubmatch(data, -1) {
		name, value := string(m[1]), string(m[2])
		if (credentialName(name) && isSecret(name, value)) || urlCarriesPassword(value) {
			found = append(found, "json:"+name)
		}
	}
	for _, m := range yamlLine.FindAllSubmatch(data, -1) {
		name := lastSegment(string(m[1]))
		value, _ := yamlValue(string(m[2]))
		// A block scalar header (| or >) is a non-empty value under a credential
		// name, so it is a finding: the secret sits in the lines below it, which
		// this loop cannot read as the value, and refusing the header fails closed.
		if credentialName(name) && isSecret(name, value) {
			found = append(found, "yaml:"+string(m[1]))
		}
	}
	return found
}

// RedactJSON replaces every credential value with the marker, leaving every other
// byte alone, and returns the count. That includes a number under a credential name,
// every string and number leaf nested inside the value of one, and a URL with a
// userinfo password, whatever its key. A token-shaped word in a value that is not
// under a credential name and is not such a URL is not rewritten,
// so it survives into the output and the check below refuses the document. It
// refuses input that is not valid JSON, and its own output if a finding or a syntax
// error survives.
func RedactJSON(data []byte) (out []byte, n int, err error) {
	if !json.Valid(data) {
		return nil, 0, errors.New("townstate: input is not valid JSON, refusing to redact it")
	}
	hits, err := jsonHits(data)
	if err != nil {
		return nil, 0, fmt.Errorf("townstate: reading JSON: %w", err)
	}
	var buf bytes.Buffer
	last := 0
	for _, h := range hits {
		if h.rule == valueShapeRule {
			continue
		}
		buf.Write(data[last:h.from])
		buf.WriteString(`"` + redactedMarker + `"`)
		last = h.to
		n++
	}
	buf.Write(data[last:])
	out = buf.Bytes()
	if !json.Valid(out) || len(FindCredentials(out)) > 0 {
		return nil, 0, errors.New("townstate: redaction left a credential or broke the document, refusing the output")
	}
	return out, n, nil
}
