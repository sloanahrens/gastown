package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
)

// customStatusStore records the status of each list a pool fetch makes and
// each status.custom read. configErr, when set, fails the read.
type customStatusStore struct {
	beads.Client
	configErr  error
	configGets int
	lists      []string
}

func (s *customStatusStore) ConfigGet(key string) (string, error) {
	s.configGets++
	if s.configErr != nil {
		return "", s.configErr
	}
	return s.Client.(customStatusReader).ConfigGet(key)
}

func (s *customStatusStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	s.lists = append(s.lists, opts.Status)
	return s.Client.List(opts)
}

// customStatusLists is the lists of a fetch that are not the pool's own: the
// live statuses and closed. An empty status, which bd reads as no filter at
// all, is one of them.
func customStatusLists(lists []string) []string {
	live := strings.Join(duplicatePoolStatuses, ",")
	var out []string
	for _, status := range lists {
		if status != live && status != "closed" {
			out = append(out, status)
		}
	}
	return out
}

// registerCustomStatuses registers the constants' custom statuses in db, as a
// rig that has run `gt doctor fix beads-custom-statuses` does.
func registerCustomStatuses(t *testing.T, db *beadsfake.Fake) {
	t.Helper()
	if err := db.ConfigSet("status.custom", constants.BeadsCustomStatuses); err != nil {
		t.Fatalf("ConfigSet status.custom: %v", err)
	}
}

// A store that registers no custom status is not asked for one: bd rejects an
// unregistered status on every pool fetch, so the fetch lists none and warns
// nothing (gt-f6gv4k).
func TestDuplicateCustomStatusUnsetListsNothing(t *testing.T) {
	t.Parallel()
	db := duplicateGuardFixture(t)
	if err := db.ConfigSet("status.custom", ""); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	store := &customStatusStore{Client: listLikeBD{Client: db}}
	var warnings []string
	warn := func(_ string, err error) { warnings = append(warnings, err.Error()) }
	if _, err := fetchDuplicatePoolWith(store, warn, t.TempDir(), nil); err != nil {
		t.Fatalf("fetchDuplicatePoolWith: %v", err)
	}
	if got := customStatusLists(store.lists); len(got) != 0 {
		t.Errorf("listed custom statuses %q with status.custom unset, want none", got)
	}
	if len(warnings) != 0 {
		t.Errorf("warned %q with status.custom unset, want no warning", warnings)
	}
}

// The custom list names only the statuses the store registered, in the order
// the constants list them (gt-f6gv4k).
func TestDuplicateCustomStatusListsOnlyTheRegisteredStatuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		registered string
		want       []string
	}{
		{"both registered", "staged_ready,staged_warnings", []string{"staged_ready,staged_warnings"}},
		{"only staged_ready registered", "staged_ready", []string{"staged_ready"}},
		{"an unknown status registered alongside", "staged_ready,other_status", []string{"staged_ready"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := duplicateGuardFixture(t)
			if err := db.ConfigSet("status.custom", tc.registered); err != nil {
				t.Fatalf("ConfigSet: %v", err)
			}
			store := &customStatusStore{Client: listLikeBD{Client: db}}
			if _, err := fetchDuplicatePoolWith(store, func(string, error) {}, t.TempDir(), nil); err != nil {
				t.Fatalf("fetchDuplicatePoolWith: %v", err)
			}
			if got := customStatusLists(store.lists); !slices.Equal(got, tc.want) {
				t.Errorf("custom lists = %q, want %q", got, tc.want)
			}
		})
	}
}

// A failed status.custom read warns once, skips the custom list, and is not
// read again inside duplicatePoolTTL: a second fetch reports the same skip
// from the tick's cache (gt-f6gv4k).
func TestDuplicateCustomStatusConfigErrorWarnsAndReadsOncePerTTL(t *testing.T) {
	t.Parallel()
	store := &customStatusStore{
		Client:    listLikeBD{Client: duplicateGuardFixture(t)},
		configErr: errors.New("bd config get: boom"),
	}
	cache := loadDuplicateDetailCache(filepath.Join(t.TempDir(), "cache.json"), "daemon-1", 40)
	beadsDir := t.TempDir()
	var warnings []string
	warn := func(_ string, err error) { warnings = append(warnings, err.Error()) }

	if _, err := fetchDuplicatePoolWith(store, warn, beadsDir, cache); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "status.custom") {
		t.Fatalf("first fetch warnings = %q, want one naming status.custom", warnings)
	}
	if _, err := fetchDuplicatePoolWith(store, warn, beadsDir, cache); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if store.configGets != 1 {
		t.Errorf("status.custom read %d times across two fetches inside the TTL, want 1", store.configGets)
	}
	if got := customStatusLists(store.lists); len(got) != 0 {
		t.Errorf("listed custom statuses %q after a failed config read, want none", got)
	}
}

