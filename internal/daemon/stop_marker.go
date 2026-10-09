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

// consumeStopRequested reports whether a valid stop request for pid exists,
// and removes the marker either way: it is single use. A marker for another
// pid, one older than stopMarkerTTL, or one that cannot be parsed is not a
// request.
func consumeStopRequested(townRoot string, pid int, now time.Time) bool {
	path := stopMarkerPath(townRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
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

// removeStopRequested drops the marker after a stop that could not be sent.
func removeStopRequested(townRoot string) {
	_ = os.Remove(stopMarkerPath(townRoot))
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
