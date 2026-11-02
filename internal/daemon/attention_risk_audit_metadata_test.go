package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// TestAttentionRiskAuditReadsABeadWhoseLinkMetadataIsAnObject pins the audit's
// batch read of a bead whose dependency metadata bd prints as a JSON object
// (gt-euvnd): the read must parse it, and the pass must record the AUDITED line
// with no error.
func TestAttentionRiskAuditReadsABeadWhoseLinkMetadataIsAnObject(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 17, 25, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newRiskAuditWorld(f)
	head := strings.Repeat("a", 40)
	w.land("gt-abc", head, failNote(head, "gt-fa"))
	w.status["gt-fa"] = "closed"

	payload, err := json.Marshal([]map[string]any{{
		"id":       "gt-abc",
		"priority": 2,
		"notes":    w.notes["gt-abc"],
		"dependencies": []map[string]any{
			{"id": "gt-9", "metadata": map[string]string{"holder": "ace"}},
		},
	}})
	if err != nil {
		t.Fatalf("marshal bd show answer: %v", err)
	}
	f.src.d.openWorkBeads = func([]string, time.Duration) workBeadReader {
		return beads.NewPlain(t.TempDir(), nil, beads.WithRunner(
			func(context.Context, []string) ([]byte, []byte, error) { return payload, nil, nil },
		))
	}
	f.src.riskIssues = f.src.d.attentionRiskIssues

	wrote, err := f.src.auditResolvedRiskFails(context.Background())
	if err != nil {
		t.Fatalf("auditResolvedRiskFails = %v, want no parse error", err)
	}
	want := land.OverseerReviewMarker + " " + head + " AUDITED: findings resolved by gt-fa"
	if len(wrote) != 1 || len(w.appends) != 1 || w.appends[0] != "gt-abc: "+want {
		t.Fatalf("wrote = %q, appends = %q, want the one line %q on gt-abc", wrote, w.appends, want)
	}
}
