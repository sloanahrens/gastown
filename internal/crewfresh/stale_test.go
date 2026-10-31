package crewfresh

import (
	"slices"
	"testing"
	"time"
)

// staleNow is the clock every Stale case is read against, so no case depends
// on when the test runs.
var staleNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// staleDay is the limit the acceptance names: a day without a fetch is stale.
const staleDay = 24 * time.Hour

func TestStaleListsClonesPastTheLimitOldestFirst(t *testing.T) {
	t.Parallel()

	// The paths name each case, so a failure prints which reading it got.
	clones := []Clone{
		{Path: "/crew/23h", LastFetch: staleNow.Add(-23 * time.Hour)},
		{Path: "/crew/24h", LastFetch: staleNow.Add(-staleDay)},
		{Path: "/crew/5d", LastFetch: staleNow.Add(-5 * staleDay)},
		{Path: "/crew/never", LastFetch: time.Time{}},
	}

	got := Stale(clones, staleNow, staleDay)
	want := []string{"/crew/never", "/crew/5d", "/crew/24h"}

	gotPaths := make([]string, len(got))
	for i, c := range got {
		gotPaths[i] = c.Path
	}
	if !slices.Equal(gotPaths, want) {
		t.Fatalf("Stale listed %v, want %v", gotPaths, want)
	}
}

func TestStaleKeepsUnreadableClones(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// clone is the checkout the case reads; only one is passed, so the
		// result says whether this clone came back.
		clone Clone
		want  []Clone
	}{
		{
			name:  "unreadable but freshly fetched",
			clone: Clone{Path: "/crew/a", LastFetch: staleNow.Add(-time.Hour), Err: "read the branch: no such directory"},
			want:  []Clone{{Path: "/crew/a", LastFetch: staleNow.Add(-time.Hour), Err: "read the branch: no such directory"}},
		},
		{
			name:  "unreadable and never fetched",
			clone: Clone{Path: "/crew/b", Err: "read the branch: no such directory"},
			want:  []Clone{{Path: "/crew/b", Err: "read the branch: no such directory"}},
		},
		{
			name:  "readable and freshly fetched",
			clone: Clone{Path: "/crew/c", LastFetch: staleNow.Add(-time.Hour)},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Stale([]Clone{tc.clone}, staleNow, staleDay)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Stale listed %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStaleOrdersSameFetchByPath(t *testing.T) {
	t.Parallel()

	// Both rows have the same age, so only their paths can order them.
	old := staleNow.Add(-5 * staleDay)
	got := Stale([]Clone{
		{Path: "/crew/zebra", LastFetch: old},
		{Path: "/crew/ant", LastFetch: old},
	}, staleNow, staleDay)
	want := []string{"/crew/ant", "/crew/zebra"}

	gotPaths := make([]string, len(got))
	for i, c := range got {
		gotPaths[i] = c.Path
	}
	if !slices.Equal(gotPaths, want) {
		t.Fatalf("Stale listed %v, want %v", gotPaths, want)
	}
}
