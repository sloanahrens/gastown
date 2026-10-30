package ring

import (
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
