package land

import "time"

// ReviewKey names the review a stored verdict is matched against: the bead, the
// head and its patch-id, the om that ran and its config, and the digest of the
// inputs om read. A record is reusable only when all six fields match.
type ReviewKey struct {
	Bead, Head, PatchID, OMVersion, OMConfigHash, InputDigest string
}

// Miss reasons. ReuseMiss returns one of these, so callers and tests compare
// the name, not the prose. A key miss is missKeyPrefix followed by the field name.
const (
	missKeyPrefix = "key:"
	missVersion   = "version"
	missFuture    = "future"
	missAge       = "age"
)

// ReuseMiss returns "" when rec, a stored review, may stand in for a review
// keyed by cur, and otherwise the reason it may not. It is the one test for
// both reuse and invalidity, so the two cannot disagree (gt-6bsp). now is a
// parameter: nothing here reads the clock. g, base and touched feed the checks
// after these three, and are unused here.
func ReuseMiss(g Repo, rec AheadRecord, cur ReviewKey, base string, touched []string, now time.Time, expiry time.Duration) string {
	fields := []struct{ name, rec, cur string }{
		{"Bead", rec.Bead, cur.Bead},
		{"Head", rec.Head, cur.Head},
		{"PatchID", rec.PatchID, cur.PatchID},
		{"OMVersion", rec.OMVersion, cur.OMVersion},
		{"OMConfigHash", rec.OMConfigHash, cur.OMConfigHash},
		{"InputDigest", rec.InputDigest, cur.InputDigest},
	}
	for _, f := range fields {
		if f.rec == "" || f.rec != f.cur {
			return missKeyPrefix + f.name
		}
	}
	if !usableOMVersion(cur.OMVersion) {
		return missVersion
	}
	// An expiry of zero or less makes nothing reusable, so a mis-set config fails closed.
	age := now.Sub(rec.ReviewedAt)
	switch {
	case age < 0:
		return missFuture
	case expiry <= 0 || age > expiry:
		return missAge
	}
	return ""
}

// usableOMVersion reports whether v names an om whose verdicts can be reused:
// not empty, not the unknown placeholder, and not a dev build.
func usableOMVersion(v string) bool {
	switch v {
	case "", omVersionUnknown, "dev":
		return false
	}
	return true
}
