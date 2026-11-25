package land

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// bundleWithRoom is an open bundle of internal/land/note.go for followupDay that
// has room for room more findings: its labels carry MaxAcceptance-room
// fingerprints.
func bundleWithRoom(room int) ExistingFollowup {
	b := openBundleFor(followupDay, "internal/land/note.go", "open")
	for i := 1; i < specdispatch.MaxAcceptance-room; i++ {
		b.Labels = append(b.Labels, fmt.Sprintf("%sfill%d", FingerprintPrefix, i))
	}
	return b
}

// numberedFindings is n findings of one package, titled "finding 1" to "finding n".
func numberedFindings(n int) []Finding {
	var fs []Finding
	for i := 1; i <= n; i++ {
		fs = append(fs, minorAt("internal/land/note.go", i, fmt.Sprintf("finding %d", i)))
	}
	return fs
}

// fingerprints counts the om-fp labels of a bead.
func fingerprints(labels []string) int {
	n := 0
	for _, l := range labels {
		if strings.HasPrefix(l, FingerprintPrefix) {
			n++
		}
	}
	return n
}

// TestFeedOverflowsIntoANewBundle: a group larger than the room left in the open
// bundle fills the bundle by an append and files the rest as new bundles of at
// most MaxAcceptance findings each (gt-ntkwh0): five findings with room for two
// append two and create one bundle of three; twelve with room for five append five
// and create 6 and 1. The landing's comment names the beads.
func TestFeedOverflowsIntoANewBundle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		n, room      int
		appended     int
		createdSizes []int
	}{
		{"five findings with room for two", 5, 2, 2, []int{3}},
		{"twelve findings with room for five", 12, 5, 5, []int{6, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, numberedFindings(tc.n))
			f.fake.existing = []ExistingFollowup{bundleWithRoom(tc.room)}
			log := f.run()
			if len(f.fake.appended) != 1 || len(f.fake.appended[0].Add.Labels) != tc.appended {
				t.Fatalf("appended %+v, want one append of %d findings:\n%s", f.fake.appended, tc.appended, log)
			}
			var sizes []int
			for _, spec := range f.fake.created {
				sizes = append(sizes, fingerprints(spec.Labels))
			}
			if !slices.Equal(sizes, tc.createdSizes) {
				t.Errorf("created bundles carry %v findings, want %v:\n%s", sizes, tc.createdSizes, log)
			}
			comments, err := f.bd.Comments("gt-abc")
			if err != nil || len(comments) != 1 || !strings.Contains(comments[0].Text, "appended to the open follow-up for this landing: gt-bundle") {
				t.Errorf("comments = %v (%v), want one naming the appended bundle", commentTexts(comments), err)
			}
		})
	}
}

// TestFeedQueuesWhenTheBundleWriteFails: a write that fails drops nothing, and the
// queue holds WHICH findings the failed write was to file: a create that fails
// queues all five; an overflow whose create fails queues the three of the rest and
// not the two the append took; and a second chunk that fails leaves the first
// bundle filed and queues the findings of the second and third chunks.
func TestFeedQueuesWhenTheBundleWriteFails(t *testing.T) {
	t.Parallel()
	titles := func(from, to int) []string {
		var out []string
		for i := from; i <= to; i++ {
			out = append(out, fmt.Sprintf("finding %d", i))
		}
		return out
	}
	for _, tc := range []struct {
		name        string
		n           int
		existing    []ExistingFollowup
		failFrom    int
		wantCreated int
		wantQueued  []string
	}{
		{"a create that fails", 5, nil, 0, 0, titles(1, 5)},
		{"an overflow whose create fails", 5, []ExistingFollowup{bundleWithRoom(2)}, 0, 0, titles(3, 5)},
		{"a second chunk that fails", 13, nil, 2, 1, titles(7, 13)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, numberedFindings(tc.n))
			f.fake.existing = tc.existing
			f.fake.createErr, f.fake.failFrom = errors.New("bd down"), tc.failFrom
			log := f.run()
			if !strings.Contains(log, "could not file the follow-up bead") || len(f.fake.created) != tc.wantCreated {
				t.Errorf("created %d beads, want %d, and the log naming the failed create:\n%s", len(f.fake.created), tc.wantCreated, log)
			}
			queued, err := f.reviews.Queued()
			if err != nil || len(queued) != 1 {
				t.Fatalf("queued = %+v (%v), want one line", queued, err)
			}
			var got []string
			for _, q := range queued[0].Findings {
				got = append(got, q.Title)
			}
			if !slices.Equal(got, tc.wantQueued) {
				t.Errorf("queued findings %v, want %v", got, tc.wantQueued)
			}
		})
	}
}
