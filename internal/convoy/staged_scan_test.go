package convoy

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestStrandedScanExcludesStagedConvoys verifies that findStrandedConvoys
// lists convoys with the gt:convoy label and --status=open, which excludes
// staged convoys (status "staged_ready" or "staged_warnings"), and so never
// reports one (gt-csl.5.2). bd answers a list without --status=open with a
// staged convoy, so a query that dropped the filter would leak it.
func TestStrandedScanExcludesStagedConvoys(t *testing.T) {
	t.Parallel()
	bd := &bdScript{answer: func(c beads.BDCall) (string, string, int) {
		line := strings.Join(c.Args, " ")
		if positional(c.Args)[0] == "list" && !strings.Contains(line, "--status=open") {
			return `[{"id":"hq-cv-staged1","title":"Staged convoy"}]`, "", 0
		}
		return "[]", "", 0
	}}

	stranded, err := testTown(townWithBeads(t, ""), bd, nil).findStrandedWith(context.Background(), noBlockers)
	if err != nil {
		t.Fatalf("findStrandedConvoys() error: %v", err)
	}
	if len(stranded) != 0 {
		t.Errorf("stranded = %+v, want none: a staged convoy leaked into the scan", stranded)
	}

	lists := bd.ran("list")
	if len(lists) == 0 {
		t.Fatalf("bd was never called with a 'list' subcommand: %q", bd.argvs())
	}
	listLine := strings.Join(lists[0].Args, " ")
	for _, flag := range []string{"--label=gt:convoy", "--status=open", "--json", "--limit=0", "--flat"} {
		if !strings.Contains(listLine, flag) {
			t.Errorf("bd list command missing %q; got: %q", flag, listLine)
		}
	}
}
