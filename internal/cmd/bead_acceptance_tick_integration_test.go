//go:build integration

package cmd

import (
	"bytes"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// amendingStore makes an owner's write land between the tick's read and its
// first guarded write. The owner's write goes through the real bd client, so
// the guard that follows is refused by bd itself, not by a fake.
type amendingStore struct {
	beads.Client
	amended   string
	amendDone bool
	guarded   int
}

func (s *amendingStore) Update(id string, opts beads.UpdateOptions) error {
	if opts.IfAcceptance != "" {
		s.guarded++
		if !s.amendDone {
			s.amendDone = true
			if err := s.Client.Update(id, beads.UpdateOptions{Acceptance: &s.amended}); err != nil {
				return err
			}
		}
	}
	return s.Client.Update(id, opts)
}

// TestIntegrationAcceptanceTickRetriesAGuardMissOnRealBd: real bd refuses the
// guarded write when the owner amended the field after the tick read it. The
// tick re-reads, ticks the box by its text in the amended block, and writes
// once more, so the owner's boxes survive.
func TestIntegrationAcceptanceTickRetriesAGuardMissOnRealBd(t *testing.T) {
	t.Parallel()
	requireBd(t)
	_, b := setupPatrolTestDB(t)
	original := "- [ ] first\n- [ ] second"
	is, err := b.Create(beads.CreateOptions{Title: "tick guard", Acceptance: original, Priority: -1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	amended := "- [ ] added before\n- [ ] first\n- [ ] second\n- [ ] added after"
	store := &amendingStore{Client: b, amended: amended}
	var out bytes.Buffer
	v := &beadVerbs{client: store, actor: "me", out: &out}

	if err := v.tick(is.ID, "second"); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if store.guarded != 2 {
		t.Errorf("guarded writes = %d, want 2: the first is refused by bd, the retry lands", store.guarded)
	}
	want := "- [ ] added before\n- [ ] first\n- [x] second\n- [ ] added after"
	got, err := b.Show(is.ID)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if got.AcceptanceCriteria != want {
		t.Errorf("after the retried tick, criteria = %q, want %q", got.AcceptanceCriteria, want)
	}
}
