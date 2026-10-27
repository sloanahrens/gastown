package land

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestRejectedHeadReadsTheLiveRejection(t *testing.T) {
	t.Parallel()
	notes := FormatRejectionNote(RejectionNote{
		Attempt: 1, Kind: "gate", Reason: "red", Branch: "polecat/a/gt-x", Target: "main", MR: "gt-x",
		Head: "1111111111111111111111111111111111111111",
	})
	got, ok := RejectedHead(notes)
	if !ok || got != "1111111111111111111111111111111111111111" {
		t.Fatalf("RejectedHead = %q, %v; want the rejected tip", got, ok)
	}
}

func TestRejectedHeadPrefersTheLastAttempt(t *testing.T) {
	t.Parallel()
	notes := FormatRejectionNote(RejectionNote{Attempt: 1, Kind: "gate", Reason: "red", Branch: "b", Target: "main", MR: "gt-x", Head: "aaaa"}) +
		"\n" + FormatRejectionNote(RejectionNote{Attempt: 2, Kind: "gate", Reason: "still red", Branch: "b", Target: "main", MR: "gt-x", Head: "bbbb"})
	if got, ok := RejectedHead(notes); !ok || got != "bbbb" {
		t.Fatalf("RejectedHead = %q, %v; want the live (last) rejection's head", got, ok)
	}
}

func TestRejectedHeadWithoutABlock(t *testing.T) {
	t.Parallel()
	for name, notes := range map[string]string{
		"no rejection":    "Findings: look at the helper.\n",
		"no head line":    FormatRejectionNote(RejectionNote{Attempt: 1, Kind: "gate", Reason: "red", Branch: "b", Target: "main", MR: "gt-x"}),
		"a quoted header": "the gate tail said MERGE REJECTION (attempt 1): gate - x\n",
		"empty notes":     "",
	} {
		if got, ok := RejectedHead(notes); ok {
			t.Errorf("%s: RejectedHead = %q, true; want ok=false", name, got)
		}
	}
}

func TestHeadsEqual(t *testing.T) {
	t.Parallel()
	full := "0123456789abcdef0123456789abcdef01234567"
	for name, tc := range map[string]struct {
		a, b string
		want bool
	}{
		"identical":          {full, full, true},
		"case insensitive":   {full, "0123456789ABCDEF0123456789ABCDEF01234567", true},
		"whitespace trimmed": {" " + full + "\n", full, true},
		"abbreviated prefix": {"0123456", full, true},
		"other abbreviated":  {full[:10], full, true},
		"different":          {full, "fedcba9876543210fedcba9876543210fedcba98", false},
		"too short to match": {"01234", full, false},
		"one side empty":     {"", full, false},
		"both empty":         {"", "", false},
	} {
		if got := HeadsEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: HeadsEqual(%q, %q) = %v, want %v", name, tc.a, tc.b, got, tc.want)
		}
	}
}

func TestNewestRequeueReadsTheNewestRequeue(t *testing.T) {
	t.Parallel()
	head := "1111111111111111111111111111111111111111"
	comments := []beads.Comment{
		{Text: FormatRequeueComment("sloan", "polecat/a/gt-x", head, 1, 1, "runner fault")},
		{Text: "commit: abc — the fix"},
		{Text: FormatRequeueComment("sloan", "polecat/a/gt-x", head, 2, 3, "runner fault again")},
	}
	got, ok := NewestRequeue(comments)
	if !ok || got.Attempt != 2 || got.Blocks != 3 {
		t.Fatalf("NewestRequeue = %+v, %v; want the newest requeue's attempt 2 of 3 blocks", got, ok)
	}
}

func TestNewestRequeueWithoutARequeue(t *testing.T) {
	t.Parallel()
	for name, comments := range map[string][]beads.Comment{
		"no comments":      nil,
		"none are requeue": {{Text: "commit: abc — the fix"}, {Text: "Submitted for landing"}},
		"marker quoted":    {{Text: "the REQUEUED: comment means it went back in the queue"}},
		"no attempt line":  {{Text: "REQUEUED: sloan re-queued this rejected landing unchanged\nBranch: b\nHead: abc\nReason: x"}},
	} {
		if got, ok := NewestRequeue(comments); ok {
			t.Errorf("%s: NewestRequeue = %+v, true; want ok=false", name, got)
		}
	}
}

// TestNewestRequeueWithoutABlockCount is gt-kwe1l's fallback: a comment written
// before the Blocks: line existed still names its attempt, with no count.
func TestNewestRequeueWithoutABlockCount(t *testing.T) {
	t.Parallel()
	comments := []beads.Comment{{Text: "REQUEUED: sloan re-queued this rejected landing unchanged\nBranch: b\nHead: abc\nAttempt: 3\nReason: x"}}
	got, ok := NewestRequeue(comments)
	if !ok || got.Attempt != 3 || got.Blocks != 0 {
		t.Fatalf("NewestRequeue = %+v, %v; want attempt 3 and no block count", got, ok)
	}
}