// A store that cannot report its registered statuses is an error, not a store
// with none registered: the custom list is skipped and the skip is reported
// (gt-f6gv4k).
func TestDuplicateCustomStatusStoreWithoutConfigWarnsAndListsNothing(t *testing.T) {
	t.Parallel()
	store := &unconfiguredStore{Client: listLikeBD{Client: duplicateGuardFixture(t)}}
	var warnings []string
	warn := func(_ string, err error) { warnings = append(warnings, err.Error()) }
	if _, err := fetchDuplicatePoolWith(store, warn, t.TempDir(), nil); err != nil {
		t.Fatalf("fetchDuplicatePoolWith: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "cannot read status.custom") {
		t.Errorf("warnings = %q, want one saying the store cannot read status.custom", warnings)
	}
	if got := customStatusLists(store.lists); len(got) != 0 {
		t.Errorf("listed custom statuses %q from a store that cannot read them, want none", got)
	}
}

// unconfiguredStore lists beads but has no status.custom read, as a store that
// is not bd or the tracker has none. It has no ConfigGet method at all.
type unconfiguredStore struct {
	beads.Client
	lists []string
}

func (s *unconfiguredStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	s.lists = append(s.lists, opts.Status)
	return s.Client.List(opts)
}

// TestDuplicateCustomStatusReadOutsideLock: a fetch whose read is a bd subprocess
// does not hold the cache's lock for it, so a fetch of another directory on the
// same cache is answered while the first read is still blocked (gt-18sp1c).
func TestDuplicateCustomStatusReadOutsideLock(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	c := &duplicateDetailCache{}
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				first <- fmt.Errorf("the first fetch panicked: %v", r)
			}
		}()
		got, err := c.customStatuses("/a", func() ([]string, error) {
			close(entered)
			<-release
			return []string{"a"}, nil
		})
		if err == nil && !slices.Equal(got, []string{"a"}) {
			err = fmt.Errorf("the first fetch returned %v, want [a]", got)
		}
		first <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the first read never started")
	}
	second := make(chan []string, 1)
	go func() {
		got, _ := c.customStatuses("/b", func() ([]string, error) { return []string{"b"}, nil })
		second <- got
	}()
	select {
	case got := <-second:
		if !slices.Equal(got, []string{"b"}) {
			t.Errorf("the second fetch returned %v, want [b]", got)
		}
	case <-ctx.Done():
		t.Error("a second fetch was blocked while the first read ran: the cache lock is held over the read")
	}
	close(release)
	done, stopDone := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopDone()
	select {
	case err := <-first:
		if err != nil {
			t.Error(err)
		}
	case <-done.Done():
		t.Fatal("the first fetch never returned after its read was released")
	}
}

// TestDuplicateCustomStatusConcurrentMiss: two fetches of one directory that both
// miss overlap in their reads (each read waits until both have entered), each
// caller gets its own read's answer, the held entry is one of the two, and a
// third fetch inside duplicatePoolTTL is served by it and reads nothing more. Run
// under -race, the two stores are concurrent, so a store outside the lock fails
// (gt-18sp1c).
func TestDuplicateCustomStatusConcurrentMiss(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	c := &duplicateDetailCache{}
	var reads atomic.Int32
	entered, release := make(chan struct{}, 2), make(chan struct{})
	answers := [][]string{{"a"}, {"b"}}
	got := make(chan []string, 2)
	for i := range answers {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					got <- []string{fmt.Sprintf("panic: %v", r)}
				}
			}()
			statuses, _ := c.customStatuses("/rig", func() ([]string, error) {
				reads.Add(1)
				entered <- struct{}{}
				<-release
				return answers[i], nil
			})
			got <- statuses
		}()
	}
	for range answers {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("the two reads never overlapped: a fetch was held behind the other's read")
		}
	}
	close(release)
	var returned [][]string
	for range answers {
		select {
		case s := <-got:
			returned = append(returned, s)
		case <-ctx.Done():
			t.Fatal("a fetch never returned after its read was released")
		}
	}
	if !slices.ContainsFunc(returned, func(s []string) bool { return slices.Equal(s, []string{"a"}) }) ||
		!slices.ContainsFunc(returned, func(s []string) bool { return slices.Equal(s, []string{"b"}) }) {
		t.Errorf("the callers returned %v, want each its own read's answer, [a] and [b]", returned)
	}
	third, err := c.customStatuses("/rig", func() ([]string, error) { reads.Add(1); return []string{"third"}, nil })
	if err != nil || !(slices.Equal(third, []string{"a"}) || slices.Equal(third, []string{"b"})) || reads.Load() != 2 {
		t.Errorf("the third fetch = %v, %v after %d reads, want one of the two held answers and no third read", third, err, reads.Load())
	}
}

// TestDuplicateCustomStatusExpiryAndRecovery: an entry older than
// duplicatePoolTTL is read again, a failed entry inside the TTL is still served,
// and a failed entry past the TTL is replaced by a recovered read. The entries
// are seeded stale because customStatuses stamps with time.Now(): there is no
// clock to inject and nothing sleeps (gt-18sp1c).
func TestDuplicateCustomStatusExpiryAndRecovery(t *testing.T) {
	t.Parallel()
	down := errors.New("bd down")
	for _, tc := range []struct {
		name      string
		age       time.Duration
		seeded    customStatusEntry
		wantReads int32
		want      []string
		wantErr   error
	}{
		{"fresh entry is served", 0, customStatusEntry{statuses: []string{"old"}}, 0, []string{"old"}, nil},
		{"stale entry is read again", 2 * duplicatePoolTTL, customStatusEntry{statuses: []string{"old"}}, 1, []string{"new"}, nil},
		{"failed entry inside the TTL is still served", 0, customStatusEntry{err: down}, 0, nil, down},
		{"failed entry past the TTL is replaced by the recovered read", 2 * duplicatePoolTTL, customStatusEntry{err: down}, 1, []string{"new"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reads atomic.Int32
			tc.seeded.fetchedAt = time.Now().Add(-tc.age)
			c := &duplicateDetailCache{registered: map[string]customStatusEntry{"/rig": tc.seeded}}
			got, err := c.customStatuses("/rig", func() ([]string, error) { reads.Add(1); return []string{"new"}, nil })
			if !slices.Equal(got, tc.want) || !errors.Is(err, tc.wantErr) || reads.Load() != tc.wantReads {
				t.Errorf("= %v, %v after %d reads, want %v, %v after %d", got, err, reads.Load(), tc.want, tc.wantErr, tc.wantReads)
			}
		})
	}
}
