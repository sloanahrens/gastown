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
// partway leaves the pair to restore on record. For a daemon that runs as a
// pod, Previous is the pair its Deployment ran, read from the cluster, and names
// no commit: a Deployment carries the two images and no build.
type ImageRecord struct {
	V        int       `json:"v"`
	Current  ImagePair `json:"current"`
	Previous ImagePair `json:"previous"`
}

// The checks a digest install runs after the swap, named in a RollbackRecord.
// The first two are the rollout wait: the swap must apply and the daemon must
// report the new commit. The last two are the pair check's digest and tag reads.
const (
	CheckRollout        = "rollout"
	CheckVersion        = "version"
	CheckRunningDigests = "running-digests"
	CheckRegistryTag    = "registry-tag"
	// CheckPair is a pair check failure that names no check of its own, such as
	// the values file no longer holding the pair. It is never read as a rollout.
	CheckPair = "pair"
)

// RollbackRecord is what a digest install that put the previous pair back on a
// failed check records: the check that failed and the pair restored. A rollback
// whose restore failed is not recorded here; its escalation is the record.
type RollbackRecord struct {
	Check    string    `json:"check"`
	Restored ImagePair `json:"restored"`
}

// PairCheckError is a pair check's failure with the check that failed, so the
// rollback can name it. Its message is the check's own, unchanged.
type PairCheckError struct {
	Check string
	Err   error
}

func (e PairCheckError) Error() string { return e.Err.Error() }

func (e PairCheckError) Unwrap() error { return e.Err }

