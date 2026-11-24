package update

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// applyPruneEntry is a delete candidate for tag, in the daemon image.
func applyPruneEntry(tag string) PruneEntry {
	return PruneEntry{Image: "gastown-daemon", Tag: tag, Digest: "sha256:" + tag}
}

func TestApplyPrune(t *testing.T) {
	t.Parallel()
	errRecheck := errors.New("ledger unreadable")
	errDel := errors.New("registry refused the delete")
	a, b, c, k := applyPruneEntry("aaa"), applyPruneEntry("bbb"), applyPruneEntry("ccc"), applyPruneEntry("kkk")
	all := PrunePlan{Delete: []PruneEntry{a, b, c}}

	tests := []struct {
		name        string
		plan        PrunePlan
		max         int
		recheckErrs map[string]error
		delErrs     map[string]error
		calls       string // the recheck and del calls, in order
		deleted     string // tags of Deleted
		skipped     string // tags of Skipped
		failed      bool
		wantErr     error
	}{
		{name: "deletes every candidate in plan order", plan: all, max: 10, calls: "recheck:aaa del:aaa recheck:bbb del:bbb recheck:ccc del:ccc", deleted: "aaa bbb ccc"},
		{name: "max caps the deletes before the next recheck", plan: all, max: 2, calls: "recheck:aaa del:aaa recheck:bbb del:bbb", deleted: "aaa bbb"},
		{name: "a Keep entry never reaches del", plan: PrunePlan{Keep: []PruneEntry{k}, Delete: []PruneEntry{a}}, max: 5, calls: "recheck:aaa del:aaa", deleted: "aaa"},
		{name: "a failing recheck skips without spending a delete", plan: all, max: 2, recheckErrs: map[string]error{"aaa": errRecheck}, calls: "recheck:aaa recheck:bbb del:bbb recheck:ccc del:ccc", deleted: "bbb ccc", skipped: "aaa", failed: true},
		{name: "a delete error stops the run with no retry", plan: all, max: 10, delErrs: map[string]error{"bbb": errDel}, calls: "recheck:aaa del:aaa recheck:bbb del:bbb", deleted: "aaa", failed: true, wantErr: errDel},
		{name: "a delete error on the first entry deletes nothing", plan: PrunePlan{Delete: []PruneEntry{a, b}}, max: 10, delErrs: map[string]error{"aaa": errDel}, calls: "recheck:aaa del:aaa", failed: true, wantErr: errDel},
		{name: "a skipped recheck and a later delete error", plan: all, max: 10, recheckErrs: map[string]error{"aaa": errRecheck}, delErrs: map[string]error{"bbb": errDel}, calls: "recheck:aaa recheck:bbb del:bbb", skipped: "aaa", failed: true, wantErr: errDel},
		{name: "zero max makes no call and is not a failure", plan: all, max: 0},
		{name: "an empty plan makes no call", plan: PrunePlan{}, max: 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			recheck := func(e PruneEntry) error {
				calls = append(calls, "recheck:"+e.Tag)
				return tc.recheckErrs[e.Tag]
			}
			del := func(_ context.Context, e PruneEntry) error {
				calls = append(calls, "del:"+e.Tag)
				return tc.delErrs[e.Tag]
			}

			res, err := ApplyPrune(t.Context(), tc.plan, tc.max, recheck, del)

			if got := strings.Join(calls, " "); got != tc.calls {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
			var deleted, skipped []string
			for _, d := range res.Deleted {
				deleted = append(deleted, d.Tag)
			}
			for _, s := range res.Skipped {
				skipped = append(skipped, s.Entry.Tag)
				if !errors.Is(s.Err, tc.recheckErrs[s.Entry.Tag]) {
					t.Errorf("skip of %s carries %v, want %v", s.Entry.Tag, s.Err, tc.recheckErrs[s.Entry.Tag])
				}
			}
			if got := strings.Join(deleted, " "); got != tc.deleted {
				t.Errorf("deleted = %q, want %q", got, tc.deleted)
			}
			if got := strings.Join(skipped, " "); got != tc.skipped {
				t.Errorf("skipped = %q, want %q", got, tc.skipped)
			}
			if res.Failed != tc.failed {
				t.Errorf("Failed = %v, want %v", res.Failed, tc.failed)
			}
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestApplyPruneRefuses: a negative max or a nil callback is refused before
// any call, so a bad argument never reaches the registry.
func TestApplyPruneRefuses(t *testing.T) {
	t.Parallel()
	plan := PrunePlan{Delete: []PruneEntry{applyPruneEntry("aaa")}}
	var calls int
	recheck := func(PruneEntry) error { calls++; return nil }
	del := func(context.Context, PruneEntry) error { calls++; return nil }
	tests := []struct {
		name    string
		max     int
		recheck func(PruneEntry) error
		del     func(context.Context, PruneEntry) error
	}{
		{name: "negative max", max: -1, recheck: recheck, del: del},
		{name: "nil recheck", max: 10, del: del},
		{name: "nil del", max: 10, recheck: recheck},
	}
	for _, tc := range tests {
		calls = 0
		res, err := ApplyPrune(t.Context(), plan, tc.max, tc.recheck, tc.del)
		if !errors.Is(err, ErrPruneApplyRefused) || calls != 0 || res.Failed || len(res.Deleted) != 0 {
			t.Errorf("%s: err = %v, calls = %d, result = %+v; want ErrPruneApplyRefused, no calls, nothing deleted", tc.name, err, calls, res)
		}
	}
}
