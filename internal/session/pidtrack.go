package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/procid"
	"github.com/steveyegge/gastown/internal/tmux"
)

// pidTracker holds the process operations PID tracking depends on, so tests
// can script process start tokens instead of probing real PIDs. A tracked
// process is a procid.ID: the pid alone is reused once the process dies, so
// a record is acted on only while the pid's start token still matches.
type pidTracker struct {
	token     func(pid int) (string, bool)
	terminate func(pid int) error
}

// osPIDTracker probes and signals real processes.
var osPIDTracker = pidTracker{
	token:     procid.StartToken,
	terminate: terminateProcess,
}

// pidsDir returns the directory for PID tracking files.
// All PID files live under <townRoot>/.runtime/pids/ since tmux session
// names are globally unique (they include the rig name).
func pidsDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "pids")
}

// pidFile returns the path to a PID file for a given session.
func pidFile(townRoot, sessionID string) string {
	return filepath.Join(pidsDir(townRoot), sessionID+".pid")
}

// TrackSessionPID captures the pane PID of a tmux session and writes it
// to a PID tracking file. This is defense-in-depth: if a session dies
// unexpectedly and KillSessionWithProcesses can't find the tmux pane,
// we still have the PID on disk for cleanup.
//
// This is best-effort — errors are returned but callers should treat them
// as non-fatal since the primary kill mechanism (KillSessionWithProcesses)
// doesn't depend on PID files.
func TrackSessionPID(townRoot, sessionID string, t *tmux.Tmux) error {
	pidStr, err := t.GetPanePID(sessionID)
	if err != nil {
		return fmt.Errorf("getting pane PID: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
	if err != nil {
		return fmt.Errorf("parsing PID %q: %w", pidStr, err)
	}

	return TrackPID(townRoot, sessionID, pid)
}

// TrackPID writes a PID to a tracking file for later cleanup.
func TrackPID(townRoot, sessionID string, pid int) error {
	return osPIDTracker.track(townRoot, sessionID, pid)
}

func (pt pidTracker) track(townRoot, sessionID string, pid int) error {
	// A record without a start token could never be verified, so it could
	// never be acted on: write none (deep review G1-21).
	token, ok := pt.token(pid)
	if !ok || token == "" {
		return fmt.Errorf("reading start time of PID %d: process gone or unreadable", pid)
	}

	dir := pidsDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating pids directory: %w", err)
	}

	record := procid.ID{PID: pid, Start: token}.String()
	return os.WriteFile(pidFile(townRoot, sessionID), []byte(record+"\n"), 0644)
}

// UntrackPID removes the PID tracking file for a session.
func UntrackPID(townRoot, sessionID string) {
	_ = os.Remove(pidFile(townRoot, sessionID))
}

// PruneDeadTrackedPIDs removes the PID files under <townRoot>/.runtime/pids
// whose process is gone, and leaves every file naming a live one. A session
// that dies without UntrackPID leaves its file behind, and only KillTrackedPIDs
// ever reclaimed one — that runs during `gt down`, so on a live town they
// accumulate. Nothing here signals a process: a sweep may not kill a session
// it merely failed to recognize (unlike KillTrackedPIDs, which is the
// shutdown path and owns the kill).
func PruneDeadTrackedPIDs(townRoot string) (procid.PruneReport, error) {
	return procid.PruneDeadRecords(pidsDir(townRoot))
}

// KillTrackedPIDs reads all PID files and kills any processes that are
// still running. Returns the number of processes killed and any session
// names that had errors.
//
// This is designed for the shutdown orphan-cleanup phase: after all
// sessions have been killed through normal means, this catches any
// processes that survived (e.g., reparented to init after SIGHUP).
func KillTrackedPIDs(townRoot string) (killed int, errSessions []string) {
	return osPIDTracker.killTracked(townRoot)
}

func (pt pidTracker) killTracked(townRoot string) (killed int, errSessions []string) {
	dir := pidsDir(townRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, []string{fmt.Sprintf("read pids dir: %v", err)}
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}

		sessionID := strings.TrimSuffix(entry.Name(), ".pid")
		path := filepath.Join(dir, entry.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			errSessions = append(errSessions, fmt.Sprintf("%s: read error: %v", sessionID, err))
			continue
		}

		record, err := procid.Parse(string(data))
		if err != nil {
			// Corrupt PID file — remove it
			_ = os.Remove(path)
			continue
		}
		pid := record.PID

		if _, live := pt.token(pid); !live {
			// Process is already dead — clean up PID file
			_ = os.Remove(path)
			continue
		}

		if record.Start == "" {
			// A bare PID from an older gt: something holds the number, but
			// nothing says it is the process that was tracked. Refuse to
			// signal it (deep review G1-21).
			_ = os.Remove(path)
			errSessions = append(errSessions, fmt.Sprintf("%s (PID %d): no start time recorded — not signaling an unverified process", sessionID, pid))
			continue
		}
		if !record.Running(pt.token) {
			// Confirmed PID reuse — safe to remove tracking file.
			_ = os.Remove(path)
			continue
		}

		// Process is alive — kill it
		if err := pt.terminate(pid); err != nil {
			errSessions = append(errSessions, fmt.Sprintf("%s (PID %d): SIGTERM failed: %v", sessionID, pid, err))
		} else {
			killed++
		}

		// Clean up PID file regardless
		_ = os.Remove(path)
	}

	return killed, errSessions
}

// terminateProcess sends SIGTERM to pid.
func terminateProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}
