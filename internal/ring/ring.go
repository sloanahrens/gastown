// Package ring holds the One: the town's signed lease, stored as the "ring"
// section of the machine-owned mayor/town.json. A record signed with the
// town's key and not yet past its expires_at is Valid (full); a signed record
// past it is Waned (standby); nothing a reader can trust is Absent (off),
// carrying the reason it could not be trusted.
//
// Sign and Verify cover HMAC-SHA256 with a key the caller supplies; this
// package never reads one from the environment or from config. The bytes the
// signature covers are unambiguous: every signed field is length-prefixed,
// so no two records share a signed form (canonical). Nothing reads the Ring
// for behavior yet (gt-6mlik.5).
package ring

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// Lease is how long a forged Record holds full power.
const Lease = 36 * time.Hour

// SectionKey is the mayor/town.json key the Record is stored under.
const SectionKey = "ring"

// The refusals Sign, Verify and Write return. Read reports the same conditions
// as an Absent state's reason, so a caller that reads never has to tell "no
// ring" from "a ring I cannot trust".
var (
	// ErrNoKey: a signature over no key proves nothing, so a record made or
	// checked with one is refused rather than accepted.
	ErrNoKey = errors.New("ring: no signing key")
	// ErrNoExpiry: a record without expires_at could never wane.
	ErrNoExpiry = errors.New("ring: record has no expires_at")
	// ErrUnsigned: the record carries no signature.
	ErrUnsigned = errors.New("ring: record carries no signature")
	// ErrSignature: the record does not verify against the key.
	ErrSignature = errors.New("ring: record does not verify")
)

// Record is the One's lease as the ring section stores it.
type Record struct {
	ForgedAt  time.Time `json:"forged_at"`
	ExpiresAt time.Time `json:"expires_at"`
	ForgedBy  string    `json:"forged_by"`
	HMAC      string    `json:"hmac"`
}

// Sign returns rec with HMAC set to its signature under key.
func Sign(rec Record, key []byte) (Record, error) {
	if len(key) == 0 {
		return Record{}, ErrNoKey
	}
	if rec.ExpiresAt.IsZero() {
		return Record{}, ErrNoExpiry
	}
	rec.HMAC = signature(rec, key)
	return rec, nil
}

// Verify returns nil when rec's HMAC is its signature under key.
func Verify(rec Record, key []byte) error {
	if len(key) == 0 {
		return ErrNoKey
	}
	if rec.HMAC == "" {
		return ErrUnsigned
	}
	if !hmac.Equal([]byte(signature(rec, key)), []byte(rec.HMAC)) {
		return ErrSignature
	}
	return nil
}

// signature is rec's HMAC under key, hex encoded.
func signature(rec Record, key []byte) string {
	return mac(canonical(rec), key)
}

// signedFields is what a signature covers, in signing order: the three signed
// fields, the times in UTC. A field added to Record has to be added here too,
// or it would ride along unsigned.
func signedFields(rec Record) []string {
	return []string{
		rec.ForgedAt.UTC().Format(time.RFC3339Nano),
		rec.ExpiresAt.UTC().Format(time.RFC3339Nano),
		rec.ForgedBy,
	}
}

// canonical is the exact bytes a signature covers, so Sign and Verify cannot
// disagree.
func canonical(rec Record) []byte {
	return encodeFields(signedFields(rec))
}

// encodeFields is canonical's form: each field prefixed by its length as an
// unsigned varint, then the field's bytes. The length prefix is what makes
// the form unambiguous. Concatenated bare, two records that split the same
// bytes differently — one field ending where the next begins — would sign the
// same input, and a caller need only move a boundary to change what a record
// says while keeping its signature valid.
func encodeFields(fields []string) []byte {
	var b []byte
	for _, field := range fields {
		b = binary.AppendUvarint(b, uint64(len(field)))
		b = append(b, field...)
	}
	return b
}

// mac is key's HMAC-SHA256 over input, hex encoded.
func mac(input, key []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(input)
	return hex.EncodeToString(h.Sum(nil))
}

// hostPath is the file the ring section lives in: the host the layout
// registers for SectionKey. A layout that does not register the section is a
// build that cannot store a ring, so Read and Write refuse rather than guess a
// file.
func hostPath(townRoot string, hostOf func(key string) (host string, ok bool)) (string, error) {
	host, ok := hostOf(SectionKey)
	if !ok {
		return "", fmt.Errorf("ring: section %q is not registered in the config layout", SectionKey)
	}
	return filepath.Join(townRoot, filepath.FromSlash(host)), nil
}

