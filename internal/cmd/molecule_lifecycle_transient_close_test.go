package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// rereadDiesClient is a beads.Client whose first Close reports a partial
// close and whose second — the pass that finishes the molecule — closes for
// real but returns the transient failure of the re-read that checks it. That
// is the shape bd produces when the connection dies between the close and
// the confirming read (gt-22hdp.36).
type rereadDiesClient struct {
	beads.Client
	closes int
}

func (c *rereadDiesClient) Close(ids ...string) error {
	c.closes++
	switch c.closes {
	case 1:
		// bd skipped the last id; the rest closed.
		if err := c.Client.Close(ids[:len(ids)-1]...); err != nil {
			return err
		}
		return &beads.PartialCloseError{Closed: ids[:len(ids)-1], NotClosed: ids[len(ids)-1:]}
	case 2:
		// The batch landed, and the read that confirms it failed.
		if err := c.Client.Close(ids...); err != nil {
			return err
		}
		return errors.New("re-reading 1 closed issue(s): bd show: dial tcp: connection refused")
	}
	return nil
}

// TestCloseStepsThenRootClosesTheRootOverATransientRereadFailure: a later
// pass that ends on a failed re-read is not a refusal, so it must not borrow
// that label. closeStepsThenRoot reads the label as "leave the root open",
// and the steps are all closed by then — leaving the root open over no open
// step strands the molecule as visible work nobody owns (gt-22hdp.36).
func TestCloseStepsThenRootClosesTheRootOverATransientRereadFailure(t *testing.T) {
	t.Parallel()
	fake := beadsfake.New()
	root, err := fake.Create(beads.CreateOptions{Title: "mol-polecat-work"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	first, err := fake.Create(beads.CreateOptions{Title: "load-context", Parent: root.ID})
	if err != nil {
		t.Fatalf("create first step: %v", err)
	}
	second, err := fake.Create(beads.CreateOptions{Title: "branch-setup", Parent: root.ID})
	if err != nil {
		t.Fatalf("create second step: %v", err)
	}
	client := &rereadDiesClient{Client: fake}

	rootClosed := false
	_, err = closeStepsThenRoot(client, root.ID, func() error {
		rootClosed = true
		return nil
	})
	if err != nil {
		t.Fatalf("closeStepsThenRoot = %v, want the molecule closed", err)
	}
	if !rootClosed {
		t.Error("closeStepsThenRoot left the root open; every step had closed")
	}
	for _, id := range []string{first.ID, second.ID} {
		got, err := fake.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != string(beads.StatusClosed) {
			t.Errorf("step %s status %q, want closed", id, got.Status)
		}
	}
}
