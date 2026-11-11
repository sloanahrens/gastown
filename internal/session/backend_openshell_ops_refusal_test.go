package session

import (
	"errors"
	"strings"
	"testing"
)

// divergedStopReplies scripts a stop's push of branch b, refused by origin, in
// a sandbox whose clone holds aaa and origin holds bbb: the two heads diverged
// with merge-base mmm, the clone holding 2 commits origin lacks and origin 1.
func divergedStopReplies() map[string][]gitReply {
	return map[string][]gitReply{
		"push origin b":                   {refusedPushReply},
		"fetch origin b":                  {{}},
		"rev-parse --verify refs/heads/b": {{stdout: "aaa\n"}},
		"rev-parse --verify FETCH_HEAD":   {{stdout: "bbb\n"}},
		"merge-base aaa bbb":              {{stdout: "mmm\n"}},
		"merge-base bbb aaa":              {{stdout: "mmm\n"}},
		"rev-list --count bbb..aaa":       {{stdout: "2\n"}},
		"rev-list --count aaa..bbb":       {{stdout: "1\n"}},
	}
}

// Each read the refused-push decision makes in the sandbox clone, when it
// fails, is named in the error the stop keeps, and the stop keeps the sandbox
// rather than reading the failure as a push that went through. Each row
// overrides one scripted command with a failure or an answer that cannot be
// read.
func TestOpenShellOpsPushWorkReadFailuresAreNamed(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 128")
	cases := []struct {
		name     string
		override map[string][]gitReply
		wantText string
	}{
		{"clone_head_unreadable", map[string][]gitReply{
			"rev-parse --verify refs/heads/b": {{err: boom}},
		}, "reading the clone's head failed"},
		{"fetched_head_unreadable", map[string][]gitReply{
			"rev-parse --verify FETCH_HEAD": {{err: boom}},
		}, "reading origin's fetched head failed"},
		{"ancestry_unreadable", map[string][]gitReply{
			"merge-base aaa bbb": {{err: boom}},
		}, "comparing it with origin's head failed"},
		{"ahead_retry_push_fails", map[string][]gitReply{
			"merge-base aaa bbb": {{stdout: "mmm\n"}},
			"merge-base bbb aaa": {{stdout: "bbb\n"}},
			"push origin b":      {refusedPushReply, {err: boom}},
		}, "pushing b:"},
		{"diverged_merge_base_unreadable", map[string][]gitReply{
			"merge-base aaa bbb": {{stdout: "mmm\n"}, {err: boom}},
		}, "could not be read"},
		{"diverged_local_count_unreadable", map[string][]gitReply{
			"rev-list --count bbb..aaa": {{err: boom}},
		}, "could not be read"},
		{"diverged_remote_count_unreadable", map[string][]gitReply{
			"rev-list --count aaa..bbb": {{err: boom}},
		}, "could not be read"},
		{"diverged_count_not_a_number", map[string][]gitReply{
			"rev-list --count bbb..aaa": {{stdout: "two\n"}},
		}, "parsing commit count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			replies := divergedStopReplies()
			for key, list := range tc.override {
				replies[key] = list
			}
			b, _ := scriptedStopBackend(t, replies)
			err := b.pushWork(pushRec("b"))
			if err == nil {
				t.Fatal("pushWork = nil, want an error so the sandbox is kept")
			}
			if errors.Is(err, ErrNothingToPush) || !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("pushWork = %v, want an error that is not nothing-to-push and names %q", err, tc.wantText)
			}
		})
	}
}
