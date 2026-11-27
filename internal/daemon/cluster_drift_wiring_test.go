package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
)

// TestClusterDriftCompareFailureIsUnreadable: the collector, fed the real
// compare over a start that answers an error, raises exactly the unreadable
// item, never a clear. Mutation: a compare that swallows the start's error
// answers an empty result with no error, and the collector raises nothing
// (gt-ji7lad).
func TestClusterDriftCompareFailureIsUnreadable(t *testing.T) {
	t.Parallel()
	startErr := errors.New("fork/exec /opt/gt/bin/gt: no such file or directory")
	started := false
	start := func(context.Context, string, []string, int) ([]byte, []byte, int, error) {
		started = true
		return nil, nil, -1, startErr
	}
	exe := func() (string, error) { return "/opt/gt/bin/gt", nil }
	s := &attentionSources{
		clusterDriftEnabled: func() bool { return true },
		clusterDriftAnswer:  newClusterDriftCompare(exe, start),
	}

	items := collectClusterDriftOnce(t, s)

	if !started {
		t.Fatal("the compare never ran the start, so the answer did not come from a failed run")
	}
	assertClusterDriftItem(t, items, attention.RuleClusterDriftUnreadable, attention.KindClusterDriftUnreadable)
	if !strings.Contains(items[0].Summary, startErr.Error()) {
		t.Errorf("summary = %q, want it to name the start's error %q", items[0].Summary, startErr)
	}
}
