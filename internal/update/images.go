package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify"
)

const (
	// imageRecordFileName is the digest install's ledger, beside state.json. It
	// is its own file because every run rewrites state.json from its plan, and
	// the pair a rollback restores must survive the next run.
	imageRecordFileName = "images.json"
	// imageRecordVersion is the only ledger version this package writes and
	// reads.
	imageRecordVersion = 1
	// imageRestoreFingerprint is the escalation's alert key for a daemon that
	// could not be put back on its previous image.
	imageRestoreFingerprint = "update:daemon-image-restore"
)

// ErrNothingRecorded is a rollback with no previous pair in the ledger: no
// digest install has moved the daemon yet, or the last one was rolled back.
var ErrNothingRecorded = errors.New("no daemon image is recorded to roll back to")

// ErrAlreadyRunning is a rollback whose recorded previous pair is the one the
// daemon already runs.
var ErrAlreadyRunning = errors.New("the recorded previous image is the one the daemon runs")

// ImagePair is the daemon and polecat image pair one digest install put in
// force, with the commit both images were built from.
type ImagePair struct {
	Daemon  string `json:"daemon"`
	Polecat string `json:"polecat"`
	Commit  string `json:"commit"`
}

// imagePairOf reads a values file's pair.
func imagePairOf(v imagevalues.Values) ImagePair {
	return ImagePair{Daemon: v.Daemon, Polecat: v.Polecat, Commit: v.Commit}
}

// ImageRecord is the digest install's ledger. Current is the pair the daemon
// runs, or the pair it is being moved to; Previous is the pair a rollback puts
// back. Both are written before the service changes, so a swap that stops
// partway leaves the pair to restore on record.
type ImageRecord struct {
	V        int       `json:"v"`
	Current  ImagePair `json:"current"`
	Previous ImagePair `json:"previous"`
}

// ImageLedger reads and writes the digest install's record.
type ImageLedger interface {
	Read() (ImageRecord, error)
	Write(rec ImageRecord) error
}

// FileImageLedger keeps the record at <Dir>/images.json. A missing file is an
// empty record: no digest install has run in this town. A file that cannot be
// read or parsed is an error, never an empty record.
type FileImageLedger struct {
	// Dir is the update state directory.
	Dir string
	// marshal encodes the record; nil means json.MarshalIndent. A test replaces
	// it, since the record holds only strings and integers and no value of it
	// fails to encode.
	marshal func(v any, prefix, indent string) ([]byte, error)
}

func (l FileImageLedger) path() string {
	return filepath.Join(l.Dir, imageRecordFileName)
}

// Read returns the record on disk.
func (l FileImageLedger) Read() (ImageRecord, error) {
	path := l.path()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ImageRecord{}, nil
	}
	if err != nil {
		return ImageRecord{}, fmt.Errorf("read the daemon image record %s: %w", path, err)
	}
	var rec ImageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return ImageRecord{}, fmt.Errorf("parse the daemon image record %s: %w", path, err)
	}
	if rec.V != imageRecordVersion {
		return ImageRecord{}, fmt.Errorf("daemon image record %s: unsupported version %d, want %d", path, rec.V, imageRecordVersion)
	}
	return rec, nil
}

// Write publishes rec atomically.
func (l FileImageLedger) Write(rec ImageRecord) error {
	rec.V = imageRecordVersion
	marshal := l.marshal
	if marshal == nil {
		marshal = json.MarshalIndent
	}
	data, err := marshal(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the daemon image record: %w", err)
	}
	if err := os.MkdirAll(l.Dir, dirMode); err != nil {
		return fmt.Errorf("create update state directory %s: %w", l.Dir, err)
	}
	if err := atomicfile.WriteFile(l.path(), append(data, '\n'), fileMode); err != nil {
		return fmt.Errorf("write the daemon image record %s: %w", l.path(), err)
	}
	return nil
}

