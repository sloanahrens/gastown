package land

import (
	"testing"
	"time"
)

const (
	reuseTestHead = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

// reuseTestNow is the instant the predicate is asked at; the record was reviewed a minute before.
func reuseTestNow() time.Time {
	return time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
}

// reuseTestRecord is a record whose six key fields all hold a value.
func reuseTestRecord() AheadRecord {
	return AheadRecord{
		Bead: "gt-abc", Head: reuseTestHead, PatchID: "patch-1", OMVersion: "om 1.2.3",
		OMConfigHash: "cfg-hash", InputDigest: "digest", ReviewedAt: reuseTestNow().Add(-time.Minute),
	}
}

// reuseTestKey is the key a record from reuseTestRecord matches.
func reuseTestKey() ReviewKey {
	return ReviewKey{
		Bead: "gt-abc", Head: reuseTestHead, PatchID: "patch-1", OMVersion: "om 1.2.3",
		OMConfigHash: "cfg-hash", InputDigest: "digest",
	}
}

// TestVerdictReusePredicate: one row per key field where the current key differs
// and where both sides are empty, and an all-equal row that is reusable.
func TestVerdictReusePredicate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(rec *AheadRecord, cur *ReviewKey)
		want string
	}{
		{"Bead differs", func(_ *AheadRecord, c *ReviewKey) { c.Bead = "gt-other" }, missKeyPrefix + "Bead"},
		{"Head differs", func(_ *AheadRecord, c *ReviewKey) { c.Head = "cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34" }, missKeyPrefix + "Head"},
		{"PatchID differs", func(_ *AheadRecord, c *ReviewKey) { c.PatchID = "patch-2" }, missKeyPrefix + "PatchID"},
		{"OMVersion differs", func(_ *AheadRecord, c *ReviewKey) { c.OMVersion = "om 1.2.4" }, missKeyPrefix + "OMVersion"},
		{"OMConfigHash differs", func(_ *AheadRecord, c *ReviewKey) { c.OMConfigHash = "cfg-other" }, missKeyPrefix + "OMConfigHash"},
		{"InputDigest differs", func(_ *AheadRecord, c *ReviewKey) { c.InputDigest = "digest-2" }, missKeyPrefix + "InputDigest"},
		{"Bead empty", func(r *AheadRecord, c *ReviewKey) { r.Bead, c.Bead = "", "" }, missKeyPrefix + "Bead"},
		{"Head empty", func(r *AheadRecord, c *ReviewKey) { r.Head, c.Head = "", "" }, missKeyPrefix + "Head"},
		{"PatchID empty", func(r *AheadRecord, c *ReviewKey) { r.PatchID, c.PatchID = "", "" }, missKeyPrefix + "PatchID"},
		{"OMVersion empty", func(r *AheadRecord, c *ReviewKey) { r.OMVersion, c.OMVersion = "", "" }, missKeyPrefix + "OMVersion"},
		{"OMConfigHash empty", func(r *AheadRecord, c *ReviewKey) { r.OMConfigHash, c.OMConfigHash = "", "" }, missKeyPrefix + "OMConfigHash"},
		{"InputDigest empty", func(r *AheadRecord, c *ReviewKey) { r.InputDigest, c.InputDigest = "", "" }, missKeyPrefix + "InputDigest"},
		{"all equal", func(_ *AheadRecord, _ *ReviewKey) {}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, cur := reuseTestRecord(), reuseTestKey()
			tc.edit(&rec, &cur)
			if got := ReuseMiss(nil, rec, cur, "", nil, reuseTestNow(), time.Hour); got != tc.want {
				t.Errorf("ReuseMiss = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReusePredicateUnusableVersion: an om version that is unknown or dev is a miss,
// and a real version is reusable. An empty version is caught by the key check first.
func TestVerdictReusePredicateUnusableVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{"empty version", "", missKeyPrefix + "OMVersion"},
		{"unknown version", "unknown", missVersion},
		{"dev version", "dev", missVersion},
		{"real version", "om 1.2.3", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, cur := reuseTestRecord(), reuseTestKey()
			rec.OMVersion, cur.OMVersion = tc.version, tc.version
			if got := ReuseMiss(nil, rec, cur, "", nil, reuseTestNow(), time.Hour); got != tc.want {
				t.Errorf("ReuseMiss = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReusePredicateExpired: a record is reusable up to and including its
// expiry, and a miss one nanosecond past it. An expiry of zero or less makes nothing reusable.
func TestVerdictReusePredicateExpired(t *testing.T) {
	t.Parallel()
	const expiry = time.Hour
	cases := []struct {
		name   string
		age    time.Duration
		expiry time.Duration
		want   string
	}{
		{"age equals expiry", expiry, expiry, ""},
		{"age one nanosecond past expiry", expiry + time.Nanosecond, expiry, missAge},
		{"zero expiry, reviewed now", 0, 0, missAge},
		{"negative expiry", time.Minute, -time.Hour, missAge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, cur := reuseTestRecord(), reuseTestKey()
			rec.ReviewedAt = reuseTestNow().Add(-tc.age)
			if got := ReuseMiss(nil, rec, cur, "", nil, reuseTestNow(), tc.expiry); got != tc.want {
				t.Errorf("ReuseMiss = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReusePredicateFutureDated: a record dated after now is a miss, and one
// dated exactly now is reusable.
func TestVerdictReusePredicateFutureDated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		reviewedAt time.Time
		want       string
	}{
		{"reviewed exactly now", reuseTestNow(), ""},
		{"reviewed one nanosecond in the future", reuseTestNow().Add(time.Nanosecond), missFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, cur := reuseTestRecord(), reuseTestKey()
			rec.ReviewedAt = tc.reviewedAt
			if got := ReuseMiss(nil, rec, cur, "", nil, reuseTestNow(), time.Hour); got != tc.want {
				t.Errorf("ReuseMiss = %q, want %q", got, tc.want)
			}
		})
	}
}
