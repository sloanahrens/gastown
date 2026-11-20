package daemon

import (
	"strings"
	"testing"
)

// TestNodePruneReportReadsBothSummaries reads one dry-run log and one applied
// log: the dry run reports its planned count and its would-remove lines in
// order, the applied run reports its removed and failed counts.
func TestNodePruneReportReadsBothSummaries(t *testing.T) {
	t.Parallel()

	dryRun := `node-prune: would remove sha256:aaa gastown-polecat created 2026-11-20T03:00:00Z
node-prune: would remove sha256:bbb gastown-daemon created 2026-11-20T03:01:00Z
node-prune: would remove sha256:ccc gastown-bd created 2026-11-20T03:02:00Z
node-prune: dry run planned 3 images
`
	rep, err := parseNodePruneLog([]byte(dryRun))
	if err != nil {
		t.Fatalf("parseNodePruneLog(dry run) error = %v", err)
	}
	if rep.Planned != 3 {
		t.Errorf("dry run Planned = %d, want 3", rep.Planned)
	}
	want := []string{
		"node-prune: would remove sha256:aaa gastown-polecat created 2026-11-20T03:00:00Z",
		"node-prune: would remove sha256:bbb gastown-daemon created 2026-11-20T03:01:00Z",
		"node-prune: would remove sha256:ccc gastown-bd created 2026-11-20T03:02:00Z",
	}
	if len(rep.Lines) != len(want) {
		t.Fatalf("dry run Lines = %q, want %d lines", rep.Lines, len(want))
	}
	for i := range want {
		if rep.Lines[i] != want[i] {
			t.Errorf("dry run Lines[%d] = %q, want %q", i, rep.Lines[i], want[i])
		}
	}
	if rep.Removed != 0 || rep.Failed != 0 {
		t.Errorf("dry run Removed/Failed = %d/%d, want 0/0", rep.Removed, rep.Failed)
	}

	applied := `node-prune: removed sha256:aaa
node-prune: cannot remove sha256:bbb: exit status 1
node-prune: applied removed 2 failed 1
`
	rep, err = parseNodePruneLog([]byte(applied))
	if err != nil {
		t.Fatalf("parseNodePruneLog(applied) error = %v", err)
	}
	if rep.Removed != 2 || rep.Failed != 1 {
		t.Errorf("applied Removed/Failed = %d/%d, want 2/1", rep.Removed, rep.Failed)
	}
	if rep.Planned != 0 || len(rep.Lines) != 0 {
		t.Errorf("applied Planned/Lines = %d/%q, want 0/none", rep.Planned, rep.Lines)
	}

	rep, err = parseNodePruneLog([]byte("node-prune: dry run planned 0 images\n"))
	if err != nil || rep.Planned != 0 || len(rep.Lines) != 0 {
		t.Errorf("an empty plan = %+v, %v, want Planned 0 and no lines", rep, err)
	}
}

// TestNodePruneReportRefuses drives each log the report cannot read: an empty
// log, a log with no summary, a summary of the wrong shape or with a count
// that does not parse or is negative, a log with both summaries, and a
// truncated would-remove line are each an error, never a zero report.
func TestNodePruneReportRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		log  string
	}{
		{"empty", ""},
		{"no summary", "node-prune: would remove sha256:aaa gastown-polecat created 2026-11-20T03:00:00Z\n"},
		{"non-numeric planned", "node-prune: dry run planned many images\n"},
		{"non-numeric removed", "node-prune: applied removed many failed 0\n"},
		{"non-numeric failed", "node-prune: applied removed 1 failed many\n"},
		{"negative count", "node-prune: dry run planned -1 images\n"},
		{"both summaries", "node-prune: dry run planned 1 images\nnode-prune: applied removed 1 failed 0\n"},
		{"repeated summary", "node-prune: dry run planned 1 images\nnode-prune: dry run planned 1 images\n"},
		{"malformed planned summary", "node-prune: dry run planned 3\n"},
		{"malformed applied summary", "node-prune: applied removed 1 of 2\n"},
		{"truncated would-remove", "node-prune: would remove sha256:aaa\nnode-prune: dry run planned 1 images\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, err := parseNodePruneLog([]byte(tt.log))
			if err == nil {
				t.Fatalf("parseNodePruneLog(%q) = %+v, want an error", tt.log, rep)
			}
			if !strings.Contains(err.Error(), "node-prune") {
				t.Errorf("error %q does not name the problem", err)
			}
		})
	}
}
