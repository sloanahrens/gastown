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
	// tokensSegment: the plural word as a whole name segment; countOrFlag: a number or a switch.
	tokensSegment = regexp.MustCompile(`(?i)(^|[^a-z0-9])tokens([^a-z0-9]|$)`)
	countOrFlag   = regexp.MustCompile(`(?i)^(\d+|on|off|true|false)$`)
)

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
// Empty values and placeholders are not. The one shape exemption is a setting
// that counts or switches tokens: the plural word "tokens" as a whole name
// segment (MAX_OUTPUT_TOKENS) with a number or on/off/true/false, and no other
// credential word in the name (API_KEY_TOKENS is not exempt). Under key, secret,
// password or the singular token, no value is exempt by shape.
func isSecret(name, value string) bool {
	if value == "" || strings.EqualFold(value, redactedMarker) || strings.EqualFold(value, "redacted") {
		return false
	}
	return !(tokensSegment.MatchString(name) && countOrFlag.MatchString(value) &&
		!credentialName(tokensSegment.ReplaceAllString(name, "_")))
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
func isValueShapeSplit(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(`,;"'()`, r)
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

// jsonHits walks valid JSON token by token, so every occurrence of a key is seen
// (duplicates included). A string under a credential name is a key hit; every
// other string value, in an object, an array or at the top, is checked by shape.
func jsonHits(data []byte) ([]hit, error) {
	type frame struct {
		object  bool
		wantKey bool
		key     string
		cred    bool // the current key (object) or the key that opened the array
	}
	var stack []*frame
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
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				f := &frame{object: d == '{', wantKey: d == '{'}
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
				out = appendValueShape(out, data, s, end)
			}
		case top.wantKey:
			top.key, top.cred, top.wantKey = s, credentialName(s), false
		default:
			if isString && top.cred && isSecret(top.key, s) {
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
// Valid JSON is walked: a credential-named key's value is a json:key finding, and
// a token-shaped word in any other string value is a value:shape finding. Anything
// else is scanned for NAME=value and "key": "value".
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
