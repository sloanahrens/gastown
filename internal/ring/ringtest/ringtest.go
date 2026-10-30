// Package ringtest mints Ring records into a temporary town for other
// packages' tests, so a test that needs a valid, waned or tampered One does
// not hand-build one and get the signing fields subtly wrong (gt-6mlik.1).
package ringtest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/ring"
)

// Kind is the record a Mint call writes.
type Kind string

const (
	// ValidRecord signs a record at now that expires Lease later.
	ValidRecord Kind = "valid"
	// WanedRecord signs a record whose lease ran out a minute before now.
	WanedRecord Kind = "waned"
	// TamperedRecord signs a record and then changes a signed field, so its
	// signature no longer matches.
	TamperedRecord Kind = "tampered"
)

// Valid writes a signed record that is Valid at now and returns it.
func Valid(t testing.TB, townRoot string, key []byte, now time.Time) ring.Record {
	t.Helper()
	return Mint(t, townRoot, key, now, ValidRecord)
}

// Waned writes a signed record whose lease expired before now and returns it.
func Waned(t testing.TB, townRoot string, key []byte, now time.Time) ring.Record {
	t.Helper()
	return Mint(t, townRoot, key, now, WanedRecord)
}

// Tampered writes a record whose signature no longer matches and returns it.
func Tampered(t testing.TB, townRoot string, key []byte, now time.Time) ring.Record {
	t.Helper()
	return Mint(t, townRoot, key, now, TamperedRecord)
}

// Mint writes a record of the given kind into townRoot's ring section,
// creating a minimal mayor/town.json when the town has none, and returns the
// record that was written.
func Mint(t testing.TB, townRoot string, key []byte, now time.Time, kind Kind) ring.Record {
	t.Helper()
	ensureTown(t, townRoot)
	rec := Record(now, kind)
	rec, err := ring.Sign(rec, key)
	if err != nil {
		t.Fatalf("ringtest: signing the record: %v", err)
	}
	if kind == TamperedRecord {
		rec.ForgedBy += " (tampered)"
	}
	if err := ring.Write(townRoot, rec); err != nil {
		t.Fatalf("ringtest: writing the record: %v", err)
	}
	return rec
}

// Record is the unsigned record a Mint call of the given kind starts from.
func Record(now time.Time, kind Kind) ring.Record {
	rec := ring.Record{ForgedAt: now, ForgedBy: "ringtest", ExpiresAt: now.Add(ring.Lease)}
	if kind == WanedRecord {
		rec.ForgedAt = now.Add(-ring.Lease - time.Minute)
		rec.ExpiresAt = rec.ForgedAt.Add(ring.Lease)
	}
	return rec
}

// ensureTown writes a minimal mayor/town.json when townRoot has none, so a
// caller can mint into a directory that is not a town yet. An existing town is
// left exactly as it is.
func ensureTown(t testing.TB, townRoot string) {
	t.Helper()
	path := filepath.Join(townRoot, filepath.FromSlash(config.MachineConfigFile))
	if _, err := os.Stat(path); err == nil {
		return
	}
	town := &config.TownConfig{Type: "town", Version: config.CurrentTownVersion, Name: "ringtest"}
	if err := config.WriteConfigJSON(path, town, 0o600); err != nil {
		t.Fatalf("ringtest: writing %s: %v", path, err)
	}
}
