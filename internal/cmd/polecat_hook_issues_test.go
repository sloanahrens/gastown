package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// hookCountingBeads is a bead client that counts its reads: one Show per id and
// one ShowMultiple per call. The list path must read a rig's hooks in one
// ShowMultiple, however many polecats the rig has.
type hookCountingBeads struct {
	issues       map[string]*beads.Issue
	shows        int
	showMultiple int
	multiErr     error
	omit         map[string]bool // ids the batch silently skips
}

func (c *hookCountingBeads) Show(id string) (*beads.Issue, error) {
	c.shows++
	if issue, ok := c.issues[id]; ok {
		return issue, nil
	}
	return nil, beads.ErrNotFound
}

func (c *hookCountingBeads) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	c.showMultiple++
	if c.multiErr != nil {
		return nil, c.multiErr
	}
	out := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		if issue, ok := c.issues[id]; ok && !c.omit[id] {
			out[id] = issue
		}
	}
	return out, nil
}

// hookFixture builds n polecats, each holding its own active hook bead, plus
// one hook that names a bead the rig does not have.
func hookFixture(n int) (*hookCountingBeads, []string) {
	src := &hookCountingBeads{issues: map[string]*beads.Issue{}}
	var hooks []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("gt-hook%d", i)
		src.issues[id] = &beads.Issue{ID: id, Status: "hooked"}
		hooks = append(hooks, id)
	}
	hooks = append(hooks, "gt-gone")
	return src, hooks
}

// TestHookIssuesReadsARigInOneBatch: the reads a rig's seats make do not grow
// with its polecat count. Three polecats and thirty read the same way, one
// ShowMultiple and no Show, however many seats ask for their hook or none.
func TestHookIssuesReadsARigInOneBatch(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 30} {
		n := n
		t.Run(fmt.Sprintf("%d polecats", n), func(t *testing.T) {
			t.Parallel()
			src, hooks := hookFixture(n)
			hookIDs := append([]string{}, hooks...)
			hookIDs = append(hookIDs, "", hooks[0]) // an idle seat and a duplicate hook
			reader := newPolecatHookIssues(src, hookIDs)
			for _, id := range hookIDs {
				hookBeadDispositionForInventory(reader, id)
			}
			// The one id the rig does not have is the only Show: a missing id
			// costs one read, however many polecats the rig has.
			if src.showMultiple != 1 || src.shows != 1 {
				t.Fatalf("reads = %d ShowMultiple, %d Show; want 1 and 1", src.showMultiple, src.shows)
			}
		})
	}
}

// TestHookIssuesClassifyAsShowWould: a hook the batch returned classifies as
// its issue does, a hook the batch was asked for and did not return is not
// found, and an empty hook is safe without a read.
func TestHookIssuesClassifyAsShowWould(t *testing.T) {
	t.Parallel()
	src, _ := hookFixture(1)
	reader := newPolecatHookIssues(src, []string{"gt-hook0", "gt-gone", ""})

	got := hookBeadDispositionForInventory(reader, "gt-hook0")
	if got.Blocker == "" || got.Safe {
		t.Errorf("hooked bead = %+v, want a blocker", got)
	}
	gone := hookBeadDispositionForInventory(reader, "gt-gone")
	if want := polecat.ClassifyHookBead("gt-gone", nil, beads.ErrNotFound); gone != want {
		t.Errorf("missing bead = %+v, want %+v", gone, want)
	}
	if empty := hookBeadDispositionForInventory(reader, ""); !empty.Safe {
		t.Errorf("empty hook = %+v, want safe", empty)
	}
	if src.shows != 1 {
		t.Errorf("Show calls = %d, want 1 (only the missing id)", src.shows)
	}
}

// TestActiveMRIsReadInTheBatch: an active merge request that the rig's index
// does not hold (another rig's MR) is read in the batch, not with a Show of
// its own. This is the per-polecat read the list used to pay.
func TestActiveMRIsReadInTheBatch(t *testing.T) {
	t.Parallel()
	src := &hookCountingBeads{issues: map[string]*beads.Issue{"be-mr1": {ID: "be-mr1", Status: "open"}}}
	reader := newPolecatHookIssues(src, []string{"", "be-mr1", "be-mr1", "be-absent"})
	issue, err := reader.Show("be-mr1")
	if err != nil || issue == nil || issue.ID != "be-mr1" {
		t.Fatalf("Show(be-mr1) = %v, %v; want the batched issue", issue, err)
	}
	if _, err := reader.Show("be-absent"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Show(be-absent) err = %v; want not found from the source", err)
	}
	if src.shows != 1 {
		t.Errorf("Show calls = %d, want 1 (only be-absent)", src.shows)
	}
}

// TestHookIssuesFallBackWhenTheBatchFails: a failed batch read is not an
// empty answer. Every hook goes to the source one id at a time, as before.
func TestHookIssuesFallBackWhenTheBatchFails(t *testing.T) {
	t.Parallel()
	src, hooks := hookFixture(3)
	src.multiErr = errors.New("dolt unreachable")
	reader := newPolecatHookIssues(src, hooks)
	for _, id := range hooks {
		hookBeadDispositionForInventory(reader, id)
	}
	if src.shows != len(hooks) {
		t.Fatalf("Show calls = %d, want %d (one per hook after a failed batch)", src.shows, len(hooks))
	}
}

// TestEmptyHookSkipsTheRead: an idle polecat's empty hook is classified
// without a Show of the empty id.
func TestEmptyHookSkipsTheRead(t *testing.T) {
	t.Parallel()
	src := &hookCountingBeads{issues: map[string]*beads.Issue{}}
	if got := hookBeadDispositionForInventory(src, ""); !got.Safe {
		t.Errorf("empty hook = %+v, want safe", got)
	}
	if src.shows != 0 {
		t.Errorf("Show calls = %d, want 0 for an empty hook", src.shows)
	}
}

// TestBatchOmissionIsNotNotFound: a batch that skips an id it should have
// answered for does not make that id not found. The source answers it, so a
// bead that exists is never reported as missing.
func TestBatchOmissionIsNotNotFound(t *testing.T) {
	t.Parallel()
	src := &hookCountingBeads{
		issues: map[string]*beads.Issue{"gt-skip": {ID: "gt-skip", Status: "hooked"}},
		omit:   map[string]bool{"gt-skip": true},
	}
	reader := newPolecatHookIssues(src, []string{"gt-skip"})
	issue, err := reader.Show("gt-skip")
	if err != nil || issue == nil || issue.ID != "gt-skip" {
		t.Fatalf("Show(gt-skip) = %v, %v; want the source's issue, not not-found", issue, err)
	}
	if src.shows != 1 {
		t.Errorf("Show calls = %d, want 1 for the id the batch skipped", src.shows)
	}
}
