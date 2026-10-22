package util

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/doltargv"
)

// StrayDoltProcess is what a remedy needs to know about a Dolt server on the
// machine: its pid, its listening port, and its data-dir. Port and DataDir are
// zero/empty when the process's argv named neither.
type StrayDoltProcess struct {
	PID     int
	Port    int
	DataDir string
}

// StrayDoltFinding is one stray process with the command that clears it.
type StrayDoltFinding struct {
	Process StrayDoltProcess
	Remedy  string
}

// StrayDoltRemedy names the one command that clears a stray Dolt process.
//
// A process on the town's port or data-dir is an imposter on the town's own
// endpoint, and `gt dolt kill-imposters` matches it. Any other stray is a leak
// that command cannot reach — it only looks at the town's port — so the remedy
// is `kill <pid>`, the pid alone and never a pattern: a pattern wide enough to
// catch the leak can also match another session's server (gt-gyw5w).
func StrayDoltRemedy(p StrayDoltProcess, townPort int, townDataDir string) string {
	if (townPort != 0 && p.Port == townPort) || (townDataDir != "" && p.DataDir == townDataDir) {
		return "gt dolt kill-imposters"
	}
	return fmt.Sprintf("kill %d", p.PID)
}

// ClassifyStrayDolt maps a stray-process listing to one finding per process.
// An empty list yields no findings: nothing to report is not a finding.
func ClassifyStrayDolt(procs []StrayDoltProcess, townPort int, townDataDir string) []StrayDoltFinding {
	findings := make([]StrayDoltFinding, 0, len(procs))
	for _, p := range procs {
		findings = append(findings, StrayDoltFinding{
			Process: p,
			Remedy:  StrayDoltRemedy(p, townPort, townDataDir),
		})
	}
	return findings
}

// IsDoltSQLServerArgs reports whether an argv is a `dolt sql-server`
// invocation. The matching rules live in doltargv.IsSQLServer, the one matcher
// that gt down and hoststat share too; they sit in a leaf package because
// hoststat cannot import util.
//
// No build tag: doltserver's pre-signal identity check needs it on every
// platform, and the orphan scan in dolt_orphan.go (non-Windows) calls it too.
func IsDoltSQLServerArgs(args []string) bool {
	return doltargv.IsSQLServer(args)
}
