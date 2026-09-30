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

// beadsDirHonoringBD is a bd that answers from the database BEADS_DIR names,
// like the real one: the agent bead (and, when given, the work bead assigned
// to the polecat) exist only in the rig database. Any other database reports
// the agent bead as not found. Every call's BEADS_DIR and argv is recorded.
func beadsDirHonoringBD(agentInRig bool, hookBead, assignedWork string) *fakeCLI {
	agentJSON := fmt.Sprintf(`[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"agent_state: working","hook_bead":"%s","agent_state":"working","updated_at":"%s"}]`,
		hookBead, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
	workJSON := "[]"
	if assignedWork != "" {
		workJSON = fmt.Sprintf(`[{"id":"%s","status":"hooked","assignee":"myr/polecats/mycat"}]`, assignedWork)
	}
	return newFakeCLIFor(func(c cliCall) cliReply {
		rig := strings.HasSuffix(c.getenv("BEADS_DIR"), "/myr/mayor/rig/.beads")
		argv := strings.Join(c.args, " ")
		if len(c.args) == 0 {
			return cliReply{stdout: "[]\n"}
		}
		switch c.args[0] {
		case "list":
			switch {
			case strings.Contains(argv, "gt:agent"):
				if rig && agentInRig {
					return cliReply{stdout: agentJSON + "\n"}
				}
				return cliReply{stdout: "[]\n"}
			case strings.Contains(argv, "--status=hooked"):
				if rig {
					return cliReply{stdout: workJSON + "\n"}
				}
				return cliReply{stdout: "[]\n"}
			}
			return cliReply{stdout: "[]\n"}
		case "mol":
			return cliReply{stdout: "[]\n"}
		case "show":
			id := ""
			if len(c.args) > 1 {
				id = c.args[1]
			}
			if id == "gt-myr-polecat-mycat" {
				if rig && agentInRig {
					return cliReply{stdout: agentJSON + "\n"}
				}
				return cliReply{stderr: `Error: no issue found matching "gt-myr-polecat-mycat"` + "\n", code: 1}
			}
			return cliReply{stdout: `[{"id":"` + id + `","status":"hooked"}]` + "\n"}
		}
		return cliReply{stdout: "[]\n"}
	})
}

func newCanonicalDBDaemon(t *testing.T, townRoot string, bd *fakeCLI, logBuf *strings.Builder) *Daemon {
	t.Helper()
	return &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
		bdPath:   "bd",
		execCmd:  bd.run,
	}
}

// bdLog renders bd's recorded calls, one "BEADS_DIR=<dir> <argv>" per line.
func bdLog(bd *fakeCLI) string {
	var b strings.Builder
	for _, c := range bd.recorded() {
		fmt.Fprintf(&b, "BEADS_DIR=%s %s\n", c.getenv("BEADS_DIR"), strings.Join(c.args, " "))
	}
	return b.String()
}

// hook_bead has not been written since hq-l6mm5 (updateAgentHookBead is a
// no-op); the work bead's status+assignee is authoritative. A dead polecat
// with hooked work assigned must still raise a crash.
func TestCheckPolecatHealth_EmptyHookSlotUsesAssignedWork(t *testing.T) {
	t.Parallel()
	townRoot, _ := routedTown(t)
	bd := beadsDirHonoringBD(true, "", "gt-work2")
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(t, townRoot, bd, &logBuf)

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "CRASH DETECTED") || !strings.Contains(got, "gt-work2") {
		t.Fatalf("dead polecat with assigned hooked work gt-work2 raised no crash naming it\nlog: %s\nbd calls:\n%s",
			got, bdLog(bd))
	}
}

// With the hook_bead slot empty, crash detection reads the assigned work. If
// that read fails, the answer is unknown: log UNKNOWN, raise no crash, and
// never read the failure as "no work".
func TestCheckPolecatHealth_AssignedWorkReadFailureIsUnknown(t *testing.T) {
	t.Parallel()
	townRoot, _ := routedTown(t)
	agentJSON := `[{"id":"gt-myr-polecat-mycat","issue_type":"agent","labels":["gt:agent"],"description":"agent_state: working","hook_bead":"","agent_state":"working","updated_at":"2026-01-01T00:00:00Z"}]`
	bd := newFakeCLI(func(args []string) cliReply {
		if len(args) > 0 && args[0] == "show" {
			return cliReply{stdout: agentJSON + "\n"}
		}
		if strings.Contains(strings.Join(args, " "), "--status=hooked") {
			return cliReply{stderr: "Error: database is locked\n", code: 1}
		}
		return cliReply{stdout: "[]\n"}
	})
	var logBuf strings.Builder
	d := newCanonicalDBDaemon(t, townRoot, bd, &logBuf)

	d.checkPolecatHealth("myr", "mycat")

	got := logBuf.String()
	if !strings.Contains(got, "UNKNOWN") || !strings.Contains(got, "assigned work") {
		t.Fatalf("assigned-work read failure not logged as UNKNOWN: %q", got)
	}
	if strings.Contains(got, "CRASH DETECTED") {
		t.Fatalf("acted on an unknown assigned-work answer: %q", got)
	}
}
