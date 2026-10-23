package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrUnrequestedStop is returned by Run after a graceful shutdown caused by a
// signal that gt did not send through StopDaemon (a stray SIGTERM, Docker or
// the OS ending the process). The caller maps it to exit code 75, like
// ErrRestartForUpgrade: launchd's KeepAlive {SuccessfulExit: false} relaunches
// only a nonzero exit, and an exit 0 here left the town down until someone ran
// launchctl kickstart (2026-10-09 10:51, gt-swsqm).
var ErrUnrequestedStop = errors.New("daemon: stopped by a signal gt did not request")

// stopMarkerTTL bounds how long a stop request stays valid. StopDaemon writes
// the marker immediately before it signals, so a marker older than this was
// left by a stop that failed and must not excuse a later unexplained SIGTERM.
const stopMarkerTTL = time.Minute

func stopMarkerPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "stop-requested")
}

// writeStopRequested records that gt is about to stop the daemon with the
// given pid. The daemon, on its signal, exits 0 only if it finds this marker.
func writeStopRequested(townRoot string, pid int, now time.Time) error {
	path := stopMarkerPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	body := fmt.Sprintf("%d\n%d\n", pid, now.Unix())
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stopRequested reports whether a valid stop request for pid exists. It does
// not touch the marker: the signal branch reads it before the shutdown, and
// exitAfterSignal consumes it after. A marker for another pid, one older than
// stopMarkerTTL, or one that cannot be parsed is not a request.
func stopRequested(townRoot string, pid int, now time.Time) bool {
	data, err := os.ReadFile(stopMarkerPath(townRoot))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return false
	}
	markerPID, err1 := strconv.Atoi(fields[0])
	unix, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil || markerPID != pid {
		return false
	}
	age := now.Sub(time.Unix(unix, 0))
	return age >= -stopMarkerTTL && age <= stopMarkerTTL
}

// consumeStopRequested reports whether a valid stop request for pid exists,
// and removes the marker either way: it is single use.
func consumeStopRequested(townRoot string, pid int, now time.Time) bool {
	requested := stopRequested(townRoot, pid, now)
	_ = os.Remove(stopMarkerPath(townRoot))
	return requested
}

// removeStopRequested drops the marker after a stop that could not be sent.
func removeStopRequested(townRoot string) {
	_ = os.Remove(stopMarkerPath(townRoot))
}

// signalStopPath is where a daemon records that a signal ended it, for the
// upgrade heal helper to read.
func signalStopPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "stopped-by-signal")
}

// recordSignalStop leaves a durable trace that a signal ended the daemon. The
// stop marker above is consumed by the exit it excuses, and a supervised
// `gt daemon stop` is a launchctl bootout that writes no marker at all, so
// this record is the only trace a bootout inside the upgrade heal helper's
// wait leaves (gt-r8ux8). Its second line says whether gt requested the stop:
// gt down signals the daemon without a bootout, the daemon exits 0, and
// launchd leaves the job loaded with no pid, which is the shape a respawn that
// failed also has. Only an unrequested stop is for the helper to heal
// (gt-518hq).
func recordSignalStop(townRoot string, now time.Time, requested bool) error {
	path := signalStopPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	kind := signalStopUnrequested
	if requested {
		kind = signalStopRequested
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n%s\n", now.Unix(), kind)), 0o644)
}

// The second line of the signal-stop record. The heal helper matches the
// requested line exactly, so a record from before this line existed reads as
// unrequested.
const (
	signalStopRequested   = "requested"
	signalStopUnrequested = "unrequested"
)

// clearSignalStop drops a signal-stop record left by an earlier stop, so the
// helper armed by an upgrade exit only sees stops that happen after it, and a
// daemon that starts does not leave the record of the daemon before it.
func clearSignalStop(townRoot string) {
	_ = os.Remove(signalStopPath(townRoot))
}

// exitAfterSignal is what Run returns once the graceful shutdown for a signal
// has finished: nil when gt asked for the stop (StopDaemon left a marker for
// this pid), ErrUnrequestedStop otherwise so the process exits nonzero and
// launchd relaunches it. A shutdown error is returned as it is.
func exitAfterSignal(townRoot string, pid int, now time.Time, shutdownErr error) error {
	if shutdownErr != nil {
		return shutdownErr
	}
	if consumeStopRequested(townRoot, pid, now) {
		return nil
	}
	return ErrUnrequestedStop
}
