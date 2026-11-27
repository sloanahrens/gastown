package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/metrics"
)

// archiveIssuesAtSHA is a commit id in the form git prints.
const archiveIssuesAtSHA = "0123456789abcdef0123456789abcdef01234567"

// archiveIssuesAtRunner answers the archive's listing and shards; failOn makes the call whose argv holds it fail.
func archiveIssuesAtRunner(t *testing.T, listing string, shards map[string]string, failOn string) metrics.Runner {
	return func(_ context.Context, argv []string) ([]byte, error) {
		if len(argv) < 5 {
			t.Fatalf("git argv %v does not name a listing or a shard", argv)
		}
		if failOn != "" && strings.Contains(strings.Join(argv, " "), failOn) {
			return nil, errors.New("refused")
		}
		switch argv[3] {
		case "ls-tree":
			return []byte(listing), nil
		case "show":
			if data, ok := shards[strings.TrimPrefix(argv[4], archiveIssuesAtSHA+":")]; ok {
				return []byte(data), nil
			}
			return nil, errors.New("no shard")
		}
		return nil, fmt.Errorf("unexpected argv %v", argv)
	}
}

func TestArchiveIssuesAt(t *testing.T) {
	t.Parallel()
	twoShards := map[string]string{"gt/issues.jsonl": `{"id":"a"}`, "gt/issues.2.jsonl": `{"id":"b"}`}
	cases := []struct {
		name, rig, sha, listing, failOn, want, wantErr string
		shards                                         map[string]string
	}{
		{"two shards joined in order, a newline added", "", "", "gt/issues.2.jsonl\ngt/issues.jsonl\n", "", `{"id":"a"}` + "\n" + `{"id":"b"}` + "\n", "", twoShards},
		{"a failed listing names the commit", "", "", "", "ls-tree", "", "listing gt at " + archiveIssuesAtSHA, twoShards},
		{"no issues file names the commit", "", "", "gt/events.jsonl\n", "", "", "gt at " + archiveIssuesAtSHA + " holds no issues file", nil},
		{"an unreadable shard names it and the commit", "", "", "gt/issues.jsonl\ngt/issues.2.jsonl\n", "gt/issues.2.jsonl", "", "gt/issues.2.jsonl at " + archiveIssuesAtSHA, twoShards},
		{"an empty shard adds no line of its own", "", "", "gt/issues.jsonl\ngt/issues.2.jsonl\n", "", `{"id":"b"}` + "\n", "", map[string]string{"gt/issues.jsonl": "", "gt/issues.2.jsonl": `{"id":"b"}`}},
		{"a last shard without a newline ends on its own line", "", "", "gt/issues.jsonl\ngt/issues.2.jsonl\n", "", `{"id":"a"}` + "\n" + `{"id":"b"}` + "\n", "", map[string]string{"gt/issues.jsonl": `{"id":"a"}` + "\n", "gt/issues.2.jsonl": `{"id":"b"}`}},
		{"an option-like commit is refused before any git call", "", "-0123456789abcdef0123456789abcdef0123456", "", "", "", `"-0123456789abcdef0123456789abcdef0123456" is not a commit id`, nil},
		{"a rig name is required before any git call", "../other", "", "", "", "", `"../other" is not a database name`, nil},
		{"an option-like rig is refused before any git call", "-x", "", "", "", "", `"-x" is not a database name`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := archiveIssuesAt(context.Background(), archiveIssuesAtRunner(t, tc.listing, tc.shards, tc.failOn), "/repo", cmp.Or(tc.rig, "gt"), cmp.Or(tc.sha, archiveIssuesAtSHA))
			ok := err == nil && string(got) == tc.want
			if tc.wantErr != "" {
				ok = err != nil && got == nil && strings.Contains(err.Error(), tc.wantErr)
			}
			if !ok {
				t.Fatalf("archiveIssuesAt = %q, %v; want %q, or no bytes and an error naming %q", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
