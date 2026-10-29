package rigbootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// errNotObject is returned when a settings value is not a JSON object: the
// merge keeps keys in order, which only an object has.
var errNotObject = errors.New("not a JSON object")

// errTrailing is returned when bytes follow the closing brace of an object.
var errTrailing = errors.New("data after the JSON object")

// member is one key of a JSON object. value is the bytes the file held, so
// numbers and escapes are kept as written.
type member struct {
	key   string
	value json.RawMessage
}

// MergeRoleAgents returns the rig settings in existing with type and version set
// when absent, and role_agents.polecat set to polecat when polecat is non-empty.
// Keys keep their order, new keys append in the order type, version,
// role_agents, and the result is indented two spaces with a final newline. Empty
// existing starts from an empty object. Invalid JSON returns an error and no
// bytes.
func MergeRoleAgents(existing []byte, polecat string) ([]byte, error) {
	var top []member
	if len(existing) > 0 {
		ms, err := parseObject(existing)
		if err != nil {
			return nil, fmt.Errorf("rig settings: %w", err)
		}
		top = ms
	}
	top = setDefault(top, "type", json.RawMessage(`"rig-settings"`))
	top = setDefault(top, "version", json.RawMessage(`1`))

	var roleAgents []member
	if raw, ok := lookup(top, "role_agents"); ok {
		ms, err := parseObject(raw)
		if err != nil {
			return nil, fmt.Errorf("role_agents: %w", err)
		}
		roleAgents = ms
	}
	if polecat != "" {
		roleAgents = setMember(roleAgents, "polecat", encodeString(polecat))
	}
	top = setMember(top, "role_agents", encodeObject(roleAgents))

	// Every byte here came from a parse or an encoder, so it is valid JSON and
	// Indent cannot fail: no input reaches an error branch for it.
	var out bytes.Buffer
	_ = json.Indent(&out, encodeObject(top), "", "  ")
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// parseObject reads data as one JSON object, keeping its keys in file order.
// Input that ends early is reported as io.ErrUnexpectedEOF, not io.EOF, so the
// error names the problem.
func parseObject(data []byte) ([]member, error) {
	ms, err := decodeObject(data)
	if errors.Is(err, io.EOF) {
		return nil, io.ErrUnexpectedEOF
	}
	return ms, err
}

func decodeObject(data []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	open, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := open.(json.Delim); !ok || delim != '{' {
		return nil, errNotObject
	}
	var ms []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		// The decoder only returns a key here: a non-string key is a syntax error.
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		ms = setMember(ms, key, value)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	// The decoder's first object is complete, so anything it reads next makes the
	// file invalid. Only io.EOF means the file ended there.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errTrailing
	}
	return ms, nil
}

// lookup returns the value of key and whether it is present.
func lookup(ms []member, key string) (json.RawMessage, bool) {
	for _, m := range ms {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}

// setMember replaces the value of key in place, or appends key when absent.
// A duplicate key in the file keeps its first position and takes its last value,
// as a Python dict does.
func setMember(ms []member, key string, value json.RawMessage) []member {
	for i := range ms {
		if ms[i].key == key {
			ms[i].value = value
			return ms
		}
	}
	return append(ms, member{key: key, value: value})
}

// setDefault appends key with value when absent and leaves an existing value alone.
func setDefault(ms []member, key string, value json.RawMessage) []member {
	if _, ok := lookup(ms, key); ok {
		return ms
	}
	return append(ms, member{key: key, value: value})
}

// encodeObject writes ms as compact JSON. The values are not re-encoded, so
// their bytes come through unchanged.
func encodeObject(ms []member) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(encodeString(m.key))
		buf.WriteByte(':')
		buf.Write(m.value)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// encodeString quotes s as JSON. Go's default HTML escaping would turn <, > and &
// into \u escapes the settings file never held, so it is off.
func encodeString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = enc.Encode(s)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
