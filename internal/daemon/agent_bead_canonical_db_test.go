package daemon

import (
	"fmt"
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

// writeBeadsDirHonoringBD writes a fake bd that answers from the database
// BEADS_DIR names, like the real one: the agent bead (and, when given, the
// work bead assigned to the polecat) exist only in the rig database. Any
// other database reports the agent bead as not found. Every call's BEADS_DIR
// and argv is appended to the returned log path.
func writeBeadsDirHonoringBD(t *testing.T, agentInRig bool, hookBead, assignedWork string) (bdPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "bd.log")
	agentJSON := fmt.Sprintf(`[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"agent_state: working","hook_bead":"%s","agent_state":"working","updated_at":"%s"}]`,
		hookBead, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
	workJSON := "[]"
	if assignedWork != "" {
		workJSON = fmt.Sprintf(`[{"id":"%s","status":"hooked","assignee":"myr/polecats/mycat"}]`, assignedWork)
	}
	agentInRigFlag := "0"
	if agentInRig {
		agentInRigFlag = "1"
	}
	script := "#!/bin/sh\n" +
		"echo \"BEADS_DIR=$BEADS_DIR $*\" >> '" + logPath + "'\n" +
		"rig=0\n" +
		"case \"$BEADS_DIR\" in */myr/mayor/rig/.beads) rig=1;; esac\n" +
		"if [ \"$1\" = list ]; then\n" +
		"  case \"$*\" in\n" +
		"    *gt:agent*) if [ $rig = 1 ] && [ " + agentInRigFlag + " = 1 ]; then echo '" + agentJSON + "'; else echo '[]'; fi; exit 0;;\n" +
		"    *--status=hooked*) if [ $rig = 1 ]; then echo '" + workJSON + "'; else echo '[]'; fi; exit 0;;\n" +
		"  esac\n" +
		"  echo '[]'; exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = mol ]; then echo '[]'; exit 0; fi\n" +
		"if [ \"$1\" = show ]; then\n" +
		"  case \"$2\" in\n" +
		"    gt-myr-polecat-mycat)\n" +
		"      if [ $rig = 1 ] && [ " + agentInRigFlag + " = 1 ]; then echo '" + agentJSON + "'; exit 0; fi\n" +
		"      echo 'Error: no issue found matching \"gt-myr-polecat-mycat\"' >&2; exit 1;;\n" +
		"    *) echo \"[{\\\"id\\\":\\\"$2\\\",\\\"status\\\":\\\"hooked\\\"}]\"; exit 0;;\n" +
		"  esac\n" +
		"fi\n" +
		"echo '[]'\n"
	bdPath = filepath.Join(dir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bdPath, logPath
}

func newCanonicalDBDaemon(t *testing.T, townRoot, bdPath string, logBuf *strings.Builder) *Daemon {
	t.Helper()
	return &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
	}
}

func bdLog(t *testing.T, path string) string {
	t.Helper()
	data, _ := os.ReadFile(path)
	return string(data)
}

// G1-01: the polecat's agent bead lives only in the rig database. The daemon
// used to pin its read to the town database, get "not found", and return
// silently, so a crashed polecat with hooked work raised nothing.
func TestCheckPolecatHealth_ReadsRigLocalAgentBead(t *testing.T) {
	townRoot, _ := routedTown(t)
	bdPath, logPath := writeBeadsDirHonoringBD(t, true, "gt-work1", "")
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(t, townRoot, bdPath, &logBuf)

	d.checkPolecatHealth("myr", "mycat")

	if !strings.Contains(logBuf.String(), "CRASH DETECTED") {
		t.Fatalf("no CRASH DETECTED for a dead polecat whose agent bead is rig-local\nlog: %s\nbd calls:\n%s",
			logBuf.String(), bdLog(t, logPath))
	}
}

// G1-01: an agent bead that cannot be read is UNKNOWN, logged as such, never
// a silent return.
func TestCheckPolecatHealth_AgentBeadNotFoundIsLoggedUnknown(t *testing.T) {
	townRoot, _ := routedTown(t)
	bdPath, _ := writeBeadsDirHonoringBD(t, false, "", "")
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(t, townRoot, bdPath, &logBuf)

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "UNKNOWN") || !strings.Contains(got, "gt-myr-polecat-mycat") {
		t.Fatalf("agent-bead miss was not logged as UNKNOWN naming the bead: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Fatalf("acted on an unreadable agent bead: %q", got)
	}
}

// hook_bead has not been written since hq-l6mm5 (updateAgentHookBead is a
// no-op); the work bead's status+assignee is authoritative. A dead polecat
// with hooked work assigned must still raise a crash.
func TestCheckPolecatHealth_EmptyHookSlotUsesAssignedWork(t *testing.T) {
	townRoot, _ := routedTown(t)
	bdPath, logPath := writeBeadsDirHonoringBD(t, true, "", "gt-work2")
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(t, townRoot, bdPath, &logBuf)

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "CRASH DETECTED") || !strings.Contains(got, "gt-work2") {
		t.Fatalf("dead polecat with assigned hooked work gt-work2 raised no crash naming it\nlog: %s\nbd calls:\n%s",
			got, bdLog(t, logPath))
	}
}

// G1-01: the orphaned-work scan lists agent beads from the rig database, so a
// rig-local polecat with hooked work and a dead session is found.
func TestCheckRigOrphanedWork_ListsRigLocalAgentBeads(t *testing.T) {
	townRoot, _ := routedTown(t)
	bdPath, logPath := writeBeadsDirHonoringBD(t, true, "gt-work3", "")
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(t, townRoot, bdPath, &logBuf)

	d.checkRigOrphanedWork("myr")

	if !strings.Contains(logBuf.String(), "Orphaned work detected") {
		t.Fatalf("orphaned-work scan missed a rig-local agent bead\nlog: %s\nbd calls:\n%s",
			logBuf.String(), bdLog(t, logPath))
	}
}
