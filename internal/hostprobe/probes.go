package hostprobe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is the version of the probe file this binary writes and reads.
// A file stamped with another version is refused rather than half-read: a
// field added in a later schema would decode as its zero value, and a zero
// reads as a host with nothing running and no disk left.
const SchemaVersion = 1

// futureTolerance is how far ahead of the reader's clock a probe's stamp may
// sit. The agent and the daemon read one host's clock, so a stamp beyond this
// is a clock that jumped, and a future stamp must not read as fresh however
// long the file sits unrewritten.
const futureTolerance = 5 * time.Second

// Field is one probe reading. A failed read sets Err and leaves Value at its
// zero, so one command that failed does not cost the daemon the rest of the
// file; Err, not Value, says whether the reading happened.
type Field[T any] struct {
	Value T      `json:"value"`
	Err   string `json:"err,omitempty"`
}

// Disk is the town volume's use, in the shape the daemon's disk rule reads
// (hostDisk in internal/daemon/attention_host.go).
type Disk struct {
	UsedPct float64 `json:"used_pct"`
	FreeGB  float64 `json:"free_gb"`
}

// Consumer is one of the host's busiest processes.
type Consumer struct {
	Name string `json:"name"`
	// CPU is percent of one core, the reading hoststat's top sample takes.
	CPU float64 `json:"cpu"`
}

// Probes is one reading of the host: what the host agent writes to
// .runtime/host/probes.json and what the container daemon reads, since a
// container cannot run memory_pressure or ps against the host.
type Probes struct {
	// V is the schema version. Write stamps it, so the file always says what
	// wrote it and Read can refuse a file from another schema.
	V            int               `json:"v"`
	At           time.Time         `json:"at"`
	MemFreePct   Field[float64]    `json:"mem_free_pct"`
	Disk         Field[Disk]       `json:"disk"`
	Containers   Field[[]string]   `json:"containers"`
	CPUIdlePct   Field[float64]    `json:"cpu_idle_pct"`
	TopConsumers Field[[]Consumer] `json:"top_consumers"`
	ExecTaxMS    Field[float64]    `json:"exec_tax_ms"`
}

// probeFileMode is the permissions the probe file is published with: the host
// agent writes it and the daemon, which in container mode is another user,
// reads it.
const probeFileMode = 0o644

// tempFile is the one temp file a write creates before it is renamed into
// place. *os.File is the production value; a test supplies its own.
type tempFile interface {
	Name() string
	Write(p []byte) (int, error)
	Chmod(mode os.FileMode) error
	Sync() error
	Close() error
}

// probeFS is the filesystem one write touches. Every field is a seam with the
// real operation behind a nil one, so a test sets only the call it scripts.
// The rename needs one: a real filesystem publishes the temp file or leaves
// it, and no test can make it fail on demand without staging a directory in
// the target's place. That is also why the write below runs atomicfile's
// sequence itself instead of calling it: atomicfile has no such seam.
type probeFS struct {
	// createTemp makes the temp file in the directory the rename happens in;
	// nil is os.CreateTemp.
	createTemp func(dir, pattern string) (tempFile, error)
	// rename publishes the temp file over the target; nil is os.Rename.
	rename func(oldpath, newpath string) error
	// remove deletes the temp file; nil is os.Remove.
	remove func(name string) error
}

// withDefaults fills the fields the caller left nil, so the zero value is
// production.
func (fs probeFS) withDefaults() probeFS {
	if fs.createTemp == nil {
		fs.createTemp = func(dir, pattern string) (tempFile, error) {
			return os.CreateTemp(dir, pattern)
		}
	}
	if fs.rename == nil {
		fs.rename = os.Rename
	}
	if fs.remove == nil {
		fs.remove = os.Remove
	}
	return fs
}

// Write writes p to path atomically, stamping the schema version, so a reader
// sees the whole new file or the previous one and never a half file.
//
// A write whose rename fails removes its temp file and leaves the target as it
// found it. A value JSON cannot hold, such as a NaN reading, and a zero At — a
// stamp every reader computes as older than any maxAge — fail here instead of
// writing a file no reader can use.
func Write(path string, p Probes) error {
	return write(path, p, probeFS{})
}

// write is Write with its filesystem injected: probeFS is the seam a test
// scripts a failing step on. The round-trip test goes through Write, so a
// wrapper that stopped passing the real operations fails there.
func write(path string, p Probes, fs probeFS) error {
	if p.At.IsZero() {
		return fmt.Errorf("write probes to %s: no At stamp, so every reader would refuse the file as stale", path)
	}
	p.V = SchemaVersion
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode probes for %s: %w", path, err)
	}
	fs = fs.withDefaults()

	// The temp file goes in the target's directory: a rename is atomic only
	// within a filesystem.
	dir, base := filepath.Dir(path), filepath.Base(path)
	f, err := fs.createTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("write probes to %s: %w", path, err)
	}
	tmp := f.Name()

	// Every failure after the temp file exists removes it, so the directory is
	// left holding the target the write found and nothing else.
	discard := func(cause error) error {
		// The write has already failed; the close only releases the handle,
		// so its error says nothing the caller can act on.
		_ = f.Close()
		if rmErr := fs.remove(tmp); rmErr != nil {
			return fmt.Errorf("write probes to %s: %w (and removing the temp file %s: %v)",
				path, cause, tmp, rmErr)
		}
		return fmt.Errorf("write probes to %s: %w", path, cause)
	}

	if _, err := f.Write(data); err != nil {
		return discard(err)
	}
	// CreateTemp makes the file 0600; the daemon reads it as another user.
	if err := f.Chmod(probeFileMode); err != nil {
		return discard(err)
	}
	// The bytes reach the disk before the rename publishes them, so the
	// destination never names a file whose contents are still only in the page
	// cache. The directory is not synced: a crash between the rename and its
	// directory write loses the newest probes and leaves the previous file in
	// place, which Read reports as stale once maxAge passes and the agent
	// replaces within the minute.
	if err := f.Sync(); err != nil {
		return discard(err)
	}
	if err := f.Close(); err != nil {
		return discard(err)
	}
	if err := fs.rename(tmp, path); err != nil {
		return discard(err)
	}
	return nil
}

// Read returns the probes in the file at path, read as of now, and refuses a
// file the daemon cannot use: one that is missing or cannot be decoded, one
// from another schema version, one stamped more than maxAge ago, and one
// stamped more than futureTolerance ahead of now. A refusal is an error, never
// an empty Probes: the attention rules report a failing probe on an error and
// would read an empty value as a healthy host.
func Read(path string, now time.Time, maxAge time.Duration) (Probes, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Probes{}, fmt.Errorf("read probe file %s: %w", path, err)
	}
	var p Probes
	if err := json.Unmarshal(data, &p); err != nil {
		return Probes{}, fmt.Errorf("decode probe file %s: %w", path, err)
	}
	if p.V != SchemaVersion {
		return Probes{}, fmt.Errorf("probe file %s is schema version %d, want %d", path, p.V, SchemaVersion)
	}
	if age := now.Sub(p.At); age > maxAge {
		return Probes{}, fmt.Errorf("probe file %s is %s old, older than the %s allowed",
			path, age.Round(time.Second), maxAge)
	}
	if ahead := p.At.Sub(now); ahead > futureTolerance {
		return Probes{}, fmt.Errorf("probe file %s is stamped %s in the future, beyond the %s a clock may drift",
			path, ahead.Round(time.Second), futureTolerance)
	}
	return p, nil
}
