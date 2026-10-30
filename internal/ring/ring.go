// Package ring holds the One: the town's signed lease, stored as the "ring"
// section of the machine-owned mayor/town.json. A record signed with the
// town's key and not yet past its expires_at is Valid (full); a signed record
// past it is Waned (standby); nothing a reader can trust is Absent (off),
// carrying the reason it could not be trusted.
//
// Sign and Verify cover HMAC-SHA256 with a key the caller supplies; this
// package never reads one from the environment or from config. Nothing reads
// the Ring for behavior yet (gt-6mlik.5).
package ring

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
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
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical(rec))
	return hex.EncodeToString(mac.Sum(nil))
}

// canonical is the exact bytes a signature covers, so Sign and Verify cannot
// disagree: the three signed fields in a fixed order, one per line, in UTC. A
// field added to Record has to be added here too, or it would ride along
// unsigned.
func canonical(rec Record) []byte {
	var b []byte
	b = append(b, rec.ForgedAt.UTC().Format(time.RFC3339Nano)...)
	b = append(b, '\n')
	b = append(b, rec.ExpiresAt.UTC().Format(time.RFC3339Nano)...)
	b = append(b, '\n')
	b = append(b, rec.ForgedBy...)
	return b
}

// townPath is the machine-owned file the ring section lives in.
func townPath(townRoot string) string {
	return filepath.Join(townRoot, filepath.FromSlash(config.MachineConfigFile))
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
	if len(key) == 0 {
		return State{}, ErrNoKey
	}
	town, err := config.LoadTownConfig(townPath(townRoot))
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
	err = config.UpdateRawJSON(townPath(townRoot), func(tree map[string]any) (bool, error) {
		tree[SectionKey] = json.RawMessage(data)
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("writing the ring: %w", err)
	}
	return nil
}
