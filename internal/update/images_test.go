package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

const (
	imgOldCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imgNewCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	imgWait      = 10 * time.Second
	imgPoll      = 5 * time.Second
)

// imgRef is a registry digest reference for image at digest byte d.
func imgRef(image, d string) string {
	return "registry.internal:5000/gastown/" + image + "@sha256:" + strings.Repeat(d, 64)
}

// oldPair and newPair are the two pairs the install tests move between.
var (
	oldPair = ImagePair{Daemon: imgRef("gastown-daemon", "1"), Polecat: imgRef("gastown-polecat", "2"), Commit: imgOldCommit}
	newPair = ImagePair{Daemon: imgRef("gastown-daemon", "3"), Polecat: imgRef("gastown-polecat", "4"), Commit: imgNewCommit}
)

// fakeImageLedger keeps the record in memory and can refuse a read or a write.
type fakeImageLedger struct {
	rec      ImageRecord
	readErr  error
	writeErr error
	writes   []ImageRecord
}

func (l *fakeImageLedger) Read() (ImageRecord, error) {
	if l.readErr != nil {
		return ImageRecord{}, l.readErr
	}
	return l.rec, nil
}

func (l *fakeImageLedger) Write(rec ImageRecord) error {
	if l.writeErr != nil {
		return l.writeErr
	}
	l.rec = rec
	l.writes = append(l.writes, rec)
	return nil
}

// fakeImageSwap is the daemon's side of a swap and the probe that reads it. A
// pair whose daemon ref is in stuck is applied, but the daemon comes up
// reporting no build; a ref in fail refuses the swap outright.
type fakeImageSwap struct {
	ledger  *fakeImageLedger
	running string
	applied []ImagePair
	// seenAtApply is the ledger's record at each Apply, in order.
	seenAtApply []ImageRecord
	stuck       map[string]bool
	fail        map[string]error
}

func (s *fakeImageSwap) Apply(_ context.Context, pair ImagePair) error {
	s.applied = append(s.applied, pair)
	s.seenAtApply = append(s.seenAtApply, s.ledger.rec)
	if err := s.fail[pair.Daemon]; err != nil {
		return err
	}
	if s.stuck[pair.Daemon] {
		s.running = ""
		return nil
	}
	s.running = pair.Commit
	return nil
}

// DaemonVersion is the probe the wait reads: the commit the daemon reports.
func (s *fakeImageSwap) DaemonVersion(context.Context) (string, error) {
	return s.running, nil
}

func (s *fakeImageSwap) GTVersion(context.Context) (string, error)  { return "", nil }
func (s *fakeImageSwap) ImageLabel(context.Context) (string, error) { return "", nil }
func (s *fakeImageSwap) Health(context.Context) (string, error)     { return "", nil }

// fakeHostInstaller stands in for the host install every cycle runs.
type fakeHostInstaller struct {
	calls  int
	result InstallResult
	err    error
}

func (f *fakeHostInstaller) Install(_ context.Context, sha string) (InstallResult, error) {
	f.calls++
	res := f.result
	if res.Event == "" {
		res = InstallResult{Event: "installed", Commit: sha, Prev: "-"}
	}
	return res, f.err
}

// imageFixture is one digest install wired to fakes: a ledger that holds oldPair
// as running, a daemon that comes up on whatever it is given, and a values
// file for newPair.
type imageFixture struct {
	inst   *DigestInstaller
	host   *fakeHostInstaller
	ledger *fakeImageLedger
	swap   *fakeImageSwap
	notify *notifyfake.Recorder
	clock  *fakeDrainClock
	path   string
}