// ImageSwap puts a pair in force: the daemon service runs the pair's daemon
// image, and the polecat image the pair names is the one sandboxes run on. It
// returns once the daemon has been recreated on the pair; the caller waits for
// the daemon to report the build.
type ImageSwap interface {
	Apply(ctx context.Context, pair ImagePair) error
}

// ImageDeps are the effects a digest install and a rollback reach.
type ImageDeps struct {
	// Ledger holds the recorded pairs.
	Ledger ImageLedger
	// Swap puts a pair in force.
	Swap ImageSwap
	// Verify asks the daemon which build it runs; the wait for a swap reads it.
	Verify VerifyProbes
	// Clock and Sleep drive the wait for the daemon, as the restart wait does.
	Clock Clock
	Sleep func(context.Context, time.Duration) error
	// Wait and Poll bound the wait for the daemon to report the new build: the
	// cycle's restart bound.
	Wait, Poll time.Duration
	// Notify raises the escalation for a daemon that could not be restored.
	Notify notify.Notifier
}

// wired refuses to act with an effect unset: a swap that cannot be recorded,
// judged or escalated must not start.
func (d ImageDeps) wired() error {
	switch {
	case d.Ledger == nil:
		return errors.New("no daemon image record is wired")
	case d.Swap == nil:
		return errors.New("no daemon image swap is wired")
	case d.Verify == nil:
		return errors.New("no daemon version probe is wired")
	case d.Clock == nil || d.Sleep == nil:
		return errors.New("no clock is wired for the daemon wait")
	case d.Notify == nil:
		return errors.New("no escalation sink is wired")
	}
	return nil
}

// swapTo puts pair in force and reports whether the daemon came back on
// pair.Commit within the bound. A canceled context is an error.
func (d ImageDeps) swapTo(ctx context.Context, pair ImagePair) (bool, error) {
	if err := d.Swap.Apply(ctx, pair); err != nil {
		return false, err
	}
	return WaitForRestart(ctx, d.Clock, d.Sleep, d.Verify, pair.Commit, d.Wait, d.Poll)
}

// restore puts prev back after a swap that did not hold, and records prev as
// running with nothing to roll back to. A restore that fails escalates and
// returns an error: the daemon is left where it is, and the caller reports it.
func (d ImageDeps) restore(ctx context.Context, prev ImagePair) error {
	if prev == (ImagePair{}) {
		return d.escalateRestore(ctx, "no previous daemon image is recorded, so the daemon cannot be restored")
	}
	healthy, err := d.swapTo(ctx, prev)
	if err != nil {
		return d.escalateRestore(ctx, fmt.Sprintf("restoring %s failed: %v", prev.Daemon, err))
	}
	if !healthy {
		return d.escalateRestore(ctx, fmt.Sprintf("restoring %s did not come back on commit %s within the restart bound", prev.Daemon, prev.Commit))
	}
	if err := d.Ledger.Write(ImageRecord{Current: prev, Previous: prev}); err != nil {
		return fmt.Errorf("the daemon runs %s, but recording that failed: %w", prev.Daemon, err)
	}
	return nil
}

// escalateRestore raises the critical escalation for a failed restore and
// returns the failure. An escalation that does not reach the town is named in
// the error, so the failure is never reported as handled.
func (d ImageDeps) escalateRestore(ctx context.Context, reason string) error {
	err := d.Notify.Escalate(ctx, notify.Escalation{
		Severity:    "critical",
		Description: "gt update: the daemon did not come back on its previous image",
		Reason:      reason,
		Source:      "gt-update",
		Fingerprint: imageRestoreFingerprint,
	})
	if err != nil {
		return fmt.Errorf("%s; the escalation did not reach the town: %w", reason, err)
	}
	return errors.New(reason)
}

