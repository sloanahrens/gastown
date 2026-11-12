package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/townhealth"
)

// landingHoldFileDaemon is a daemon whose town holds no landing.hold file yet,
// logging to a line capture the test reads.
func landingHoldFileDaemon(t *testing.T) (*Daemon, func() []string) {
	t.Helper()
	d := upgradeTestDaemon(t)
	logger, lines := lineLogger()
	d.logger = logger
	return d, lines
}

func writeLandingHoldFile(t *testing.T, d *Daemon, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.config.TownRoot, landingHoldFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLandingHoldFileStopsNewPassesWhileItStands: with landing.hold present no
// landing pass starts, on the heartbeat that reads the file or the next one.
func TestLandingHoldFileStopsNewPassesWhileItStands(t *testing.T) {
	t.Parallel()
	d, _ := landingHoldFileDaemon(t)
	writeLandingHoldFile(t, d, "overseer: drain the queue\n")

	d.checkLandingHoldFile()
	if d.beginLandingPass() {
		t.Fatal("a landing pass started while landing.hold was present")
	}
	d.checkLandingHoldFile()
	if d.beginLandingPass() {
		t.Fatal("a landing pass started on the heartbeat after landing.hold was read")
	}
	if got := d.landingPasses.Load(); got != 0 {
		t.Errorf("refused passes left %d in flight, want 0", got)
	}
}

// TestLandingHoldFileLetsThePassInFlightFinish: a pass already in flight when
// the file appears keeps its slot through the heartbeat, is not cancelled, and
// ends normally; the step logs once the pass has ended.
func TestLandingHoldFileLetsThePassInFlightFinish(t *testing.T) {
	t.Parallel()
	d, lines := landingHoldFileDaemon(t)
	if !d.beginLandingPass() {
		t.Fatal("a quiet town refused a landing pass")
	}
	writeLandingHoldFile(t, d, "overseer: drain the queue\n")

	d.checkLandingHoldFile()
	if got := d.landingPasses.Load(); got != 1 {
		t.Fatalf("in-flight count = %d after the hold went on, want the pass still counted (1)", got)
	}
	if !d.landingPassDraining() {
		t.Error("the pass in flight is not told to stop claiming beads under the hold")
	}

	d.landingPassEnded()
	d.checkLandingHoldFile()
	if got := d.landingPasses.Load(); got != 0 {
		t.Errorf("in-flight count = %d after the pass ended, want 0", got)
	}
	want := []string{
		"landing.hold present: new landing passes paused (overseer: drain the queue); polecats keep working",
		"landing.hold: waiting for the landing pass in flight to end",
		"landing.hold: the landing pass in flight has ended; no new pass starts while the file stands",
	}
	if got := lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("logged lines = %q\nwant %q", got, want)
	}
}

// TestLandingHoldFileReleasesOnTheNextHeartbeat: removing the file releases the
// hold on the next heartbeat, and not before.
func TestLandingHoldFileReleasesOnTheNextHeartbeat(t *testing.T) {
	t.Parallel()
	d, _ := landingHoldFileDaemon(t)
	writeLandingHoldFile(t, d, "overseer: drain the queue\n")
	d.checkLandingHoldFile()

	if err := os.Remove(filepath.Join(d.config.TownRoot, landingHoldFileName)); err != nil {
		t.Fatal(err)
	}
	if !d.landingHolds.heldBy(landingHoldFileHold) {
		t.Fatal("the hold dropped before the heartbeat that reads the removal")
	}

	d.checkLandingHoldFile()
	if d.landingHolds.heldBy(landingHoldFileHold) {
		t.Error("the hold is still taken after landing.hold was removed")
	}
	if !d.beginLandingPass() {
		t.Error("a landing pass was refused after landing.hold was removed")
	}
}

// TestLandingHoldFileLogsOnceWhenTakenAndOnceWhenReleased: the step logs when
// the hold is taken and when it is released, not on each heartbeat between.
func TestLandingHoldFileLogsOnceWhenTakenAndOnceWhenReleased(t *testing.T) {
	t.Parallel()
	d, lines := landingHoldFileDaemon(t)
	writeLandingHoldFile(t, d, "overseer: drain\n")
	d.checkLandingHoldFile()
	d.checkLandingHoldFile()
	d.checkLandingHoldFile()
	if err := os.Remove(filepath.Join(d.config.TownRoot, landingHoldFileName)); err != nil {
		t.Fatal(err)
	}
	d.checkLandingHoldFile()
	d.checkLandingHoldFile()

	want := []string{
		"landing.hold present: new landing passes paused (overseer: drain); polecats keep working",
		"landing.hold removed: new landing passes resume",
	}
	if got := lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("logged lines = %q\nwant %q", got, want)
	}
}

// TestLandingHoldFileUnreadableHolds: a landing.hold that exists but cannot be
// read holds, with the reason saying so, because the overseer's answer cannot be
// ruled out. A directory is such a path: it is not a regular file.
func TestLandingHoldFileUnreadableHolds(t *testing.T) {
	t.Parallel()
	d, _ := landingHoldFileDaemon(t)
	if err := os.Mkdir(filepath.Join(d.config.TownRoot, landingHoldFileName), 0o755); err != nil {
		t.Fatal(err)
	}

	d.checkLandingHoldFile()
	if !d.landingHolds.heldBy(landingHoldFileHold) {
		t.Fatal("an unreadable landing.hold did not hold the landing passes")
	}
	if d.beginLandingPass() {
		t.Error("a landing pass started under an unreadable landing.hold")
	}
	if got := d.landingPausedReason(); !strings.Contains(got, "unreadable") || !strings.Contains(got, "treating as paused") {
		t.Errorf("paused reason = %q, want it to say the file is unreadable and treated as paused", got)
	}
}

