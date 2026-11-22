package townstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"

	"github.com/steveyegge/gastown/internal/config"
)

// redactedMarker replaces a credential value; a value that is already the
// marker is not a finding, so RedactJSON can check its own output.
const redactedMarker = "<redacted>"

// A name marks a credential when it contains a credential word in any case,
// unless it ends in an exempt suffix (a place or an indirection, not the secret).
var (
	credentialWords = []string{"key", "token", "secret", "password"}
	exemptSuffixes  = []string{"_file", "_path", "_env", "_name"}
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
	// blockScalarHeader is a mapping value that opens a block scalar: | or > with
	// an optional indentation digit and chomping sign, in either order. The header
	// states how the lines below are read, so it is not a value.
	blockScalarHeader = regexp.MustCompile(`^[|>](?:[1-9][+-]?|[+-][1-9]?)?$`)
	// tokensSegment: the plural word as a whole name segment; countOrFlag: a number or a switch.
	tokensSegment = regexp.MustCompile(`(?i)(^|[^a-z0-9])tokens([^a-z0-9]|$)`)
	countOrFlag   = regexp.MustCompile(`(?i)^(\d+|on|off|true|false)$`)
	// envRefOnly: the whole value is one environment reference, the form the town
	// config uses to name where a token lives instead of holding it. The name
	// follows config's env var rule, so a leading digit (a shell positional) is
	// not a reference (gt-t5pxk8).
	envRefOnly = regexp.MustCompile(`^\$(?:\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)$`)
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

// hit is one credential value in valid JSON: its rule and the byte span of its string literal.
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
	key     string
	cred    bool // the current key (object) or the key that opened the array
	parent  *jsonFrame
	depth   int     // containers above this one
	seg     pathSeg // the step from the parent into this container
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
// every string element of an array that key opened, nested arrays included, unless
// the name rule is exempt at its path; every other string value, in an object, an
// array or at the top, is checked by shape. The walk is linear in the document:
// opening a container costs O(1) and the path test reads three steps.
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
				// A credential-named key owns its array value, so each string element
				// is a finding like a direct value; without this the key list was
				// copied into the backup unredacted. The name exemption is decided
				// once, where the key is read, since the element's frame is the
				// array, not the member that holds it.
				if d == '[' && top != nil && top.cred && (!top.object || !nameRuleExempt(top)) {
					f.key, f.cred = top.key, true
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
		switch {
		case top == nil || !top.object:
			if isString {
				if top != nil && top.cred && isSecret(top.key, s) {
					out = append(out, hit{rule: "json:" + top.key, from: stringStart(data, end), to: end})
				} else {
					out = appendValueShape(out, data, s, end)
				}
			}
		case top.wantKey:
			top.key, top.cred, top.wantKey = s, credentialName(s), false
		default:
			if isString && top.cred && !nameRuleExempt(top) && isSecret(top.key, s) {
				out = append(out, hit{rule: "json:" + top.key, from: stringStart(data, end), to: end})
			} else if isString {
				out = appendValueShape(out, data, s, end)
			}
			top.wantKey = true
		}
	}
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
// Valid JSON is walked: a credential-named key's value is a json:key finding unless
// its path is exempt, and a token-shaped word in any other string value is a
// value:shape finding. Anything else is scanned for NAME=value, "key": "value"
// and a YAML name: value entry, whose last dotted segment decides the name.
func FindCredentials(data []byte) []string {
	var found []string
	if hits, err := jsonHits(data); err == nil {
		for _, h := range hits {
			found = append(found, h.rule)
		}
		return found
	}
	for _, m := range envLine.FindAllSubmatch(data, -1) {
		if credentialName(string(m[1])) && isSecret(string(m[1]), strings.Trim(strings.TrimSpace(string(m[2])), `"'`)) {
			found = append(found, "env:"+string(m[1]))
		}
	}
	for _, m := range jsonMember.FindAllSubmatch(data, -1) {
		if credentialName(string(m[1])) && isSecret(string(m[1]), string(m[2])) {
			found = append(found, "json:"+string(m[1]))
		}
	}
	for _, m := range yamlLine.FindAllSubmatch(data, -1) {
		name := lastSegment(string(m[1]))
		value, quoted := yamlValue(string(m[2]))
		// An unquoted | or > opens a block scalar, so it says nothing about a
		// secret; the block's own lines go through this loop as ordinary lines.
		if !quoted && blockScalarHeader.MatchString(value) {
			continue
		}
		if credentialName(name) && isSecret(name, value) {
			found = append(found, "yaml:"+string(m[1]))
		}
	}
	return found
}

// RedactJSON replaces every credential value with the marker, leaving every other
// byte alone, and returns the count. A token-shaped word in a value that is not
// under a credential name is not rewritten, so it survives into the output and
// the check below refuses the document. It refuses input that is not valid JSON,
// and its own output if a finding or a syntax error survives.
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
