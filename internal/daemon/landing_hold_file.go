package daemon

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// landingHoldFileName is the overseer's landing hold: a file at the town root
// whose existence starts no new landing pass. Polecats keep working and
// submitting, and the pass in flight finishes (gt-ckc93z).
const landingHoldFileName = "landing.hold"

// landingHoldFileHold is the name the file takes on landingHolds. It is apart
// from the jobs' names, so releasing it frees only what the file took.
const landingHoldFileHold = "landing-hold-file"

// landingHoldReasonMax bounds the reason the status line and the dashboard
// carry, in runes. The file's first line says who paused landings and why.
const landingHoldReasonMax = 120

// landingHoldReadMax bounds what the step reads off the file. The reason is the
// first line, so a file someone pasted a log into is read only to 4 KiB.
const landingHoldReadMax = 4 << 10

// landingHoldFileState is what the heartbeat last read off the file. Only the
// heartbeat touches it.
type landingHoldFileState struct {
	// taken is whether the file's hold is on landingHolds.
	taken bool
	// reason is the pause's reason while the hold is taken.
	reason string
	// waiting is set when the hold went on with a landing pass in flight; the
	// step logs once that pass has ended.
	waiting bool
}

// checkLandingHoldFile is the heartbeat step that keeps the landing hold the
// file asks for (gt-ckc93z). It stops no work, only new landing passes, so it
// is not lifecycle: an e-stopped town still reads it and reports the pause.
func (d *Daemon) checkLandingHoldFile() {
	open := d.landingHoldOpen
	if open == nil {
		open = openLandingHoldFile
	}
	present, reason, err := readLandingHoldFile(d.config.TownRoot, open)
	switch {
	case err != nil:
		// An unreadable file holds: the pause the overseer wrote cannot be
		// ruled out, so landings wait for the file to be readable again.
		reason = fmt.Sprintf("%s unreadable (%v); treating as paused", landingHoldFileName, err)
	case present && reason == "":
		reason = landingHoldFileName + " names no reason"
	}
	if !present {
		// Only a file that is absent releases the hold: an error reads as present.
		d.releaseLandingHoldFile()
		return
	}
	d.takeLandingHoldFile(reason)
}

// landingHoldOpener opens the hold file for reading. The daemon's is
// openLandingHoldFile unless a test sets Daemon.landingHoldOpen.
type landingHoldOpener func(path string) (io.ReadCloser, error)

// openLandingHoldFile is the production opener: os.Open, as a reader.
func openLandingHoldFile(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// readLandingHoldFile reads the hold file. present is false when the file does
// not exist. reason is the first line, cleaned for one terminal line. The file
// is stat'ed before it is opened, so a FIFO or a directory is refused without
// blocking on an open. Any other failure to stat, open or read leaves the
// file's answer unknown: it exists, unread, and holds.
func readLandingHoldFile(townRoot string, open landingHoldOpener) (present bool, reason string, err error) {
	path := filepath.Join(townRoot, landingHoldFileName)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return true, "", err
	}
	if !info.Mode().IsRegular() {
		return true, "", errors.New("not a regular file")
	}
	f, err := open(path)
	if err != nil {
		return true, "", err
	}
	// Read-only: a close error loses nothing that was read.
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, landingHoldReadMax))
	if err != nil {
		return true, "", err
	}
	first, _, _ := strings.Cut(string(data), "\n")
	return true, cleanHoldReason(first), nil
}

// cleanHoldReason makes a reason safe for one terminal line: control characters,
// an escape sequence's among them, become spaces, blank runs collapse, and the
// result is cut to landingHoldReasonMax runes.
func cleanHoldReason(line string) string {
	spaced := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, line)
	clean := strings.Join(strings.Fields(spaced), " ")
	if utf8.RuneCountInString(clean) > landingHoldReasonMax {
		clean = string([]rune(clean)[:landingHoldReasonMax])
	}
	return clean
}

// takeLandingHoldFile takes the file's hold, or keeps it taken and notes what
// changed: the reason, and the pass in flight ending.
func (d *Daemon) takeLandingHoldFile(reason string) {
	reason = cleanHoldReason(reason)
	st := &d.landingHoldFile
	if !st.taken {
		// The hold goes on before the in-flight count is read, so a pass that
		// ends in between is not missed (landing_hold.go).
		d.landingHolds.hold(landingHoldFileHold)
		st.taken, st.reason = true, reason
		d.logger.Printf("landing.hold present: new landing passes paused (%s); polecats keep working", reason)
		st.waiting = d.landingPasses.Load() > 0
		if st.waiting {
			d.logger.Println("landing.hold: waiting for the landing pass in flight to end")
		}
		return
	}
	if reason != st.reason {
		st.reason = reason
		d.logger.Printf("landing.hold reason changed: %s", reason)
	}
	if st.waiting && d.landingPasses.Load() == 0 {
		st.waiting = false
		d.logger.Println("landing.hold: the landing pass in flight has ended; no new pass starts while the file stands")
	}
}

// releaseLandingHoldFile drops the file's hold once the file is gone. It logs
// only when a hold was taken, so a town with no file logs nothing.
func (d *Daemon) releaseLandingHoldFile() {
	st := &d.landingHoldFile
	if !st.taken {
		return
	}
	d.landingHolds.release(landingHoldFileHold)
	d.logger.Println("landing.hold removed: new landing passes resume")
	*st = landingHoldFileState{}
}

// landingPausedReason is the reason the health report carries: the file's
// reason while its hold stands, and "" when no pause is on or there is no daemon
// to ask (a health source built without one, as LandingInfra allows).
func (d *Daemon) landingPausedReason() string {
	if d == nil || !d.landingHoldFile.taken {
		return ""
	}
	return d.landingHoldFile.reason
}
