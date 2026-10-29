package hostprobe

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
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

// Write writes p to path atomically, stamping the schema version, so a reader
// never sees a half file and a crash mid-write leaves the previous probes in
// place. A value JSON cannot hold, such as a NaN reading, fails here instead of
// writing a file no reader can decode.
func Write(path string, p Probes) error {
	p.V = SchemaVersion
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode probes for %s: %w", path, err)
	}
	// atomicfile writes a temp file in path's own directory and renames it
	// over path, removing the temp file when either step fails.
	if err := atomicfile.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write probes to %s: %w", path, err)
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