func newImageFixture(t *testing.T, recorded ImageRecord) *imageFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "images.yaml")
	if err := imagevalues.Write(path, imagevalues.Values{Daemon: newPair.Daemon, Polecat: newPair.Polecat, Commit: newPair.Commit}); err != nil {
		t.Fatalf("write the values file: %v", err)
	}
	ledger := &fakeImageLedger{rec: recorded}
	swap := &fakeImageSwap{ledger: ledger, running: recorded.Current.Commit}
	clock := &fakeDrainClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	host := &fakeHostInstaller{}
	notes := notifyfake.New()
	return &imageFixture{
		inst: &DigestInstaller{
			Inner: host,
			Path:  path,
			Images: ImageDeps{
				Ledger: ledger,
				Swap:   swap,
				Verify: swap,
				Clock:  clock,
				Sleep:  clock.sleep,
				Wait:   imgWait,
				Poll:   imgPoll,
				Notify: notes,
			},
		},
		host:   host,
		ledger: ledger,
		swap:   swap,
		notify: notes,
		clock:  clock,
		path:   path,
	}
}

func TestInstallByDigestRecordsThePreviousBeforeTheSwap(t *testing.T) {
	t.Parallel()
	running := ImageRecord{Current: oldPair, Previous: ImagePair{}}

	t.Run("the running pair is recorded as previous before the first swap", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, running)
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultOK {
			t.Fatalf("result = %+v, want a successful install", res)
		}
		if len(f.swap.applied) != 1 || f.swap.applied[0] != newPair {
			t.Fatalf("applied = %+v, want one swap to %+v", f.swap.applied, newPair)
		}
		want := ImageRecord{Current: newPair, Previous: oldPair}
		if got := f.swap.seenAtApply[0]; got != want {
			t.Errorf("the ledger at the swap = %+v, want %+v: the previous pair must be on record before the service changes", got, want)
		}
	})

	t.Run("a values file for another commit is refused with nothing changed", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, running)
		_, err := f.inst.Install(context.Background(), imgOldCommit)
		if err == nil || !strings.Contains(err.Error(), "nothing changed") {
			t.Fatalf("Install error = %v, want a refusal that names nothing changed", err)
		}
		assertNothingChanged(t, f, running)
	})

	t.Run("a values file that does not read is refused with nothing changed", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, running)
		f.inst.Path = filepath.Join(t.TempDir(), "missing.yaml")
		if _, err := f.inst.Install(context.Background(), newPair.Commit); err == nil {
			t.Fatal("Install of a missing values file returned no error")
		}
		assertNothingChanged(t, f, running)
	})

	t.Run("a record that cannot be written stops the install before the swap", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, running)
		f.ledger.writeErr = errors.New("disk full")
		if _, err := f.inst.Install(context.Background(), newPair.Commit); err == nil {
			t.Fatal("Install with an unwritable record returned no error")
		}
		// The host install runs before the record is written, so only the
		// service and the record are held to nothing changed.
		if len(f.swap.applied) != 0 {
			t.Errorf("the daemon was swapped to %+v, want no swap", f.swap.applied)
		}
		if f.ledger.rec != running {
			t.Errorf("the record changed to %+v, want %+v untouched", f.ledger.rec, running)
		}
	})
}

// assertNothingChanged fails when a refused install reached the host install,
// the swap, or the record.
func assertNothingChanged(t *testing.T, f *imageFixture, record ImageRecord) {
	t.Helper()
	if f.host.calls != 0 {
		t.Errorf("the host install ran %d times, want none", f.host.calls)
	}
	if len(f.swap.applied) != 0 {
		t.Errorf("the daemon was swapped to %+v, want no swap", f.swap.applied)
	}
	if len(f.ledger.writes) != 0 || f.ledger.rec != record {
		t.Errorf("the record changed to %+v (writes %d), want %+v untouched", f.ledger.rec, len(f.ledger.writes), record)
	}
}

