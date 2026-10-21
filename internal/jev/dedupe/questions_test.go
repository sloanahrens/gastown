package dedupe

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

// questionSetHashes pins the wording of the question set to its Version. Each
// Version maps to the hash questionSetHash produced when that Version shipped.
// When a question id, its text, an option text or a severity level changes,
// TestJevDedupeVersionTracksTheQuestionSet fails: bump Version and add the new
// hash here under it. Keep the older entries, so a logged line's
// question_version still names the wording it was asked with.
var questionSetHashes = map[int]string{
	1: "db779a0e9344eaf0c1e2181696c0626575d5fcda95ab42c48dd1310adedc1de7",
}

// questionSetHash fingerprints everything a question's wording consists of: the
// fixed battery, and the duplicate Choice's instructions and option texts over
// two sample candidate ids.
func questionSetHash(t *testing.T) string {
	t.Helper()
	set := map[string]any{
		"battery":   Questions(),
		"duplicate": duplicateChoice([]string{"gt-a", "gt-b"}),
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// Version is written to every log line to tell answers to different wording
// apart, so changing the wording without bumping it would mix them.
func TestJevDedupeVersionTracksTheQuestionSet(t *testing.T) {
	t.Parallel()
	got := questionSetHash(t)
	want, ok := questionSetHashes[Version]
	if !ok {
		t.Fatalf("Version %d has no entry in questionSetHashes; add %q under it", Version, got)
	}
	if got != want {
		t.Errorf("the question set hashes to %s, but Version %d pins %s: "+
			"a question id, its text or a severity level changed, so bump Version "+
			"and add the new hash to questionSetHashes", got, Version, want)
	}
	seen := map[string]int{}
	for v, h := range questionSetHashes {
		if v > Version {
			t.Errorf("questionSetHashes has version %d, above Version %d", v, Version)
		}
		if prev, dup := seen[h]; dup {
			t.Errorf("versions %d and %d pin the same hash: a bump needs changed wording", prev, v)
		}
		seen[h] = v
	}
}
