package cmd

import (
	"reflect"
	"testing"
)

func TestArchiveIssueShards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, listing string
		want          []string
	}{
		{"number order past ten", "gt/issues.10.jsonl\ngt/issues.jsonl\ngt/issues.2.jsonl\ngt/issues.11.jsonl\ngt/issues.9.jsonl\n", []string{"gt/issues.jsonl", "gt/issues.2.jsonl", "gt/issues.9.jsonl", "gt/issues.10.jsonl", "gt/issues.11.jsonl"}},
		{"look-alikes", "gt/issues.jsonl\ngt/issues.02.jsonl\ngt/issues.+2.jsonl\ngt/issues.x.jsonl\ngt/issues_3.jsonl\ngt/issues2.jsonl\ngt/issues.2.jsonl.bak\ngt/issues.1.jsonl\ngt/events.jsonl\n", []string{"gt/issues.jsonl"}},
		{"blank lines and no match", "\n  \ngt/events.jsonl\nhq/issues.jsonl.bak\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := archiveIssueShards(tc.listing); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("archiveIssueShards(%q) = %q, want %q", tc.listing, got, tc.want)
			}
		})
	}
}
