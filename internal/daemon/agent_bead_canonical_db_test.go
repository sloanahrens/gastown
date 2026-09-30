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

// gt-fcxe9.7: the orphaned-work scan lists agent beads from the rig database, so a
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

// writeScriptBD writes a fake bd from a shell body and returns its path.
func writeScriptBD(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// With the hook_bead slot empty, crash detection reads the assigned work. If
// that read fails, the answer is unknown: log UNKNOWN, raise no crash, and
// never read the failure as "no work".
func TestCheckPolecatHealth_AssignedWorkReadFailureIsUnknown(t *testing.T) {
	townRoot, _ := routedTown(t)
	agentJSON := `[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"agent_state: working","hook_bead":"","agent_state":"working","updated_at":"2026-01-01T00:00:00Z"}]`
	bdPath := writeScriptBD(t, ""+
		"if [ \"$1\" = show ]; then echo '"+agentJSON+"'; exit 0; fi\n"+
		"case \"$*\" in *--status=hooked*) echo 'Error: database is locked' >&2; exit 1;; esac\n"+
		"echo '[]'\n")
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(t, townRoot, bdPath, &logBuf)

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "UNKNOWN") || !strings.Contains(got, "assigned work") {
		t.Fatalf("assigned-work read failure not logged as UNKNOWN: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Fatalf("acted on an unknown assigned-work answer: %q", got)
	}
}

// The orphaned-work TOCTOU re-read of hook_bead failing is unknown, logged,
// and not acted on; it is never read as "hook cleared" silently.
func TestCheckRigOrphanedWork_ReReadFailureIsLoggedUnknown(t *testing.T) {
	townRoot, _ := routedTown(t)
	agentJSON := `[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"agent_state: working","hook_bead":"gt-work4","agent_state":"working"}]`
	bdPath := writeScriptBD(t, ""+
		"case \"$*\" in *gt:agent*) echo '"+agentJSON+"'; exit 0;; esac\n"+
		"if [ \"$1\" = show ]; then echo 'Error: connection refused' >&2; exit 1; fi\n"+
		"echo '[]'\n")
	var logBuf strings.Builder
	rec := notifyfake.New()
	d := newCanonicalDBDaemon(t, townRoot, bdPath, &logBuf)
	d.notifier = rec

	d.checkRigOrphanedWork("myr")

	got := logBuf.String()
	if !strings.Contains(got, "re-read failed") {
		t.Fatalf("hook re-read failure was not logged: %q", got)
	}
	if strings.Contains(got, "Orphaned work detected") {
		t.Fatalf("acted on an unknown re-read: %q", got)
	}
}
