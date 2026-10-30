package attention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// landingBase is when the fixture landings were answered.
var landingBase = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// landingHeads are two full commit ids of one bead, long enough that their
// twelve-character forms differ.
const (
	landingHeadA = "6cf8b456abcd0123456789abcdef0123456789ab"
	landingHeadB = "0123456789abcdef0123456789abcdef01234567"
)

// landingRiskLine is one landing-log line in the shape the battery writes: the bead,
// the full head, and its risk answer.
func landingRiskLine(bead, head string, score float64) []byte {
	raw, err := json.Marshal(map[string]any{
		"ts":               landingBase,
		"bead":             bead,
		"rig":              "gastown",
		"head":             head,
		"landed_at":        landingBase.Add(-time.Minute),
		"model":            "jev-test",
		"answers":          map[string]any{"risk": map[string]any{"score": score, "confidence": 0.8}},
		"question_version": 1,
	})
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}

func appendLanding(t *testing.T, path string, lines ...[]byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G304: a test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, l := range lines {
		if _, err := f.Write(l); err != nil {
			t.Fatal(err)
		}
	}
}

// The reader joins a landing line to the risk-path item the collector raises for
// it: risk:<bead>:<first 12 characters of the head>. A line for another head of
// the same bead is another landing and does not answer this one, and the newest
// line for a landing is its answer.
func TestLandingRiskReaderJoinsTheBeadAndTheShortHead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "landing.jsonl")
	appendLanding(t, path,
		landingRiskLine("gt-abc", landingHeadA, 1),
		landingRiskLine("gt-abc", landingHeadB, 2),
		// A later answer for the first landing wins over the one before it.
		landingRiskLine("gt-abc", landingHeadA, 3),
		// A line whose call failed answers no landing, so it adds no key.
		append([]byte(`{"bead":"gt-def","head":"`+landingHeadB+`","answers":null}`), '\n'),
	)

	got, err := NewLandingRiskReader(path).Risks()
	if err != nil {
		t.Fatalf("reading the landing log: %v", err)
	}
	want := LandingRisks{
		"risk:gt-abc:" + landingHeadA[:12]: 3,
		"risk:gt-abc:" + landingHeadB[:12]: 2,
	}
	if len(got) != len(want) {
		t.Fatalf("the reader holds %v, want %v", got, want)
	}
	for key, score := range want {
		if got[key] != score {
			t.Errorf("%s scores %v, want %.1f (all: %v)", key, got[key], score, got)
		}
	}
}

// A landing the battery has not answered answers no item, so the queue is the
// order needs_human alone gives it. A log that exists but cannot be read keeps
// the answers of the last good read beside its error, rather than drawing an
// answered landing as unanswered.
func TestLandingRiskReaderMissingOrUnreadableLog(t *testing.T) {
	t.Parallel()

	t.Run("a missing log leaves today's order", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "landing.jsonl")
		risks, err := NewLandingRiskReader(path).Risks()
		if err != nil || len(risks) != 0 {
			t.Fatalf("a log nobody wrote read as %v, %v", risks, err)
		}
		items := []Item{
			{Key: "risk:gt-abc:" + landingHeadA[:12], Kind: KindRiskPath, FirstSeen: needsBase},
			{Key: "red-main:gastown", Kind: KindRedMain, FirstSeen: needsBase.Add(time.Minute)},
			{Key: "risk:gt-def:" + landingHeadB[:12], Kind: KindRiskPath, FirstSeen: needsBase.Add(2 * time.Minute)},
		}
		needs := score(nil, "red-main:gastown", 1, 0.5)
		got := keysOf(OrderByLandingRisk(items, needs, risks))
		want := keysOf(OrderByNeedsHuman(items, needs))
		if !sameKeys(got, want) {
			t.Errorf("an unwritten landing log ordered the queue %v, want today's %v", got, want)
		}
	})

	t.Run("an unreadable log keeps the last answers", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "landing.jsonl")
		r := NewLandingRiskReader(path)
		appendLanding(t, path, landingRiskLine("gt-abc", landingHeadA, 3))
		if _, err := r.Risks(); err != nil {
			t.Fatalf("the first read: %v", err)
		}

		// Replaced by something that is not a file: it stats, it opens, and it
		// cannot be read.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil { //nolint:gosec // G301: a test's own temp dir
			t.Fatal(err)
		}
		got, err := r.Risks()
		if err == nil {
			t.Fatalf("a log that could not be read reported no error, answers %v", got)
		}
		if score := got["risk:gt-abc:"+landingHeadA[:12]]; score != 3 {
			t.Errorf("the last good answer is %v after a failed read, want the 3 it read", score)
		}
	})
}
