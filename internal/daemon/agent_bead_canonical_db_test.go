package daemon

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// routedTown writes a town whose routes.jsonl sends the gt- prefix to rig
// "myr" at myr/mayor/rig, the layout since gt-a6g: a rig-prefixed agent bead
// lives in the rig's database, not the town's. It returns the town root and
// the rig's .beads directory.
func routedTown(t *testing.T) (townRoot, rigBeads string) {
	t.Helper()
	townRoot = t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	rigBeads = filepath.Join(townRoot, "myr", "mayor", "rig", ".beads")
	for _, d := range []string{townBeads, rigBeads} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	routes := `{"prefix":"hq-","path":"."}` + "\n" + `{"prefix":"gt-","path":"myr/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot, rigBeads
}

func newCanonicalDBDaemon(townRoot string, bd *workBD, logBuf *strings.Builder) *Daemon {
	return &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        log.New(logBuf, "", 0),
		tmux:          newFakeTmux(newFixedClock()),
		notifier:      notifyfake.New(),
		openWorkBeads: bd.open,
		execCmd:       bd.run,
	}
}

// hook_bead has not been written since hq-l6mm5 (updateAgentHookBead is a
// no-op); the work bead's status+assignee is authoritative. A dead polecat
// with hooked work assigned in its rig's database must still raise a crash.
func TestCheckPolecatHealth_EmptyHookSlotUsesAssignedWork(t *testing.T) {
	t.Parallel()
	townRoot, rigBeads := routedTown(t)
	bd := newWorkBD(t)
	bd.onlyIn = rigBeads
	bd.seed("gt-work2", "hooked", time.Now().Add(-time.Hour))
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(townRoot, bd, &logBuf)

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "CRASH DETECTED") || !strings.Contains(got, "gt-work2") {
		t.Fatalf("dead polecat with assigned hooked work gt-work2 raised no crash naming it\nlog: %s\nbd reads:\n%s",
			got, bd.calls(t))
	}
}

// With the hook_bead slot empty, crash detection reads the assigned work. If
// that read fails, the answer is unknown: log UNKNOWN, raise no crash, and
// never read the failure as "no work".
func TestCheckPolecatHealth_AssignedWorkReadFailureIsUnknown(t *testing.T) {
	t.Parallel()
	townRoot, _ := routedTown(t)
	bd := newWorkBD(t)
	bd.listErr = errors.New("database is locked")
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(townRoot, bd, &logBuf)

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "UNKNOWN") || !strings.Contains(got, "assigned work") {
		t.Fatalf("assigned-work read failure not logged as UNKNOWN: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Fatalf("acted on an unknown assigned-work answer: %q", got)
	}
}
