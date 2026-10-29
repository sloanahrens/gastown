package hostprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testAt is the stamp the tests share. It is a fixed date, never time.Now, so
// a test's verdict does not depend on when it ran.
func testAt() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }

// fullProbes is a value with every field filled, the value the round trip is
// about.
func fullProbes() Probes {
	return Probes{
		At:           testAt(),
		MemFreePct:   Field[float64]{Value: 94},
		Disk:         Field[Disk]{Value: Disk{UsedPct: 61.5, FreeGB: 120.25}},
		Containers:   Field[[]string]{Value: []string{"gastown-daemon", "gastown-dolt"}},
		CPUIdlePct:   Field[float64]{Value: 78.5},
		TopConsumers: Field[[]Consumer]{Value: []Consumer{{Name: "dolt", CPU: 120.5}, {Name: "gt", CPU: 40}}},
		ExecTaxMS:    Field[float64]{Value: 12.5},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "probes.json")
	want := fullProbes()
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Write stamps the schema version, so that is what the file holds.
	want.V = SchemaVersion

	got, err := Read(path, testAt().Add(30*time.Second), 3*time.Minute)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// The stamp is compared as an instant: JSON carries no monotonic reading,
	// so a decoded time is never the same struct field by field.
	if !got.At.Equal(want.At) {
		t.Errorf("At = %v, want %v", got.At, want.At)
	}
	want.At, got.At = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestWriteReadKeepsFieldError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "probes.json")
	want := fullProbes()
	want.Disk = Field[Disk]{Err: "df /: exit status 1"}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(path, testAt(), 3*time.Minute)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Disk.Err != want.Disk.Err {
		t.Errorf("Disk.Err = %q, want %q", got.Disk.Err, want.Disk.Err)
	}
	if got.MemFreePct != want.MemFreePct {
		t.Errorf("MemFreePct = %+v, want the failed field to leave its neighbours alone: %+v",
			got.MemFreePct, want.MemFreePct)
	}
}

// TestReadRefusesUnusableFile covers the refusals that do not depend on the
// clock: no file, no JSON, and a schema version this binary does not know.
func TestReadRefusesUnusableFile(t *testing.T) {
	t.Parallel()
	now := testAt()
	for _, tc := range []struct {
		name    string
		body    string // empty means no file is written
		wantErr error  // when set, the error must wrap it
		want    string // the refusal the error must state, so a row cannot pass on another refusal
	}{
		{name: "a missing file", wantErr: fs.ErrNotExist, want: "read probe file"},
		{name: "not JSON", body: "{", want: "decode probe file"},
		{name: "a later schema version", body: fmt.Sprintf(`{"v":2,"at":%q}`, now.Format(time.RFC3339)), want: "schema version 2"},
		{name: "a zero schema version", body: fmt.Sprintf(`{"at":%q}`, now.Format(time.RFC3339)), want: "schema version 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "probes.json")
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
					t.Fatalf("write the fixture: %v", err)
				}
			}

			got, err := Read(path, now, 3*time.Minute)
			if err == nil {
				t.Fatalf("Read = %+v, nil; want an error", got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Read error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Read error = %q, want it to state %q", err, tc.want)
			}
		})
	}
}

// TestReadAgeChecks pins both ends of the freshness window: a stamp older than
// maxAge and one more than futureTolerance ahead are refused, and the two edges
// themselves are read. The stale row fails if the maxAge check goes, the future
// row if the future check goes.
func TestReadAgeChecks(t *testing.T) {
	t.Parallel()
	now := testAt()
	const maxAge = 3 * time.Minute
	for _, tc := range []struct {
		name    string
		at      time.Time
		wantErr bool
	}{
		{name: "a minute old", at: now.Add(-time.Minute)},
		{name: "exactly maxAge old", at: now.Add(-maxAge)},
		{name: "older than maxAge", at: now.Add(-maxAge - time.Second), wantErr: true},
		{name: "five seconds ahead", at: now.Add(futureTolerance)},
		{name: "more than five seconds ahead", at: now.Add(futureTolerance + time.Second), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "probes.json")
			body, err := json.Marshal(Probes{V: SchemaVersion, At: tc.at, MemFreePct: Field[float64]{Value: 94}})
			if err != nil {
				t.Fatalf("marshal the fixture: %v", err)
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatalf("write the fixture: %v", err)
			}

			if _, err := Read(path, now, maxAge); (err != nil) != tc.wantErr {
				t.Errorf("Read error = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}
}

// TestWriteRefusesNaN: JSON cannot carry a NaN, and a write that failed must
// leave no file for the daemon to decode.
func TestWriteRefusesNaN(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "probes.json")
	p := fullProbes()
	p.CPUIdlePct = Field[float64]{Value: math.NaN()}

	if err := Write(path, p); err == nil {
		t.Fatal("Write of a NaN reading succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat after the failed write = %v, want the file to be absent", err)
	}
}

// TestWriteRenameFailureLeavesNoPartialFile: the rename fails for real here —
// on every platform this runs on, a rename over a non-empty directory fails —
// which is how atomicfile's own tests drive its cleanup. A temp file left in
// the directory shows up as a second entry.
func TestWriteRenameFailureLeavesNoPartialFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "probes.json")
	if err := os.MkdirAll(filepath.Join(target, "occupant"), 0o755); err != nil {
		t.Fatalf("stage the target directory: %v", err)
	}

	if err := Write(target, fullProbes()); err == nil {
		t.Fatal("Write over a non-empty directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	if len(entries) != 1 || entries[0].Name() != "probes.json" || !entries[0].IsDir() {
		t.Errorf("entries after the failed write = %v, want only the untouched target", names)
	}
}
