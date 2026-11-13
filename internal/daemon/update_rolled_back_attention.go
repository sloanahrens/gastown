package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/update"
)

// collectUpdateRolledBack raises one high item when the update's last run put
// the daemon back on its previous image after a check failed. The item is the
// run's own record: the check that failed and the pair restored, so the summary
// says what the run recorded and nothing the daemon inferred. The next run
// replaces the state file, which clears the item.
//
// No run having written state raises nothing. A state file that cannot be read
// fails the collection, so the previous tick's item of this kind is kept rather
// than a read failure passing as no rollback (gt-9odoqa).
func (s *attentionSources) collectUpdateRolledBack(_ context.Context) ([]attention.Item, error) {
	store := &update.Store{Dir: update.Dir(s.d.config.TownRoot)}
	st, err := store.Read()
	switch {
	case errors.Is(err, update.ErrNoState):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if st.Rollback == nil {
		return nil, nil
	}
	return []attention.Item{updateRolledBackItem(st)}, nil
}

// updateRolledBackItem is the rolled-back item of the run st ended with. It is
// keyed by the target sha, so one roll raises one item however many ticks it
// stands.
func updateRolledBackItem(st update.State) attention.Item {
	rb := st.Rollback
	sha := greenRefShortSHA(st.Target.SHA)
	return attention.Item{
		Key:      "update-rolled-back:" + sha,
		Kind:     attention.KindUpdateRolledBack,
		Severity: attention.SeverityHigh,
		Summary: fmt.Sprintf("gt update %s rolled back: the %s check failed; restored the daemon to %s on commit %s",
			sha, rb.Check, rb.Restored.Daemon, greenRefShortSHA(rb.Restored.Commit)),
	}
}

// updateRolledBackOwned picks the rolled-back items out of the kind's previous
// items, so a failed collector keeps only its own (collectAttention).
func updateRolledBackOwned(it attention.Item) bool {
	return it.Kind == attention.KindUpdateRolledBack && strings.HasPrefix(it.Key, "update-rolled-back:")
}
