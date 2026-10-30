package ring

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

var (
	testKey  = []byte("a town's signing key")
	forgedAt = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
)

// signed returns the record forgedAt/forgedAt+Lease signed with testKey.
func signed(t *testing.T) Record {
	t.Helper()
	rec, err := Sign(Record{ForgedAt: forgedAt, ExpiresAt: forgedAt.Add(Lease), ForgedBy: "mayor"}, testKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return rec
}

func TestSignThenVerify(t *testing.T) {
	t.Parallel()
	rec := signed(t)
	if rec.HMAC == "" {
		t.Fatal("Sign left the signature empty")
	}
	if err := Verify(rec, testKey); err != nil {
		t.Fatalf("Verify of a signed record = %v, want nil", err)
	}
}

// TestVerifyFailsWhenASignedFieldChanges: the signature covers forged_at,
// expires_at and forged_by, so changing any one of them is a tamper.
func TestVerifyFailsWhenASignedFieldChanges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*Record)
	}{
		{"forged_at", func(r *Record) { r.ForgedAt = r.ForgedAt.Add(-time.Hour) }},
		{"expires_at", func(r *Record) { r.ExpiresAt = r.ExpiresAt.Add(24 * time.Hour) }},
		{"forged_by", func(r *Record) { r.ForgedBy = "someone else" }},
		{"hmac", func(r *Record) {
			if r.HMAC[0] == '0' {
				r.HMAC = "1" + r.HMAC[1:]
			} else {
				r.HMAC = "0" + r.HMAC[1:]
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := signed(t)
			tc.change(&rec)
			if err := Verify(rec, testKey); !errors.Is(err, ErrSignature) {
				t.Fatalf("Verify after changing %s = %v, want ErrSignature", tc.name, err)
			}
		})
	}
}

func TestVerifyRefusesAnotherKey(t *testing.T) {
	t.Parallel()
	rec := signed(t)
	if err := Verify(rec, []byte("another town's key")); !errors.Is(err, ErrSignature) {
		t.Fatalf("Verify under another key = %v, want ErrSignature", err)
	}
}

func TestSignRefuses(t *testing.T) {
	t.Parallel()
	rec := Record{ForgedAt: forgedAt, ExpiresAt: forgedAt.Add(Lease), ForgedBy: "mayor"}
	if _, err := Sign(rec, nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("Sign with no key = %v, want ErrNoKey", err)
	}
	noExpiry := Record{ForgedAt: forgedAt, ForgedBy: "mayor"}
	if _, err := Sign(noExpiry, testKey); !errors.Is(err, ErrNoExpiry) {
		t.Errorf("Sign of a record with no expires_at = %v, want ErrNoExpiry", err)
	}
}

func TestVerifyRefuses(t *testing.T) {
	t.Parallel()
	rec := signed(t)
	if err := Verify(rec, nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("Verify with no key = %v, want ErrNoKey", err)
	}
	unsigned := rec
	unsigned.HMAC = ""
	if err := Verify(unsigned, testKey); !errors.Is(err, ErrUnsigned) {
		t.Errorf("Verify of an unsigned record = %v, want ErrUnsigned", err)
	}
}

// TestStateString: a report names the state and, when it is not Valid, why.
func TestStateString(t *testing.T) {
	t.Parallel()
	valid := State{Kind: Valid}
	if got := valid.String(); got != "valid" {
		t.Errorf("Valid.String() = %q, want %q", got, "valid")
	}
	gone := absent(ReasonNoSection)
	if got := gone.String(); got != "absent ("+ReasonNoSection+")" {
		t.Errorf("Absent.String() = %q", got)
	}
}

// TestCanonicalSeparatesAdjacentFields: the signed form length-prefixes every
// field, so two records that split the same characters differently across a
// boundary sign different bytes. Concatenated bare, these pairs are the same
// input, and moving a boundary would change what a record says while its
// signature still verified.
func TestCanonicalSeparatesAdjacentFields(t *testing.T) {
	t.Parallel()
	const (
		forged  = "2026-10-30T12:00:00Z"
		expires = "2026-10-31T00:00:00Z"
	)
	cases := []struct {
		name string
		a, b []string
	}{
		{
			name: "forged_by moved into the previous field",
			a:    []string{forged, expires, "mayor"},
			b:    []string{forged, expires + "mayor", ""},
		},
		{
			name: "a boundary moved into forged_by's start",
			a:    []string{forged, expires, "mayor"},
			b:    []string{forged, expires + "ma", "yor"},
		},
		{
			name: "a boundary between the two times",
			a:    []string{forged, expires, "mayor"},
			b:    []string{forged + "2026", "10-31T00:00:00Z", "mayor"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if bytes.Equal(encodeFields(tc.a), encodeFields(tc.b)) {
				t.Fatalf("the canonical form is the same for %q and %q", tc.a, tc.b)
			}
			if mac(encodeFields(tc.a), testKey) == mac(encodeFields(tc.b), testKey) {
				t.Errorf("two boundary-shifted field lists share a signature")
			}
		})
	}
}
