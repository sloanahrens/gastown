// Package doltcred keeps a Dolt credential out of every printed, encoded and
// logged output (gt-1r3vt2).
package doltcred

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// secretMask is what every redacted form of a Secret shows in place of the
// value.
const secretMask = "<redacted>"

// Secret holds a credential so that no fmt verb, JSON or text encoder or slog
// record prints it; Reveal is the one reader.
//
// The value sits behind a pointer, so even where fmt cannot call Secret's
// methods — a Secret in an unexported field, which fmt reaches by reflection —
// it prints the pointer's address, never the bytes. One residual remains: a
// dumper that follows pointers by reflection, go-spew style, still reads the
// bytes; fmt, encoding/json, encoding.TextMarshaler and slog do not.
type Secret struct {
	p *string
}

// NewSecret returns a Secret holding a copy of s.
func NewSecret(s string) Secret {
	return Secret{p: &s}
}

// Reveal returns the value. The zero Secret reveals the empty string.
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

// Format writes the mask for every verb, so no verb prints the value.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, secretMask)
}

// String returns the mask, for callers that reach the Stringer interface
// without going through Format.
func (s Secret) String() string { return secretMask }

// GoString returns the mask, for %#v printed without Format.
func (s Secret) GoString() string { return secretMask }

// MarshalJSON keeps a Secret, or a struct holding one, from encoding the value.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(secretMask)
}

// MarshalText keeps any text encoder, including a JSON map key, from encoding
// the value.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(secretMask), nil
}

// LogValue makes slog record the mask in place of the value.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(secretMask)
}
