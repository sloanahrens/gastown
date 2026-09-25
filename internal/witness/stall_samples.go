package witness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/atomicfile"
)

// Persisted stall samples (claude-8w7, step 1).
//
// The gt-xb27 stall rule needs two observations of the same polecat a full
// window apart. The witness used to hold sample 1 in its own context, so a
// respawned witness forgot it and stall detection silently never fired.
// gt patrol scan now keeps sample 1 on disk and compares every later scan
// against it with AssessStall, so the rule survives any number of witness
// session restarts.
//
// The file lives beside the witness's state.json but is written only by Go:
// the agent-maintained state.json has been found invalid and used as a prose
// scratchpad, and a stall baseline must not share that fate.

// stallSamplesFileName is the samples file inside WitnessStateDir.
const stallSamplesFileName = "stall_samples.json"

// stallSamplesVersion is the on-disk schema version.
const stallSamplesVersion = 1

// stallSampleMaxScanGap is the longest gap between two scans of a polecat over
// which sample 1 is still trusted. Beyond it nobody watched the window — the
// host slept, or the witness was down — and a scan on wake would otherwise
// report every quiet polecat stalled at once, so sample 1 is re-recorded.
//
// It is 3x the witness's 5m await-signal backoff cap rather than 2x: a quiet
// witness scans once per cap interval PLUS the cycle's own duration (patrol
// scan alone can take minutes) and, once the session respawns at the cap, its
// boot. A gap bound that ordinary cycles exceed would re-record sample 1 on
// every scan and silently disable stall detection.
const stallSampleMaxScanGap = 15 * time.Minute

// stallSamplesLockTimeout bounds the wait for the samples lock. Var so tests
// can shorten it.
var stallSamplesLockTimeout = 10 * time.Second

// StallSample is one persisted observation: sample 1 of the stall rule for a
// polecat.
type StallSample struct {
	Polecat string `json:"polecat"`
	// Session is the tmux session the sample was taken from. A sample from any
	// other session (a restarted or reused polecat) is never compared.
	Session   string    `json:"session"`
	SampledAt time.Time `json:"sampled_at"`
	// LastObservedAt is the latest scan that saw this polecat, whether or not
	// it kept sample 1. The scan-gap guard (stallSampleMaxScanGap) reads it.
	LastObservedAt time.Time `json:"last_observed_at"`
	// TranscriptPath identifies the transcript the dates below belong to.
	TranscriptPath string `json:"transcript_path"`
	// TranscriptMtime is the transcript's mtime at sampling; zero when the
	// session had no transcript that dated it.
	TranscriptMtime time.Time `json:"transcript_mtime"`
	// TranscriptBytes is the transcript's size; a same-second write can keep
	// the mtime, never the size.
	TranscriptBytes int64  `json:"transcript_bytes"`
	PaneSignature   string `json:"pane_signature"`
}

// stallSamplesFile is the on-disk envelope.
type stallSamplesFile struct {
	Version int                    `json:"version"`
	Samples map[string]StallSample `json:"samples"`
}

// StallSampleStore reads and writes a rig witness's stall samples.
type StallSampleStore struct {
	path string
}

// NewStallSampleStore returns the store for rig's witness under townRoot.
func NewStallSampleStore(townRoot, rig string) *StallSampleStore {
	return &StallSampleStore{path: filepath.Join(WitnessStateDir(townRoot, rig), stallSamplesFileName)}
}

// Path returns the samples file path.
func (s *StallSampleStore) Path() string { return s.path }

// Load returns the persisted samples keyed by polecat. A missing file is an
// empty set. An unreadable or unparseable file returns an empty set AND an
// error: the caller starts over with fresh samples, which only delays a
// verdict by one window.
func (s *StallSampleStore) Load() (map[string]StallSample, error) {
	samples := map[string]StallSample{}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return samples, nil
	}
	if err != nil {
		return samples, fmt.Errorf("reading %s: %w", s.path, err)
	}
	var f stallSamplesFile
	if err := json.Unmarshal(data, &f); err != nil {
		return samples, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	if f.Version != stallSamplesVersion {
		return samples, fmt.Errorf("%s: unknown schema version %d (want %d); starting over", s.path, f.Version, stallSamplesVersion)
	}
	for name, sample := range f.Samples {
		samples[name] = sample
	}
	return samples, nil
}

// Save replaces the persisted samples atomically (temp file + rename).
func (s *StallSampleStore) Save(samples map[string]StallSample) error {
	if samples == nil {
		samples = map[string]StallSample{}
	}
	return atomicfile.EnsureDirAndWriteJSON(s.path, stallSamplesFile{Version: stallSamplesVersion, Samples: samples})
}

// StallCheck is the stall-rule result for one observed polecat.
type StallCheck struct {
	Polecat string
	// Baseline is sample 1 as it stands after this scan: the sample this
	// observation was compared against, or the one it just recorded.
	Baseline StallSample
	// Recorded is true when this observation became the new sample 1 (first
	// sighting, a new session, or work since the previous sample 1).
	Recorded bool
	// Stalled is the AssessStall verdict against the kept sample 1. It is
	// never true on a scan that recorded a new sample 1.
	Stalled bool
	// Reason explains the outcome in one line.
	Reason string
}

