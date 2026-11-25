package land

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

const (
	// feedRoomCap is the most om-fp findings an open bundle carries once the feed
	// has appended to it: the room an append may take is what this leaves after
	// the findings the bundle already holds (gt-ghrt3e). The overflow tests read
	// the room from it, so moving the room (gt-fpfw9y) changes it here and
	// nothing else.
	feedRoomCap = followupFindingsPerBead

	// feedChunkCap is the most findings the feed files in one new bundle: what
	// is left of a group is filed in bundles of at most this many findings
	// (gt-ghrt3e). The overflow tests read the chunk from it, so moving the chunk
	// (gt-z8ygdr) changes it here and nothing else.
	feedChunkCap = specdispatch.MaxAcceptance
)

// chunkSizes is how n findings are filed as new bundles: sizes of at most
// feedChunkCap findings, in filing order, the last carrying what is left
// (gt-ghrt3e).
func chunkSizes(n int) []int {
	var sizes []int
	for n > 0 {
		size := min(n, feedChunkCap)
		sizes = append(sizes, size)
		n -= size
	}
	return sizes
}

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
// are filed as one bundle. Twelve with a bundle of one append feedRoomCap-1
// findings, all the room a bundle of one has, and file the rest in bundles of
// at most feedChunkCap findings (gt-ntkwh0). The landing's comment names the
// appended bead.
func TestFeedOverflowsIntoANewBundle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		n, findings  int
		appends      int
		appended     int
		createdSizes []int
	}{
		{"five findings with a full bundle open", 5, 3, 0, 0, chunkSizes(5)},
		{"twelve findings with a bundle of one open", 12, 1, 1, feedRoomCap - 1, chunkSizes(12 - (feedRoomCap - 1))},
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

// TestFollowupFeedRoomStopsAtTheFindingsPerBeadCap: the room left in an open
// bundle is feedRoomCap minus the findings it carries, not the larger budget one
// worker's landing carries (gt-fpfw9y): a group of three with a bundle one
// finding short of full appends one finding and files the other two as a new
// bundle.
func TestFollowupFeedRoomStopsAtTheFindingsPerBeadCap(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, numberedFindings(3))
	f.fake.existing = []ExistingFollowup{bundleWithFindings(followupFindingsPerBead - 1)}
	log := f.run()
	appended := 0
	for _, a := range f.fake.appended {
		appended += len(a.Add.Labels)
	}
	if len(f.fake.appended) != 1 || appended != 1 {
		t.Fatalf("appended %+v, want one append of 1 finding:\n%s", f.fake.appended, log)
	}
	var sizes []int
	for _, spec := range f.fake.created {
		sizes = append(sizes, fingerprints(spec.Labels))
	}
	if want := []int{2}; !slices.Equal(sizes, want) {
		t.Errorf("created bundles carry %v findings, want %v:\n%s", sizes, want, log)
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
		{"a second chunk that fails", 2*feedChunkCap + 1, nil, 2, 1, titles(feedChunkCap+1, 2*feedChunkCap+1)},
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
