package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

func TestListCursorRoundTrips(t *testing.T) {
	t.Parallel() // unit tier: pure
	at := time.Date(2026, 10, 25, 11, 30, 0, 123456000, time.UTC)
	c := listCursor{Priority: 1, CreatedAt: at, ID: "gt-abc12"}
	got, err := decodeListCursor(c.encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != 1 || !got.CreatedAt.Equal(at) || got.ID != "gt-abc12" {
		t.Errorf("round trip = %+v, want %+v (microseconds must survive)", got, c)
	}
}

func TestListCursorIsOpaqueAndRejectsGarbage(t *testing.T) {
	t.Parallel()
	enc := listCursor{Priority: 2, CreatedAt: time.Now(), ID: "gt-x"}.encode()
	if strings.ContainsAny(enc, "{}\" ") {
		t.Errorf("cursor %q looks like readable JSON; it should be opaque", enc)
	}
	for _, bad := range []string{"not-base64!!", "e30", "bm90LWpzb24"} { // garbage, {}, "not-json"
		if _, err := decodeListCursor(bad); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("decodeListCursor(%q) = %v, want api.ErrInvalid", bad, err)
		}
	}
	if got, err := decodeListCursor(""); err != nil || got != nil {
		t.Errorf("decodeListCursor(\"\") = %v, %v; want nil, nil (the first page)", got, err)
	}
}

func TestPageSize(t *testing.T) {
	t.Parallel()
	for limit, want := range map[int]int{0: defaultPageSize, -5: defaultPageSize, 7: 7, maxPageSize: maxPageSize, maxPageSize + 1: maxPageSize} {
		if got := pageSize(limit); got != want {
			t.Errorf("pageSize(%d) = %d, want %d", limit, got, want)
		}
	}
}

func TestInfraTypesAreTheOnesBDHides(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"agent", "role", "message"} {
		if !isInfraType(typ) {
			t.Errorf("%s should be an infrastructure type: bd list hides it by default", typ)
		}
	}
	for _, typ := range []string{"task", "bug", "epic", "molecule", "convoy", "event"} {
		if isInfraType(typ) {
			t.Errorf("%s is not an infrastructure type", typ)
		}
	}
}

func TestListCursorSurvivesATimeInAZoneWithSecondsInItsOffset(t *testing.T) {
	t.Parallel()
	// Postgres returns year 1 in the session zone with its local-mean-time
	// offset, -05:50:36 for Chicago. RFC 3339 text cannot carry the seconds, so
	// a cursor encoded in that zone used to decode to another instant.
	lmt := time.FixedZone("LMT", -(5*3600 + 50*60 + 36))
	at := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).In(lmt)
	for name, in := range map[string]time.Time{"year one in LMT": at, "recent in UTC-5": time.Date(2026, 10, 25, 11, 30, 0, 5000, time.FixedZone("CDT", -5*3600))} {
		got, err := decodeListCursor(listCursor{Priority: 1, CreatedAt: in, ID: "gt-a"}.encode())
		if err != nil || !got.CreatedAt.Equal(in) {
			t.Errorf("%s: cursor decoded to %v (%v), want the same instant as %v", name, got, err, in)
		}
	}
}