// The distinct reasons an Absent or Waned State carries. One reason per way a
// reader can end up without a record it can trust, so a caller can report the
// cause rather than "no ring".
const (
	// ReasonNoSection: mayor/town.json holds no "ring" key.
	ReasonNoSection = "no ring section in " + config.MachineConfigFile
	// ReasonUnparseable: the ring section is not a record.
	ReasonUnparseable = "the ring section is not a record"
	// ReasonNoExpiry: the record carries no expires_at.
	ReasonNoExpiry = "the record has no expires_at"
	// ReasonUnsigned: the record carries no signature.
	ReasonUnsigned = "the record carries no signature"
	// ReasonBadSignature: the record's signature does not verify under the key.
	ReasonBadSignature = "the record does not verify under the town's key"
	// ReasonExpired: the signed lease is past its expires_at.
	ReasonExpired = "the lease has expired"
)

// Kind is the Ring's state at one instant.
type Kind string

const (
	// Valid: a record signed with the town's key, before its expires_at.
	Valid Kind = "valid"
	// Waned: such a record at or after its expires_at. The town stands by.
	Waned Kind = "waned"
	// Absent: no record a caller can trust. The town is off.
	Absent Kind = "absent"
)

// State is the Ring at one instant: its Kind, the record it was read from
// (nil when Absent), and why it is not Valid.
type State struct {
	Kind   Kind
	Record *Record
	Reason string
}

// String names the state for a report: "valid", "waned (the lease has
// expired)" or "absent (no ring section in mayor/town.json)".
func (s State) String() string {
	if s.Reason == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + " (" + s.Reason + ")"
}

// absent is the State for a record a caller cannot trust.
func absent(reason string) State {
	return State{Kind: Absent, Reason: reason}
}

// Read returns the Ring's state at now, reading the ring section through the
// config kernel. An error means the town's own config could not be read; a
// section that is missing, unparseable, unsigned or tampered reads as Absent
// with its reason, never as Valid.
func Read(townRoot string, key []byte, now time.Time) (State, error) {
	return read(townRoot, config.SectionHost, key, now)
}

// read is Read over a section lookup, so a test can drive the refusal no
// build with the layout's registration in place can reach.
func read(townRoot string, hostOf func(key string) (host string, ok bool), key []byte, now time.Time) (State, error) {
	if len(key) == 0 {
		return State{}, ErrNoKey
	}
	path, err := hostPath(townRoot, hostOf)
	if err != nil {
		return State{}, err
	}
	town, err := config.LoadTownConfig(path)
	if err != nil {
		// A town whose config cannot be read is not a town with no ring: the
		// caller has to act on it, so it is an error and not Absent.
		return State{}, fmt.Errorf("reading the ring: %w", err)
	}
	if len(town.Ring) == 0 {
		return absent(ReasonNoSection), nil
	}
	if bytes.Equal(bytes.TrimSpace(town.Ring), []byte("null")) {
		return absent(ReasonUnparseable), nil
	}
	var rec Record
	if err := json.Unmarshal(town.Ring, &rec); err != nil {
		return absent(ReasonUnparseable), nil
	}
	if rec.ExpiresAt.IsZero() {
		return absent(ReasonNoExpiry), nil
	}
	switch err := Verify(rec, key); {
	case errors.Is(err, ErrUnsigned):
		return absent(ReasonUnsigned), nil
	case err != nil:
		return absent(ReasonBadSignature), nil
	}
	if !now.Before(rec.ExpiresAt) {
		return State{Kind: Waned, Record: &rec, Reason: ReasonExpired}, nil
	}
	return State{Kind: Valid, Record: &rec}, nil
}

// Write stores rec as the town's ring section through the config kernel's
// atomic writer, leaving every other section of mayor/town.json as it is.
func Write(townRoot string, rec Record) error {
	return write(townRoot, config.SectionHost, rec)
}

// write is Write over a section lookup, so a test can drive the refusal no
// build with the layout's registration in place can reach.
func write(townRoot string, hostOf func(key string) (host string, ok bool), rec Record) error {
	if rec.ExpiresAt.IsZero() {
		return ErrNoExpiry
	}
	if rec.HMAC == "" {
		return ErrUnsigned
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding the ring record: %w", err)
	}
	path, err := hostPath(townRoot, hostOf)
	if err != nil {
		return err
	}
	err = config.UpdateRawJSON(path, func(tree map[string]any) (bool, error) {
		tree[SectionKey] = json.RawMessage(data)
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("writing the ring: %w", err)
	}
	return nil
}

// Remove deletes the ring section from mayor/town.json, leaving every other
// section as it is. It reports false when the town had no ring section to
// delete.
func Remove(townRoot string) (bool, error) {
	return remove(townRoot, config.SectionHost)
}

// remove is Remove over a section lookup, so a test can drive the refusal no
// build with the layout's registration in place can reach.
func remove(townRoot string, hostOf func(key string) (host string, ok bool)) (bool, error) {
	path, err := hostPath(townRoot, hostOf)
	if err != nil {
		return false, err
	}
	removed := false
	err = config.UpdateRawJSON(path, func(tree map[string]any) (bool, error) {
		_, removed = tree[SectionKey]
		if !removed {
			return false, nil
		}
		delete(tree, SectionKey)
		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("removing the ring: %w", err)
	}
	return removed, nil
}