func TestInstallByDigestRollsBackAnUnhealthyDaemon(t *testing.T) {
	t.Parallel()

	t.Run("a daemon that never reports the new commit is restored to the recorded previous pair", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		f.swap.stuck = map[string]bool{newPair.Daemon: true}
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultFailed {
			t.Fatalf("result = %+v, want a failed install", res)
		}
		if !strings.Contains(res.Reason, "restored "+oldPair.Daemon) {
			t.Errorf("reason = %q, want the restore named", res.Reason)
		}
		if got := f.swap.applied; len(got) != 2 || got[0] != newPair || got[1] != oldPair {
			t.Errorf("applied = %+v, want the new pair then the restore of the previous pair", got)
		}
		if want := (ImageRecord{Current: oldPair, Previous: oldPair}); f.ledger.rec != want {
			t.Errorf("record after restore = %+v, want %+v", f.ledger.rec, want)
		}
		if n := len(f.notify.Escalations()); n != 0 {
			t.Errorf("a restore that holds raised %d escalations, want none", n)
		}
	})

	t.Run("a swap that errors is restored to the recorded previous pair", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		f.swap.fail = map[string]error{newPair.Daemon: errors.New("compose refused the image")}
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if !strings.Contains(res.Reason, "the daemon image swap failed") || !strings.Contains(res.Reason, "restored") {
			t.Errorf("reason = %q, want the swap failure and the restore", res.Reason)
		}
		if got := f.swap.applied; len(got) != 2 || got[1] != oldPair {
			t.Errorf("applied = %+v, want a restore of the previous pair after the failed swap", got)
		}
	})

	t.Run("a restore that also fails escalates critically and is reported", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		f.swap.stuck = map[string]bool{newPair.Daemon: true, oldPair.Daemon: true}
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultFailed || strings.Contains(res.Reason, "restored") {
			t.Fatalf("result = %+v, want a failed install that does not claim a restore", res)
		}
		escalations := f.notify.Escalations()
		if len(escalations) != 1 {
			t.Fatalf("escalations = %d, want exactly one", len(escalations))
		}
		if got := escalations[0].Escalation; got.Severity != "critical" || got.Fingerprint != imageRestoreFingerprint {
			t.Errorf("escalation = %+v, want a critical restore escalation", got)
		}
		if want := (ImageRecord{Current: newPair, Previous: oldPair}); f.ledger.rec != want {
			t.Errorf("record after a failed restore = %+v, want %+v: the daemon may still run the new pair", f.ledger.rec, want)
		}
	})

	t.Run("a restore that fails with nothing recorded to restore escalates", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{})
		f.swap.stuck = map[string]bool{newPair.Daemon: true}
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if !strings.Contains(res.Reason, "no previous daemon image is recorded") {
			t.Errorf("reason = %q, want the missing previous pair named", res.Reason)
		}
		if n := len(f.notify.Escalations()); n != 1 {
			t.Errorf("escalations = %d, want one", n)
		}
	})

	t.Run("an escalation that does not reach the town is named in the failure", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, ImageRecord{Current: oldPair})
		f.swap.stuck = map[string]bool{newPair.Daemon: true, oldPair.Daemon: true}
		f.notify.Fail(notifyfake.KindEscalate, errors.New("gt escalate exited 1"))
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if !strings.Contains(res.Reason, "the escalation did not reach the town") {
			t.Errorf("reason = %q, want the lost escalation named", res.Reason)
		}
	})
}

// TestInstallByDigestRefusesADamagedRecord holds the restore's input: a
// images.json that holds a pair no values file could hold stops the install
// before the host install, the swap, or any write, and the file is left as it was.
func TestInstallByDigestRefusesADamagedRecord(t *testing.T) {
	t.Parallel()
	badDigest := ImagePair{Daemon: "registry.internal:5000/gastown/gastown-daemon@sha256:zz", Polecat: oldPair.Polecat, Commit: imgOldCommit}
	partial := ImagePair{Daemon: oldPair.Daemon}
	shortCommit := ImagePair{Daemon: oldPair.Daemon, Polecat: oldPair.Polecat, Commit: "abc"}
	tests := []struct {
		name string
		rec  ImageRecord
	}{
		{name: "a previous digest that is not sha256 hex", rec: ImageRecord{V: imageRecordVersion, Current: oldPair, Previous: badDigest}},
		{name: "a previous pair that names only a daemon", rec: ImageRecord{V: imageRecordVersion, Current: oldPair, Previous: partial}},
		{name: "a current commit that is not a full commit", rec: ImageRecord{V: imageRecordVersion, Current: shortCommit}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			data, err := json.Marshal(tt.rec)
			if err != nil {
				t.Fatalf("encode the record: %v", err)
			}
			path := filepath.Join(dir, imageRecordFileName)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatalf("write the record: %v", err)
			}
			f := newImageFixture(t, ImageRecord{})
			f.inst.Images.Ledger = FileImageLedger{Dir: dir}

			_, err = f.inst.Install(context.Background(), newPair.Commit)
			if err == nil || !strings.Contains(err.Error(), "damaged pair; nothing changed") {
				t.Fatalf("Install error = %v, want the damaged record refused with nothing changed", err)
			}
			if f.host.calls != 0 {
				t.Errorf("the host install ran %d times, want none", f.host.calls)
			}
			if len(f.swap.applied) != 0 {
				t.Errorf("the daemon was swapped to %+v, want no swap", f.swap.applied)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the record: %v", err)
			}
			if string(after) != string(data) {
				t.Errorf("the record changed to %s, want it left as it was", after)
			}
		})
	}
}

