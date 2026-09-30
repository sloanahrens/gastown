package git

import (
	"testing"
	"time"
)

func TestCommitTimeParsesCommitterTime(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"show -s --format=%cI abc": ok("2026-09-30T13:36:05-05:00\n")})
	got, err := newTestGit(t, s).CommitTime("abc")
	want := time.Date(2026, 9, 30, 18, 36, 5, 0, time.UTC)
	if err != nil || !got.Equal(want) {
		t.Fatalf("CommitTime = %v, %v; want %v", got, err, want)
	}
}

func TestCommitTimeFailsOnUnknownRevAndBadOutput(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"show -s --format=%cI nope": fail(128, "fatal: ambiguous argument 'nope': unknown revision or path not in the working tree."),
		"show -s --format=%cI odd":  ok("not a time\n"),
	})
	g := newTestGit(t, s)
	for _, rev := range []string{"nope", "odd"} {
		if got, err := g.CommitTime(rev); err == nil {
			t.Errorf("CommitTime(%s) = %v, nil; want an error", rev, got)
		}
	}
}
