package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/ui"
)

// The command usage log is one JSONL file per local day in gtDataDir():
// cmd-usage-2006-01-02.jsonl. The first write of a day prunes dated files
// older than usageRetentionDays. cmd-usage.jsonl is the unrotated file
// written before daily rotation; gt metrics still reads it.
const (
	usageLogPrefix     = "cmd-usage"
	usageLegacyFile    = usageLogPrefix + ".jsonl"
	usageDateLayout    = "2006-01-02"
	usageRetentionDays = 30
)

// noLogCommands are top-level commands excluded from telemetry.
// These fire per-tool-use and would dominate the log.
var noLogCommands = map[string]bool{
	"tap":    true,
	"signal": true,
}

// logCommandUsage appends one JSONL line to today's usage log.
// Fire-and-forget: all errors are silently ignored.
func logCommandUsage(cmd *cobra.Command, args []string) {
	if usageLogSuppressed(testing.Testing(), os.Getenv) {
		return
	}

	// Walk up to the first subcommand under root to check exclusions.
	root := cmd
	for root.Parent() != nil && root.Parent().Parent() != nil {
		root = root.Parent()
	}
	if noLogCommands[root.Name()] {
		return
	}

	actor := commandActor(cmd, os.Getenv, ui.IsTerminal())
	_ = appendUsage(gtDataDir(), time.Now(), buildCommandPath(cmd), actor, len(args))
}

// commandActor is the usage-log actor for cmd. "gt daemon run" is the
// daemon whatever environment launched it: runDaemonRun clears inherited
// identity and sets BD_ACTOR=daemon, but only after this entry is written,
// and the launchd job sets no identity of its own (gt-dswsc).
func commandActor(cmd *cobra.Command, getenv func(string) string, interactive bool) string {
	if cmd == daemonRunCmd {
		return daemonRunActor
	}
	return usageActor(getenv, interactive)
}

// usageLogSuppressed reports whether this process must not write the usage
// log. In-process tests (a test binary that Executes rootCmd) and gt
// subprocesses of the hermetic harness (GT_TEST_HERMETIC=1) never write it,
// so the operator's log holds production calls only (G4-14).
func usageLogSuppressed(underTest bool, getenv func(string) string) bool {
	return underTest || getenv("GT_TEST_HERMETIC") == "1"
}

// usageActor names who ran the command, reading only identity variables from
// config.IdentityEnvVars. A qualified GT_ROLE ("gastown/witness") wins, then
// BD_ACTOR (the daemon sets BD_ACTOR=daemon for everything it spawns), then a
// bare GT_ROLE qualified with GT_RIG and GT_POLECAT/GT_CREW when present. A
// process with no identity is the operator when it runs at a terminal and
// "unknown" otherwise.
func usageActor(getenv func(string) string, interactive bool) string {
	role := getenv("GT_ROLE")
	if strings.Contains(role, "/") {
		return role
	}
	if actor := getenv("BD_ACTOR"); actor != "" {
		return actor
	}
	if role != "" {
		rig := getenv("GT_RIG")
		if name := getenv("GT_POLECAT"); rig != "" && name != "" {
			return rig + "/polecats/" + name
		}
		if name := getenv("GT_CREW"); rig != "" && name != "" {
			return rig + "/crew/" + name
		}
		return role
	}
	if interactive {
		return "operator"
	}
	return "unknown"
}

// usageLogPath is the dated usage log file for the local day of now.
func usageLogPath(dir string, now time.Time) string {
	return filepath.Join(dir, usageLogPrefix+"-"+now.Format(usageDateLayout)+".jsonl")
}

// appendUsage appends one entry to the usage log for now's day. The process
// that creates the day's file prunes the dated files past retention.
func appendUsage(dir string, now time.Time, cmdPath, actor string, argc int) error {
	path := usageLogPath(dir, now)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			pruneUsageLogs(dir, now)
		} else if errors.Is(err, fs.ErrExist) {
			f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		}
	}
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = fmt.Fprintf(f, `{"ts":"%s","cmd":"%s","actor":"%s","argc":%d}`+"\n",
		now.Format(time.RFC3339), cmdPath, actor, argc)
	return err
}

// pruneUsageLogs removes dated usage logs more than usageRetentionDays days
// before now's day. The legacy unrotated file is left for the operator.
func pruneUsageLogs(dir string, now time.Time) {
	cutoff := now.AddDate(0, 0, -usageRetentionDays).Format(usageDateLayout)
	for _, f := range usageLogFiles(dir) {
		if day, ok := usageLogDay(f); ok && day < cutoff {
			_ = os.Remove(f)
		}
	}
}

// usageLogFiles lists the usage logs in dir, the legacy file first and then
// the dated files oldest first.
func usageLogFiles(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, usageLogPrefix+"-*.jsonl"))
	var dated []string
	for _, m := range matches {
		if _, ok := usageLogDay(m); ok {
			dated = append(dated, m)
		}
	}
	sort.Strings(dated)
	legacy := filepath.Join(dir, usageLegacyFile)
	if _, err := os.Stat(legacy); err == nil {
		return append([]string{legacy}, dated...)
	}
	return dated
}

// usageLogDay returns the day a dated usage log file covers.
func usageLogDay(path string) (string, bool) {
	name := filepath.Base(path)
	day := strings.TrimSuffix(strings.TrimPrefix(name, usageLogPrefix+"-"), ".jsonl")
	if _, err := time.Parse(usageDateLayout, day); err != nil {
		return "", false
	}
	return day, true
}