func TestInstallWithoutImagesIsUnchanged(t *testing.T) {
	t.Parallel()
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	run := &fakeRun{answer: func(call runCall) (string, int, error) {
		if call.name == "make" {
			return "", 0, nil
		}
		return "install-gt: RESULT installed " + sha + " - -\n", 0, nil
	}}
	inst := &ExecInstaller{RepoRoot: t.TempDir(), Run: run.run}
	res, err := inst.Install(context.Background(), sha)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if ClassifyInstall(res) != ResultOK {
		t.Fatalf("result = %+v, want the installed verdict", res)
	}
	if len(run.calls) != 2 {
		t.Fatalf("commands run = %d, want the build and the installer only: %+v", len(run.calls), run.calls)
	}
	if got := run.calls[0]; got.name != "make" || len(got.args) != 1 || got.args[0] != "gt-installer" {
		t.Errorf("first command = %s %v, want make gt-installer", got.name, got.args)
	}
	if got := run.calls[1]; !strings.HasSuffix(got.name, "gt-installer") || strings.Join(got.args, " ") != "--sha "+sha+" --source gt-update" {
		t.Errorf("second command = %s %v, want the installer for the sha", got.name, got.args)
	}
}

func TestInstallByDigestAgainstARecordedPair(t *testing.T) {
	t.Parallel()
	recorded := ImageRecord{Current: newPair, Previous: oldPair}

	t.Run("a pair on record that the daemon already reports is left alone", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, recorded)
		f.swap.running = newPair.Commit
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultOK {
			t.Fatalf("result = %+v, want a successful install", res)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("the daemon was swapped to %+v, want no swap", f.swap.applied)
		}
		if f.ledger.rec != recorded {
			t.Errorf("record = %+v, want %+v untouched", f.ledger.rec, recorded)
		}
	})

	t.Run("a pair on record that the daemon does not report is applied again with the recorded previous kept", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, recorded)
		f.swap.running = oldPair.Commit
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultOK {
			t.Fatalf("result = %+v, want a successful install", res)
		}
		if len(f.swap.applied) != 1 || f.swap.applied[0] != newPair {
			t.Fatalf("applied = %+v, want one swap to the pair", f.swap.applied)
		}
		if got := f.swap.seenAtApply[0]; got != recorded {
			t.Errorf("the ledger at the swap = %+v, want %+v: the previous pair must survive a re-run", got, recorded)
		}
	})

	t.Run("a pair on record that does not come up is restored to the recorded previous, not to itself", func(t *testing.T) {
		t.Parallel()
		f := newImageFixture(t, recorded)
		f.swap.running = oldPair.Commit
		f.swap.stuck = map[string]bool{newPair.Daemon: true}
		res, err := f.inst.Install(context.Background(), newPair.Commit)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if ClassifyInstall(res) != ResultFailed || !strings.Contains(res.Reason, "restored "+oldPair.Daemon) {
			t.Fatalf("result = %+v, want a failed install restored to the previous pair", res)
		}
		if got := f.swap.applied; len(got) != 2 || got[1] != oldPair {
			t.Errorf("applied = %+v, want the restore to the previous pair", got)
		}
	})
}
