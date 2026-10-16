package land

import (
	"errors"
	"strings"
	"testing"
)

type recordingAppender struct {
	lines []string
	err   error
}

func (r *recordingAppender) AppendNotes(id, note string) error {
	if r.err != nil {
		return r.err
	}
	r.lines = append(r.lines, id+": "+note)
	return nil
}

func TestRecordOverseerReview(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	fail := OverseerReviewMarker + " " + head + " FAIL findings=gt-f1"
	pass := OverseerReviewMarker + " " + head + " PASS"
	for _, tc := range []struct {
		name      string
		notes     string
		verdict   string
		findings  []string
		reason    string
		want      string // the line written, empty when none
		conflict  bool
		recording error
	}{
		{name: "no review yet", verdict: "PASS", want: pass},
		{name: "FAIL carries findings", verdict: "FAIL", findings: []string{"gt-f1", "gt-f2"}, want: OverseerReviewMarker + " " + head + " FAIL findings=gt-f1,gt-f2"},
		{name: "AUDITED carries a one-line reason", verdict: "AUDITED", reason: "findings resolved\nby gt-f1", want: OverseerReviewMarker + " " + head + " AUDITED: findings resolved by gt-f1"},
		{name: "a FAIL is superseded", notes: fail, verdict: "AUDITED", reason: "r", want: OverseerReviewMarker + " " + head + " AUDITED: r"},
		{name: "the same verdict twice writes once", notes: pass, verdict: "PASS"},
		{name: "another verdict is refused", notes: pass, verdict: "WAIVED", reason: "r", conflict: true},
		{name: "a review of another head does not count", notes: OverseerReviewMarker + " " + strings.Repeat("b", 40) + " PASS", verdict: "PASS", want: pass},
		{name: "a write error is returned", verdict: "PASS", recording: errors.New("bd down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &recordingAppender{err: tc.recording}
			line, written, err := RecordOverseerReview(a, "gt-abc", tc.notes, head, tc.verdict, tc.findings, tc.reason)
			var ce *ReviewConflictError
			switch {
			case tc.conflict:
				if !errors.As(err, &ce) || written || len(a.lines) != 0 {
					t.Fatalf("written = %v, err = %v, lines = %q, want a ReviewConflictError and no write", written, err, a.lines)
				}
			case tc.recording != nil:
				if !errors.Is(err, tc.recording) || written {
					t.Fatalf("written = %v, err = %v, want the append error", written, err)
				}
			case tc.want == "":
				if err != nil || written || len(a.lines) != 0 {
					t.Fatalf("written = %v, err = %v, lines = %q, want no write", written, err, a.lines)
				}
			default:
				if err != nil || !written || line != tc.want || len(a.lines) != 1 || a.lines[0] != "gt-abc: "+tc.want {
					t.Fatalf("line = %q, written = %v, err = %v, lines = %q, want %q", line, written, err, a.lines, tc.want)
				}
			}
		})
	}
}