// TestLandingHoldFileStatFailureHolds: a failure to stat the path that is not
// "does not exist" holds too. A town root that is a regular file makes every
// path under it fail that way, and a failed stat must not read as an absent file
// and release the hold.
func TestLandingHoldFileStatFailureHolds(t *testing.T) {
	t.Parallel()
	notADir := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: notADir}, logger: log.New(io.Discard, "", 0)}

	d.checkLandingHoldFile()
	if !d.landingHolds.heldBy(landingHoldFileHold) {
		t.Fatal("a landing.hold path that cannot be opened released the landing passes")
	}
	if got := d.landingPausedReason(); !strings.Contains(got, "unreadable") {
		t.Errorf("paused reason = %q, want it to say the file is unreadable", got)
	}
}

// TestOpenLandingHoldFileRefusesAMissingFile: the production opener hands back
// the open's error, not a reader, for a path that is not there.
func TestOpenLandingHoldFileRefusesAMissingFile(t *testing.T) {
	t.Parallel()
	f, err := openLandingHoldFile(filepath.Join(t.TempDir(), landingHoldFileName))
	if err == nil {
		_ = f.Close()
		t.Fatal("the opener returned a reader for a file that does not exist")
	}
}

// failingReader is a reader whose every read fails, as a disk going away does.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// TestLandingHoldFileOpenAndReadFailuresHold: a file that is there and regular,
// but cannot be opened or read, holds with the reason saying it is unreadable.
// The opener is the seam a test forces each failure through.
func TestLandingHoldFileOpenAndReadFailuresHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		open landingHoldOpener
	}{
		{name: "the open fails", open: func(string) (io.ReadCloser, error) { return nil, errors.New("permission denied") }},
		{name: "the read fails", open: func(string) (io.ReadCloser, error) { return io.NopCloser(failingReader{}), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := landingHoldFileDaemon(t)
			writeLandingHoldFile(t, d, "overseer: drain the queue\n")
			d.landingHoldOpen = tc.open

			d.checkLandingHoldFile()
			if !d.landingHolds.heldBy(landingHoldFileHold) {
				t.Fatal("an unreadable landing.hold released the landing passes")
			}
			if got := d.landingPausedReason(); !strings.Contains(got, "unreadable") {
				t.Errorf("paused reason = %q, want it to say the file is unreadable", got)
			}
		})
	}
}

// TestLandingHoldFileReasonIsCleaned: the reason reaches the terminal and the
// dashboard, so control characters and blank runs are cleaned and the reason is
// cut to landingHoldReasonMax runes. A file with no first line names no reason.
func TestLandingHoldFileReasonIsCleaned(t *testing.T) {
	t.Parallel()
	d, _ := landingHoldFileDaemon(t)
	writeLandingHoldFile(t, d, "overseer:\x1b[31m  drain\tthe\x07queue\nsecond line ignored\n")
	d.checkLandingHoldFile()
	if got, want := d.landingPausedReason(), "overseer: [31m drain the queue"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}

	writeLandingHoldFile(t, d, strings.Repeat("é", landingHoldReasonMax+50)+"\n")
	d.checkLandingHoldFile()
	if got := len([]rune(d.landingPausedReason())); got != landingHoldReasonMax {
		t.Errorf("reason has %d runes, want %d", got, landingHoldReasonMax)
	}

	writeLandingHoldFile(t, d, "\n")
	d.checkLandingHoldFile()
	if got, want := d.landingPausedReason(), landingHoldFileName+" names no reason"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

// TestLandingHoldFileReportCarriesThePause: the health report's inputs carry
// the pause while the hold stands and nothing once it is released, so gt status
// --line and the dashboard see the same state the landing workers do.
func TestLandingHoldFileReportCarriesThePause(t *testing.T) {
	t.Parallel()
	d, _ := landingHoldFileDaemon(t)
	src := &healthSources{d: d}

	if got := src.inputs(d.clk().Now(), townhealth.DefaultThresholds(), nil).LandingPaused; got != "" {
		t.Errorf("report carries pause %q with no landing.hold file", got)
	}
	writeLandingHoldFile(t, d, "overseer: drain\n")
	d.checkLandingHoldFile()
	if got := src.inputs(d.clk().Now(), townhealth.DefaultThresholds(), nil).LandingPaused; got != "overseer: drain" {
		t.Errorf("report pause = %q, want %q", got, "overseer: drain")
	}
	if err := os.Remove(filepath.Join(d.config.TownRoot, landingHoldFileName)); err != nil {
		t.Fatal(err)
	}
	d.checkLandingHoldFile()
	if got := src.inputs(d.clk().Now(), townhealth.DefaultThresholds(), nil).LandingPaused; got != "" {
		t.Errorf("report pause = %q after the file was removed, want none", got)
	}
}