// pairCheckName is the check a pair check's failure names, or CheckPair when the
// failure names none.
func pairCheckName(err error) string {
	var pc PairCheckError
	if errors.As(err, &pc) && pc.Check != "" {
		return pc.Check
	}
	return CheckPair
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

// ImageRunningPair reads the pair a daemon runs now, so an install can put it
// back when the pair it applies fails. The pair may name no commit: a value
// read from a Deployment, rather than from a values file, names none.
type ImageRunningPair interface {
	RunningPair(ctx context.Context) (ImagePair, error)
}

// RecordedRunningPair is the pair a daemon with no Deployment to ask runs: the
// current pair its record holds. An empty record names nothing recorded.
type RecordedRunningPair struct {
	Ledger ImageLedger
}

// RunningPair is the current pair the record holds.
func (r RecordedRunningPair) RunningPair(context.Context) (ImagePair, error) {
	rec, err := r.Ledger.Read()
	if err != nil {
		return ImagePair{}, fmt.Errorf("reading the daemon image record: %w", err)
	}
	return rec.Current, nil
}

// ImageDeps are the effects a digest install and a rollback reach.
type ImageDeps struct {
	// Ledger holds the recorded pairs.
	Ledger ImageLedger
	// Running reads the pair the daemon runs now: the pod's Deployment when the
	// daemon runs as a pod, else RecordedRunningPair.
	Running ImageRunningPair
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
// pair.Commit within the bound. A pair read from a Deployment names no commit,
// so there is no build to wait for, and the swap's own return is not evidence:
// that pair is confirmed only when the running pair, read after the swap, names
// its images. A daemon with no Deployment reads the record, which the swap does
// not change until the install or rollback writes it, so a commit-less pair is
// never confirmed there. A commit-less pair is refused before the swap when no
// read of the running pair is wired. A canceled context is an error.
func (d ImageDeps) swapTo(ctx context.Context, pair ImagePair) (bool, error) {
	if pair.Commit == "" && d.Running == nil {
		return false, errors.New("no read of the pair the daemon runs is wired, so a pair with no commit cannot be confirmed")
	}
	if err := d.Swap.Apply(ctx, pair); err != nil {
		return false, err
	}
	if pair.Commit == "" {
		return d.imagesRunning(ctx, pair)
	}
	return WaitForRestart(ctx, d.Clock, d.Sleep, d.Verify, pair.Commit, d.Wait, d.Poll)
}

// imagesRunning reports whether the daemon's Deployment runs pair's images. The
// read is the Deployment's own, so a swap the cluster did not take is not
// reported as running. A failed read is an error, never a pair that held.
func (d ImageDeps) imagesRunning(ctx context.Context, pair ImagePair) (bool, error) {
	running, err := d.Running.RunningPair(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the pair the daemon runs after the swap: %w", err)
	}
	return sameImages(running, pair), nil
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
		if prev.Commit == "" {
			return d.escalateRestore(ctx, fmt.Sprintf("restoring %s: the running pair does not name the restored images, and a pair with no commit has no build to check", prev.Daemon))
		}
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
// pair the daemon runs now is read from the daemon itself and recorded before
// the swap, so a daemon that does not come up on the new pair is restored to
// it: the values file cannot say what ran before, since the update Job's init
// container rewrites it with the new pair (gt-q8e9dv).
type DigestInstaller struct {
	// Inner is the host install the cycle runs with or without --images.
	Inner Installer
	// Path is the values file gt update images wrote.
	Path string
	// Images are the effects the swap, the wait and a restore reach.
	Images ImageDeps
	// PairCheck, when set, runs once the daemon reports the new pair. An error
	// fails the install and restores the previous pair, as an unhealthy daemon
	// does. Nil checks nothing.
	PairCheck func(ctx context.Context, pair ImagePair) error
}

// validRecordedPair checks a pair the ledger holds. The zero pair is nothing
// recorded and passes; any other pair must name both images by digest, and its
// commit, when given, must be a full commit. A pair read from a Deployment names
// no commit, and is recorded that way.
func validRecordedPair(p ImagePair) error {
	if p == (ImagePair{}) {
		return nil
	}
	for _, field := range []struct{ name, value string }{
		{"daemon", p.Daemon},
		{"polecat", p.Polecat},
	} {
		if !imagevalues.ValidRef(field.value) {
			return fmt.Errorf("the %s image %q is not repo@sha256:<64 lowercase hex>", field.name, field.value)
		}
	}
	if p.Commit != "" && !imagevalues.ValidCommit(p.Commit) {
		return fmt.Errorf("the commit %q is not a full 40-character lowercase commit", p.Commit)
	}
	return nil
}

// sameImages reports whether two pairs name the same daemon and polecat images.
// The commit is not compared: a pair read from a Deployment names none.
func sameImages(a, b ImagePair) bool {
	return a.Daemon == b.Daemon && a.Polecat == b.Polecat
}

// pairHolds reports whether the pair check, when one is wired, holds pair. An
// unwired check holds nothing back.
func (d *DigestInstaller) pairHolds(ctx context.Context, pair ImagePair) bool {
	return d.PairCheck == nil || d.PairCheck(ctx, pair) == nil
}

// Install validates the values file and its commit, reads the pair the daemon
// runs now, runs the host install, and moves the daemon to the pair.
func (d *DigestInstaller) Install(ctx context.Context, sha string) (InstallResult, error) {
	if err := d.Images.wired(); err != nil {
		return InstallResult{}, err
	}
	if d.Images.Running == nil {
		return InstallResult{}, errors.New("no read of the pair the daemon runs is wired")
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
	// Both recorded pairs are checked before the host install or any write: a
	// restore puts the previous pair back, so a damaged one stops the run here.
	for _, p := range []ImagePair{rec.Current, rec.Previous} {
		if err := validRecordedPair(p); err != nil {
			return InstallResult{}, fmt.Errorf("the daemon image record names a damaged pair; nothing changed: %w", err)
		}
	}
	// The pair the daemon runs now is read from the daemon itself, before
	// anything changes: the update Job's init container rewrites the values file
	// with the new pair, so the file cannot say what ran before it.
	running, err := d.Images.Running.RunningPair(ctx)
	if err != nil {
		return InstallResult{}, fmt.Errorf("reading the pair the daemon runs now, so a failed install could not be put back; nothing changed: %w", err)
	}

	res, err := d.Inner.Install(ctx, sha)
	if err != nil || ClassifyInstall(res) != ResultOK {
		return res, err
	}
	previous := running
	if sameImages(running, target) {
		// The daemon already runs the pair. It is left alone when it reports the
		// commit and the pair check, when wired, holds the pair; otherwise a run
		// that stopped between the record and the swap left the service behind,
		// and the swap runs again with the previous pair that record kept.
		if daemon, err := d.Images.Verify.DaemonVersion(ctx); err == nil && sameCommit(daemon, sha) && d.pairHolds(ctx, target) {
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
	check := CheckVersion
	reason := fmt.Sprintf("the daemon did not report %s within the restart bound", shortSHA(sha))
	switch {
	case err != nil:
		check, reason = CheckRollout, "the daemon image swap failed: "+err.Error()
	case healthy:
		if d.PairCheck == nil {
			return res, nil
		}
		checkErr := d.PairCheck(ctx, target)
		if ctx.Err() != nil {
			return InstallResult{}, ctx.Err()
		}
		if checkErr == nil {
			return res, nil
		}
		check, reason = pairCheckName(checkErr), "the daemon did not run the pair: "+checkErr.Error()
	}
	res.Event = "failed"
	res.Exit = 1
	if rerr := d.Images.restore(ctx, previous); rerr != nil {
		res.Reason = reason + "; " + rerr.Error()
		return res, nil
	}
	res.Reason = reason + "; restored " + previous.Daemon
	res.Rollback = &RollbackRecord{Check: check, Restored: previous}
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
