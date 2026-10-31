package keystore

import (
	"encoding/json"
	"fmt"
	"io"
)

// redacted is what every printing path shows in place of a value.
const redacted = "[redacted]"

// Secret is a key's value. String, GoString, Format and MarshalJSON all print
// [redacted], so a stray %v, %s, %+v, %#v or json.Marshal cannot leak it; Bytes
// is the one accessor that returns the value.
type Secret struct {
	v *secretValue
}

// secretValue is the value behind a pointer, so fmt's reflective printing of an
// unexported Secret field shows an address rather than the value.
type secretValue struct {
	value []byte
}

// newSecret copies value into a Secret, so a caller that reuses its buffer
// cannot rewrite a stored revision.
func newSecret(value []byte) Secret {
	return Secret{v: &secretValue{value: append([]byte(nil), value...)}}
}

// Bytes returns a copy of the value.
func (s Secret) Bytes() []byte {
	if s.v == nil {
		return nil
	}
	return append([]byte(nil), s.v.value...)
}

// String stands in for the value under %v and %s.
func (s Secret) String() string { return redacted }

// GoString returns [redacted], so %#v cannot print the value.
func (s Secret) GoString() string { return redacted }

// Format writes [redacted] for every verb.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redacted)
}

// MarshalJSON keeps a struct holding a Secret from encoding the value.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(redacted)
}
