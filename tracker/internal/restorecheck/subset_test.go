package restorecheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// fakeReader serves the three reads of ReadOnlySubset, or fails the one the
// case names. Each read counts its calls so a case can show what ran.
type fakeReader struct {
	stats    api.Stats
	statsErr error
	list     api.IssuePage
	listErr  error
	byID     map[string]api.Issue
	getErr   error

	statsCalls, listCalls, getCalls int
}

func (f *fakeReader) Stats(context.Context, string) (api.Stats, error) {
	f.statsCalls++
	return f.stats, f.statsErr
}

func (f *fakeReader) ListIssues(context.Context, api.ListFilter) (api.IssuePage, error) {
	f.listCalls++
	return f.list, f.listErr
}

func (f *fakeReader) GetIssues(context.Context, []string) (map[string]api.Issue, error) {
	f.getCalls++
	return f.byID, f.getErr
}

// aReader is a copy that reads its three answers back consistently.
func aReader() *fakeReader {
	issue := api.Issue{ID: "gt-abc", Title: "restore me", Status: "open"}
	return &fakeReader{
		stats: api.Stats{Total: 1, ByStatus: map[string]int{"open": 1}},
		list:  api.IssuePage{Issues: []api.Issue{issue}},
		byID:  map[string]api.Issue{"gt-abc": issue},
	}
}

// The three reads agree, so the subset passes (gt-p85pw.72).
func TestReadOnlySubsetPassesOnAConsistentCopy(t *testing.T) {
	t.Parallel()
	if err := ReadOnlySubset(context.Background(), aReader()); err != nil {
		t.Fatalf("ReadOnlySubset = %v, want nil", err)
	}
}

// Every way the subset can disagree with itself or the copy fails the check,
// and each case says which read refused, so an operator knows what broke
// (gt-p85pw.72).
func TestReadOnlySubsetRefuses(t *testing.T) {
	t.Parallel()
	errStats := errors.New("count failed")
	errList := errors.New("list failed")
	errGet := errors.New("get failed")
	cases := []struct {
		name string
		edit func(*fakeReader)
		want string
	}{
		{
			name: "the count fails",
			edit: func(r *fakeReader) { r.statsErr = errStats },
			want: "count the issues",
		},
		{
			name: "the copy holds no issue",
			edit: func(r *fakeReader) { r.stats = api.Stats{} },
			want: "holds no issue",
		},
		{
			name: "the list fails",
			edit: func(r *fakeReader) { r.listErr = errList },
			want: "list the issues",
		},
		{
			name: "the list is empty",
			edit: func(r *fakeReader) { r.list = api.IssuePage{} },
			want: "lists no issue",
		},
		{
			name: "the by-id read fails",
			edit: func(r *fakeReader) { r.getErr = errGet },
			want: "by id",
		},
		{
			name: "the listed issue does not read back by id",
			edit: func(r *fakeReader) { r.byID = map[string]api.Issue{} },
			want: "does not read back by id",
		},
		{
			name: "the title differs",
			edit: func(r *fakeReader) {
				other := r.byID["gt-abc"]
				other.Title = "something else"
				r.byID["gt-abc"] = other
			},
			want: "different title or status",
		},
		{
			name: "the status differs",
			edit: func(r *fakeReader) {
				other := r.byID["gt-abc"]
				other.Status = "closed"
				r.byID["gt-abc"] = other
			},
			want: "different title or status",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := aReader()
			tc.edit(r)
			err := ReadOnlySubset(context.Background(), r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadOnlySubset = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// The by-id read runs only once the count and the list have answered: a copy
// that holds no issue is reported without a further read (gt-p85pw.72).
func TestReadOnlySubsetStopsAtTheFirstRefusal(t *testing.T) {
	t.Parallel()
	r := aReader()
	r.stats = api.Stats{}
	if err := ReadOnlySubset(context.Background(), r); err == nil {
		t.Fatal("ReadOnlySubset = nil, want a refusal")
	}
	if r.listCalls != 0 || r.getCalls != 0 {
		t.Errorf("list and by-id calls = %d, %d, want none after the count refused", r.listCalls, r.getCalls)
	}
}
