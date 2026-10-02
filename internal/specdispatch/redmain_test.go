package specdispatch

import (
	"testing"
	"time"
)

func TestRedMainHold(t *testing.T) {
	t.Parallel()
	redMain := Spec{ID: "gt-red", Labels: []string{LabelRedMain}}
	labeled := Spec{ID: "gt-other", Labels: []string{"needs-human", LabelRedMain}}
	plain := Spec{ID: "gt-plain", Labels: []string{"pro"}}
	// RedMainHold sees a live revert: resolving staleness is the reader's job
	// (ResolveRevert), tested in TestStaleRevert.
	inFlight := &Revert{Culprit: "gt-cul"}
	for _, tc := range []struct {
		name string
		spec Spec
		rv   *Revert
		want bool
	}{
		{name: "red-main bead held while the revert is in flight", spec: redMain, rv: inFlight, want: true},
		{name: "the label decides, not the rig", spec: labeled, rv: inFlight, want: true},
		{name: "any other bead dispatches", spec: plain, rv: inFlight},
		{name: "no revert is no hold", spec: redMain, rv: nil},
		{name: "a revert with no culprit is no hold", spec: redMain, rv: &Revert{}},
		{name: "whitespace is not a culprit", spec: redMain, rv: &Revert{Culprit: "  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RedMainHold(tc.spec, tc.rv)
			if (got != "") != tc.want {
				t.Fatalf("RedMainHold(%s, %+v) = %q, want held=%v", tc.spec.ID, tc.rv, got, tc.want)
			}
			if tc.want && got != "red-main revert of "+tc.rv.Culprit+" in flight" {
				t.Fatalf("reason %q does not name the culprit", got)
			}
		})
	}
}

// gt-wgyca: a revert record left with no bead by a crashed build, or closed by
// hand, held the rig's red-main beads with nothing to expire it.
func TestStaleRevert(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		rv      *Revert
		stale   bool
		wantWhy string
	}{
		{name: "a bead-less record 5m old is live", rv: &Revert{Culprit: "gt-cul", StartedAt: now.Add(-5 * time.Minute)}},
		{name: "a bead-less record 31m old is stale", rv: &Revert{Culprit: "gt-cul", StartedAt: now.Add(-31 * time.Minute)}, stale: true, wantWhy: "revert of gt-cul has been building for 31 minutes"},
		{name: "a bead-less record with no start time is stale", rv: &Revert{Culprit: "gt-cul"}, stale: true, wantWhy: "revert of gt-cul has no start time recorded"},
		{name: "a filed bead is never stale", rv: &Revert{Culprit: "gt-cul", Bead: "gt-rv", StartedAt: now.Add(-31 * time.Minute)}},
		{name: "no record is not stale", rv: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			why, stale := StaleRevert(tc.rv, now)
			if stale != tc.stale {
				t.Fatalf("StaleRevert(%+v) stale = %v, want %v", tc.rv, stale, tc.stale)
			}
			if why != tc.wantWhy {
				t.Fatalf("StaleRevert(%+v) = %q, want %q", tc.rv, why, tc.wantWhy)
			}
			// ResolveRevert is the dispatcher's entry point: a stale record
			// resolves away to nothing, a live one comes back whole.
			live, resolveWhy := ResolveRevert(tc.rv, now)
			if tc.stale && (live != nil || resolveWhy != tc.wantWhy) {
				t.Fatalf("ResolveRevert(%+v) = %+v, %q; want nil with the reason", tc.rv, live, resolveWhy)
			}
			if !tc.stale && live != tc.rv {
				t.Fatalf("ResolveRevert(%+v) = %+v; want the record back", tc.rv, live)
			}
		})
	}
}

func TestParseRevert(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
		want *Revert
	}{
		{name: "a revert in flight", raw: `{"last_green":"aaa","revert":{"culprit":"gt-cul","bead":"gt-rv"}}`, want: &Revert{Culprit: "gt-cul", Bead: "gt-rv"}},
		{name: "still building, no bead yet", raw: `{"revert":{"culprit":"gt-cul"}}`, want: &Revert{Culprit: "gt-cul"}},
		{name: "the build's start time", raw: `{"revert":{"culprit":"gt-cul","started_at":"2026-09-30T11:29:00Z"}}`, want: &Revert{Culprit: "gt-cul", StartedAt: time.Date(2026, 9, 30, 11, 29, 0, 0, time.UTC)}},
		{name: "no revert recorded", raw: `{"last_green":"aaa","last_run":"bbb"}`, want: nil},
		{name: "empty file", raw: "", want: nil},
		{name: "malformed is no revert", raw: `{"revert":`, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseRevert([]byte(tc.raw))
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("ParseRevert(%q) = %+v, want nil", tc.raw, got)
			case tc.want != nil && (got == nil || got.Culprit != tc.want.Culprit || got.Bead != tc.want.Bead || !got.StartedAt.Equal(tc.want.StartedAt)):
				t.Fatalf("ParseRevert(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}
