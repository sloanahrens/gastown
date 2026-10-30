package shelltier_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/shelltier"
)

func TestRedSummaryParsesWithLandsRegexp(t *testing.T) {
	t.Parallel()
	names := []string{"scripts/a_test.sh", "scripts/b_test.sh"}
	line := shelltier.Summary(shelltier.Result{
		Passed:      5,
		Failed:      2,
		FailedNames: names,
		Elapsed:     134 * time.Second,
	}, "/tmp/tier-sweep.aB12")

	// The regexp tolerates a missing marker, so pin the marker on the line it parses.
	if !strings.HasSuffix(line, " (logs /tmp/tier-sweep.aB12) in 2m14s") {
		t.Fatalf("summary line %q does not end with its logs marker and duration", line)
	}
	m := land.ShellTierSummaryRE.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		t.Fatalf("land.ShellTierSummaryRE does not match %q", line)
	}
	if got := strings.Fields(m[1]); !slices.Equal(got, names) {
		t.Errorf("captured names = %q, want %q", got, names)
	}
}
