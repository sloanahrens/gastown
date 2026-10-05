package land

import "testing"

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
