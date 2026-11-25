package land

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// bundleWithFindings is an open bundle of internal/land/note.go for followupDay
// that carries n om-fp findings: its labels carry n fingerprints.
func bundleWithFindings(n int) ExistingFollowup {
	b := openBundleFor(followupDay, "internal/land/note.go", "open")
	for i := 1; i < n; i++ {
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

// TestFeedOverflowsIntoANewBundle: a group is appended to the open bundle only
// while that bundle has fewer than followupFindingsPerBead findings (gt-hp9ce8).
// Five findings with a bundle of three open, which is full, append nothing and
// file one bundle of five. Twelve with a bundle of one append five, the room the
// feed still counts in MaxAcceptance, and create 6 and 1, each at most
// MaxAcceptance findings (gt-ntkwh0). The landing's comment names the appended
// bead.
func TestFeedOverflowsIntoANewBundle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		n, findings  int
		appends      int
		appended     int
		createdSizes []int
	}{
		{"five findings with a full bundle open", 5, 3, 0, 0, []int{5}},
		{"twelve findings with a bundle of one open", 12, 1, 1, 5, []int{6, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, numberedFindings(tc.n))
			f.fake.existing = []ExistingFollowup{bundleWithFindings(tc.findings)}
			log := f.run()
			appended := 0
			for _, a := range f.fake.appended {
				appended += len(a.Add.Labels)
			}
			if len(f.fake.appended) != tc.appends || appended != tc.appended {
				t.Fatalf("appended %+v, want %d append(s) of %d findings:\n%s", f.fake.appended, tc.appends, tc.appended, log)
			}
			var sizes []int
			for _, spec := range f.fake.created {
				sizes = append(sizes, fingerprints(spec.Labels))
			}
			if !slices.Equal(sizes, tc.createdSizes) {
				t.Errorf("created bundles carry %v findings, want %v:\n%s", sizes, tc.createdSizes, log)
			}
			if tc.appends == 0 {
				return
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
// queues all five; with a full bundle open nothing is appended, so the create of
// all five fails and queues all five; and a second chunk that fails leaves the
// first bundle filed and queues the findings of the second and third chunks
// (gt-hp9ce8).
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
		{"a full bundle open and a create that fails", 5, []ExistingFollowup{bundleWithFindings(3)}, 0, 0, titles(1, 5)},
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