// DigestInstaller is the install step of a run given --images. It runs the
// host install the cycle always runs, then moves the daemon to the image pair
// the values file names, and waits for the daemon to report the build. The
// pair the daemon ran before is recorded before the swap, so a daemon that does
// not come up on the new pair is restored to it.
type DigestInstaller struct {
	// Inner is the host install the cycle runs with or without --images.
	Inner Installer
	// Path is the values file gt update images wrote.
	Path string
	// Images are the effects the swap, the wait and a restore reach.
	Images ImageDeps
}

// Install validates the values file and its commit before anything changes, runs
// the host install, and moves the daemon to the pair.
func (d *DigestInstaller) Install(ctx context.Context, sha string) (InstallResult, error) {
	if err := d.Images.wired(); err != nil {
		return InstallResult{}, err
	}
	values, err := imagevalues.Load(d.Path)
	if err != nil {
		return InstallResult{}, fmt.Errorf("the --images values file: %w", err)
	}
	if values.Commit != sha {
		return InstallResult{}, fmt.Errorf("the --images file %s is for commit %s, but this run installs %s; nothing changed", d.Path, values.Commit, sha)
	}
	target := imagePairOf(values)
	rec, err := d.Images.Ledger.Read()
	if err != nil {
		return InstallResult{}, fmt.Errorf("reading the daemon image record: %w", err)
	}

	res, err := d.Inner.Install(ctx, sha)
	if err != nil || ClassifyInstall(res) != ResultOK {
		return res, err
	}
	previous := rec.Current
	if rec.Current == target {
		// The record already names the pair as current. The daemon is left alone
		// when it reports the commit; otherwise a run that stopped between the
		// record and the swap left the service behind, and the swap runs again
		// with the previous pair that record kept.
		if daemon, err := d.Images.Verify.DaemonVersion(ctx); err == nil && sameCommit(daemon, sha) {
			return res, nil
		}
		previous = rec.Previous
	}

	// The previous pair is on record before the first change to the service.
	if err := d.Images.Ledger.Write(ImageRecord{Current: target, Previous: previous}); err != nil {
		return InstallResult{}, fmt.Errorf("recording the previous daemon image before the swap: %w", err)
	}
	healthy, err := d.Images.swapTo(ctx, target)
	if ctx.Err() != nil {
		return InstallResult{}, ctx.Err()
	}
	if err == nil && healthy {
		return res, nil
	}

	reason := fmt.Sprintf("the daemon did not report %s within the restart bound", shortSHA(sha))
	if err != nil {
		reason = "the daemon image swap failed: " + err.Error()
	}
	res.Event = "failed"
	res.Exit = 1
	if rerr := d.Images.restore(ctx, previous); rerr != nil {
		res.Reason = reason + "; " + rerr.Error()
		return res, nil
	}
	res.Reason = reason + "; restored " + previous.Daemon
	return res, nil
}

// Rollback puts back the pair the last digest install recorded as its previous.
// It refuses, changing nothing, when nothing is recorded, when the recorded
// previous is the pair the daemon runs, and when check finds a digest of that
// pair missing from the registry.
func Rollback(ctx context.Context, d ImageDeps, check func(context.Context, ImagePair) error) (ImagePair, error) {
	if err := d.wired(); err != nil {
		return ImagePair{}, err
	}
	if check == nil {
		return ImagePair{}, errors.New("no registry check is wired")
	}
	rec, err := d.Ledger.Read()
	if err != nil {
		return ImagePair{}, fmt.Errorf("reading the daemon image record: %w", err)
	}
	prev := rec.Previous
	if prev == (ImagePair{}) {
		return ImagePair{}, ErrNothingRecorded
	}
	if prev == rec.Current {
		return prev, ErrAlreadyRunning
	}
	if err := check(ctx, prev); err != nil {
		return ImagePair{}, fmt.Errorf("refusing the recorded previous image: %w", err)
	}
	if err := d.restore(ctx, prev); err != nil {
		return ImagePair{}, err
	}
	return prev, nil
}
