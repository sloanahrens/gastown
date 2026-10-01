package beads

import (
	"strings"
	"testing"
)

// The purge carries its grace period to bd: without --older-than, bd purge
// --force deletes a just-closed merge-request bead outright (gt-1q46).
func TestPurgeClosedEphemeral_PassesOlderThan(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stdout: "3\n"} })
	got, err := newRecordedBeads(t.TempDir(), rec).PurgeClosedEphemeral("48h")
	if err != nil {
		t.Fatalf("PurgeClosedEphemeral: %v", err)
	}
	if got != "3" {
		t.Errorf("purged = %q, want 3", got)
	}
	var purges []string
	for _, argv := range rec.argvs() {
		if strings.Contains(argv, "purge") {
			purges = append(purges, argv)
		}
	}
	if len(purges) != 1 || !strings.Contains(purges[0], "purge --force --quiet --older-than 48h") {
		t.Fatalf("purge calls = %q, want one purge --force --quiet --older-than 48h", purges)
	}
}
