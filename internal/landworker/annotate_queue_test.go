package landworker

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// enqueueFails is a record queue whose writes are refused.
type enqueueFails struct{ *land.RecordQueue }

func (enqueueFails) Enqueue(land.PendingRecord) error { return errors.New("queue rewrite refused") }

// TestAnnotateQueue: a failed annotation is logged once with its bead and key
// and queued for the drain, and the key is set so a second pass neither posts
// nor queues it; a nil queue, or one that refuses, leaves the key unset
// (gt-o021qa).
func TestAnnotateQueue(t *testing.T) {
	t.Parallel()
	key, msg, what := "rework:"+noteHead, "the rework comment", "adding the rework comment"
	cases := []struct {
		name                string
		queue               string
		queued              int
		post, enqueueLogged bool
	}{
		{"a failing comment is queued and the key set", "real", 1, false, false},
		{"a nil queue leaves the key unset", "", 0, true, false},
		{"a refusing queue leaves the key unset", "refuse", 0, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal"})
			var logs []string
			h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			q := &land.RecordQueue{Path: filepath.Join(queueDir(t), "records.jsonl")}
			switch tc.queue {
			case "real":
				h.w.Records = q
			case "refuse":
				h.w.Records = enqueueFails{q}
			}
			h.w.Beads = failComments{Fake: h.bd, err: errors.New("bd down")}
			h.w.annotate("gt-abc", key, what, msg)
			recs, err := q.Pending()
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != tc.queued {
				t.Fatalf("queue holds %d record(s) %v; want %d", len(recs), recs, tc.queued)
			}
			if tc.queued == 1 && !(recs[0].Bead == "gt-abc" && recs[0].Kind == land.RecordKindAnnotation && recs[0].Key == key && recs[0].Text == msg && recs[0].At.Equal(h.now)) {
				t.Errorf("queued %+v; want gt-abc's annotation at %v", recs[0], h.now)
			}
			joined := strings.Join(logs, "\n")
			if want := "gt-abc: " + what + " (key " + key + "): bd down"; strings.Count(joined, want) != 1 {
				t.Errorf("log %q appears %d time(s); want once: %v", want, strings.Count(joined, want), logs)
			}
			if got := strings.Contains(joined, "queueing the annotation: queue rewrite refused"); got != tc.enqueueLogged {
				t.Errorf("enqueue error logged = %v; want %v: %v", got, tc.enqueueLogged, logs)
			}
			h.w.Beads = h.guard
			h.w.annotate("gt-abc", key, what, msg)
			if got := len(h.comments(t, "gt-abc")) == 1; got != tc.post {
				t.Errorf("second annotate posted = %v; want %v", got, tc.post)
			}
			if recs, err = q.Pending(); err != nil || len(recs) != tc.queued {
				t.Errorf("queue after the second annotate holds %d record(s), err %v; want %d", len(recs), err, tc.queued)
			}
		})
	}
}