// TrackStalls applies the gt-xb27 stall rule to one scan's observations using
// the persisted sample 1 for each polecat, then saves the updated samples.
//
// For each observation:
//   - no usable sample 1 (none, another session or transcript, undated) →
//     record this observation as sample 1;
//   - the transcript advanced or the pane signature changed since sample 1 →
//     the agent is working; record this observation as the new sample 1;
//   - otherwise keep sample 1 and report AssessStall(sample1, cur, window).
//
// Samples for polecats absent from observed (no live session) are pruned.
// The returned error reports a lock, load or save failure; the checks are still
// valid for this scan and must not be discarded.
//
// Load, compute and save run under an flock on a sibling lock file, so two
// concurrent scans cannot interleave. If the lock cannot be taken in
// stallSamplesLockTimeout the scan still judges against what it read but saves
// nothing, rather than clobber the holder's samples.
func TrackStalls(store *StallSampleStore, observed []RealActivity, window time.Duration, now time.Time) ([]StallCheck, error) {
	locked, lockErr := store.lock()
	if locked != nil {
		defer func() { _ = locked.Unlock() }()
	}
	samples, loadErr := store.Load()

	next := make(map[string]StallSample, len(observed))
	checks := make([]StallCheck, 0, len(observed))
	for _, cur := range observed {
		if cur.Polecat == "" {
			continue
		}
		if cur.ObservedAt.IsZero() {
			cur.ObservedAt = now
		}
		check := trackOne(samples[cur.Polecat], samples[cur.Polecat].Polecat != "", cur, window)
		next[cur.Polecat] = check.Baseline
		checks = append(checks, check)
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Polecat < checks[j].Polecat })

	if lockErr != nil {
		return checks, errors.Join(lockErr, loadErr)
	}
	saveErr := store.Save(next)
	return checks, errors.Join(loadErr, saveErr)
}

// lock takes the samples lock, or returns why it could not.
func (s *StallSampleStore) lock() (*flock.Flock, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return nil, fmt.Errorf("stall samples lock: %w", err)
	}
	fl := flock.New(s.path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), stallSamplesLockTimeout)
	defer cancel()
	ok, err := fl.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !ok {
		return nil, fmt.Errorf("stall samples lock %s not taken in %s (samples not saved): %v", fl.Path(), stallSamplesLockTimeout, err)
	}
	return fl, nil
}

func trackOne(prev StallSample, havePrev bool, cur RealActivity, window time.Duration) StallCheck {
	record := func(reason string) StallCheck {
		return StallCheck{Polecat: cur.Polecat, Baseline: sampleOf(cur), Recorded: true, Reason: reason}
	}
	now := cur.ObservedAt
	switch {
	case !havePrev:
		return record(fmt.Sprintf("sample 1 recorded; compare after %s", window))
	case prev.SampledAt.IsZero() || prev.SampledAt.After(now):
		return record("sample 1 had no usable sampled_at; sample 1 re-recorded")
	case prev.LastObservedAt.IsZero() || now.Sub(prev.LastObservedAt) > stallSampleMaxScanGap:
		return record(fmt.Sprintf("no scan saw this polecat for over %s (host asleep or witness down); sample 1 re-recorded", stallSampleMaxScanGap))
	case prev.Session != cur.Session || prev.TranscriptPath != cur.TranscriptPath:
		return record("new session or transcript since sample 1; sample 1 re-recorded")
	case prev.TranscriptMtime.IsZero():
		return record("sample 1 had no transcript date; sample 1 re-recorded")
	case prev.PaneSignature == "":
		return record("sample 1 had no pane signature; sample 1 re-recorded")
	}

	if why := workSince(prev, cur); why != "" {
		return record(why + " since sample 1 — agent is working; sample 1 re-recorded")
	}

	verdict := AssessStall(prev.activity(), cur, window)
	kept := prev
	kept.LastObservedAt = now.UTC()
	return StallCheck{Polecat: cur.Polecat, Baseline: kept, Stalled: verdict.Stalled, Reason: verdict.Reason}
}

// workSince names the progress cur shows over sample 1, or "" when it shows
// none. Only signals cur actually measured count: a missing transcript or pane
// signature is an observation gap, not progress.
func workSince(prev StallSample, cur RealActivity) string {
	if cur.ActivitySource == ActivitySourceTranscript && !cur.LastActivity.IsZero() {
		if cur.TranscriptBytes != prev.TranscriptBytes {
			return fmt.Sprintf("transcript changed size by %d bytes", cur.TranscriptBytes-prev.TranscriptBytes)
		}
		if cur.LastActivity.After(prev.TranscriptMtime) {
			return "transcript advanced"
		}
	}
	if cur.PaneSignature != "" && prev.PaneSignature != "" && cur.PaneSignature != prev.PaneSignature {
		return "pane content changed"
	}
	return ""
}

// sampleOf projects an observation into a persisted sample.
func sampleOf(a RealActivity) StallSample {
	s := StallSample{
		Polecat:        a.Polecat,
		Session:        a.Session,
		SampledAt:      a.ObservedAt.UTC(),
		LastObservedAt: a.ObservedAt.UTC(),
		TranscriptPath: a.TranscriptPath,
		PaneSignature:  a.PaneSignature,
	}
	if a.ActivitySource == ActivitySourceTranscript && !a.LastActivity.IsZero() {
		s.TranscriptMtime = a.LastActivity.UTC()
		s.TranscriptBytes = a.TranscriptBytes
	}
	return s
}

// activity rebuilds the RealActivity AssessStall compares against.
func (s StallSample) activity() RealActivity {
	a := RealActivity{
		Polecat:         s.Polecat,
		Session:         s.Session,
		ObservedAt:      s.SampledAt,
		TranscriptPath:  s.TranscriptPath,
		TranscriptBytes: s.TranscriptBytes,
		PaneSignature:   s.PaneSignature,
		ActivitySource:  ActivitySourceNone,
	}
	if !s.TranscriptMtime.IsZero() {
		a.LastActivity = s.TranscriptMtime
		a.ActivitySource = ActivitySourceTranscript
	}
	return a
}
