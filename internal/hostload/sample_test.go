package hostload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/hostprobe"
)

// sampleEpoch is the fixed instant the sample tests are stamped with.
var sampleEpoch = time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)

// tickLog is the daemon line a sample reads the dispatcher's tick duration
// from.
const tickLog = "2026/11/01 12:00:00 spec_dispatch: tick took 5.432s (roster [claude 1/2], board 2ms, candidates 0 1ms, sling 1ms); roster 1 calls\n"

// sampleRecord is a Sample as one JSON line holds it, so the tests assert the
// record's shape rather than the struct.
type sampleRecord struct {
	At     time.Time            `json:"at"`
	Load   recordField[Load]    `json:"load"`
	BD     recordField[float64] `json:"bd_latency_ms"`
	Tick   recordField[float64] `json:"tick_ms"`
	Seats  recordField[int]     `json:"seats"`
	Owners recordField[[]Row]   `json:"owners"`
}

// recordField is one probe in the record: its reading and the error that
// stopped it.
type recordField[T any] struct {
	Value T      `json:"value"`
	Err   string `json:"err"`
}

// record samples the host, encodes the sample as the --sample loop would, and
// decodes it back.
func record(t *testing.T, p *Prober) sampleRecord {
	t.Helper()
	line, err := json.Marshal(p.Sample(context.Background()))
	if err != nil {
		t.Fatalf("encoding the sample: %v", err)
	}
	var rec sampleRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("decoding %s: %v", line, err)
	}
	return rec
}

// sampleRun answers the host commands one sample runs; the probe named in fail
// answers with an error instead.
func sampleRun(fail string) hostprobe.Runner {
	return func(_ context.Context, argv []string) ([]byte, error) {
		switch argv[0] {
		case "ps":
			return []byte(cannedPS), nil
		case "top":
			return []byte(cannedTop), nil
		case "lsof":
			if fail == "lsof" {
				return nil, errors.New("exit status 1")
			}
			return []byte(cannedLsof), nil
		case "sysctl":
			return []byte("{ 4.08 2.06 1.02 }"), nil
		case "bd":
			if fail == "bd" {
				return nil, errors.New("exit status 1")
			}
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected command %q", argv[0])
	}
}

// sampleProber is a Prober over canned host answers: no runner starts a
// process, no reader opens the live town, and the clock is fixed.
func sampleProber(t *testing.T, run hostprobe.Runner, readLog func(string, int64) ([]byte, error)) *Prober {
	t.Helper()
	return &Prober{
		TownRoot:  t.TempDir(),
		Run:       run,
		ReadFile:  func(string) ([]byte, error) { return nil, errors.New("no /proc/loadavg here") },
		ReadLog:   readLog,
		SeatCount: func() (int, error) { return 6, nil },
		Clock:     clockwork.NewFakeClockAt(sampleEpoch),
		Loc:       time.UTC,
	}
}

// TestHostLoadSampleRecordProbeFailure checks a sample carries its readings and that
// one probe's failure sets that probe's error without taking the others.
func TestHostLoadSampleRecordProbeFailure(t *testing.T) {
	t.Parallel()
	t.Run("a failed bd call leaves the rest of the record", func(t *testing.T) {
		p := sampleProber(t, sampleRun("bd"), func(string, int64) ([]byte, error) {
			return []byte(tickLog), nil
		})
		rec := record(t, p)
		if !rec.At.Equal(sampleEpoch) {
			t.Errorf("record stamped %s, want %s", rec.At, sampleEpoch)
		}
		if want := (Load{One: 4.08, Five: 2.06, Fifteen: 1.02}); rec.Load.Err != "" || rec.Load.Value != want {
			t.Errorf("load = %+v, want %+v (err %q)", rec.Load.Value, want, rec.Load.Err)
		}
		if !strings.Contains(rec.BD.Err, "bd list --limit 1") || rec.BD.Value != 0 {
			t.Errorf("bd latency = %v, want its error and no value", rec.BD)
		}
		if rec.Tick.Err != "" || rec.Tick.Value != 5432 {
			t.Errorf("tick = %v, want 5432ms", rec.Tick)
		}
		if rec.Seats.Err != "" || rec.Seats.Value != 6 {
			t.Errorf("seats = %v, want 6", rec.Seats)
		}
		if rec.Owners.Err != "" || !hasOwner(rec.Owners.Value, "crew/dunlin") {
			t.Errorf("owners = %+v (err %q), want the crew session's row", rec.Owners.Value, rec.Owners.Err)
		}
	})

	t.Run("an unreadable tick log leaves the rest of the record", func(t *testing.T) {
		p := sampleProber(t, sampleRun(""), func(string, int64) ([]byte, error) {
			return nil, errors.New("open daemon.log: permission denied")
		})
		rec := record(t, p)
		if !strings.Contains(rec.Tick.Err, "daemon.log") || rec.Tick.Value != 0 {
			t.Errorf("tick = %v, want its error and no value", rec.Tick)
		}
		if rec.BD.Err != "" || rec.Load.Err != "" || rec.Seats.Value != 6 {
			t.Errorf("record = %+v, want the other probes to carry", rec)
		}
		if rec.Owners.Err != "" || !hasOwner(rec.Owners.Value, "crew/dunlin") {
			t.Errorf("owners = %+v (err %q), want the crew session's row", rec.Owners.Value, rec.Owners.Err)
		}
	})

	t.Run("a failed lsof refuses the table instead of attributing by command alone", func(t *testing.T) {
		p := sampleProber(t, sampleRun("lsof"), func(string, int64) ([]byte, error) {
			return []byte(tickLog), nil
		})
		rec := record(t, p)
		if !strings.Contains(rec.Owners.Err, "lsof") || rec.Owners.Value != nil {
			t.Errorf("owners = %v, want lsof's error and no table", rec.Owners)
		}
		if rec.Load.Err != "" || rec.BD.Err != "" || rec.Tick.Err != "" || rec.Seats.Err != "" {
			t.Errorf("record = %+v, want the other probes to carry", rec)
		}
	})
}

// TestHostLoadOwnersKeepLsofPartialAnswer drives lsof's production failure
// shape: it exits non-zero for a pid that vanished after top's sample while
// still answering for the rest, and that answer is the owner table (gt-nu89c).
func TestHostLoadOwnersKeepLsofPartialAnswer(t *testing.T) {
	t.Parallel()
	p := sampleProber(t, func(_ context.Context, argv []string) ([]byte, error) {
		switch argv[0] {
		case "ps":
			return []byte(cannedPS), nil
		case "top":
			return []byte(cannedTop), nil
		case "lsof":
			return []byte(cannedLsof), errors.New("exit status 1")
		}
		return nil, fmt.Errorf("unexpected command %q", argv[0])
	}, func(string, int64) ([]byte, error) { return []byte(tickLog), nil })
	rec := record(t, p)
	if rec.Owners.Err != "" {
		t.Fatalf("owners = %q, want the answer lsof printed before it failed", rec.Owners.Err)
	}
	for _, owner := range []string{"crew/dunlin", OwnerLanding, "vm/qemu-system-aarch64"} {
		if !hasOwner(rec.Owners.Value, owner) {
			t.Errorf("owners = %+v, want the %s row", rec.Owners.Value, owner)
		}
	}
}

// TestHostLoadSampleWithoutSeamsRefusesEachProbe drives the guards a Prober built
// without its seams hits: every probe reports its error, and none of them
// reports an empty reading as a quiet host.
func TestHostLoadSampleWithoutSeamsRefusesEachProbe(t *testing.T) {
	t.Parallel()
	rec := record(t, &Prober{TownRoot: t.TempDir()})
	fields := map[string]string{
		"load":   rec.Load.Err,
		"bd":     rec.BD.Err,
		"tick":   rec.Tick.Err,
		"seats":  rec.Seats.Err,
		"owners": rec.Owners.Err,
	}
	for name, err := range fields {
		if err == "" {
			t.Errorf("the %s probe carried no error without its seam", name)
		}
	}
}

// TestHostLoadOwnersProbeRefusesAFailedRead drives each read the owners probe can lose:
// a ps or top that failed, and a top that printed no table.
func TestHostLoadOwnersProbeRefusesAFailedRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  hostprobe.Runner
		want string
	}{
		{
			name: "ps failed",
			run: func(_ context.Context, argv []string) ([]byte, error) {
				if argv[0] == "ps" {
					return nil, errors.New("exit status 1")
				}
				return nil, fmt.Errorf("unexpected command %q", argv[0])
			},
			want: "ps:",
		},
		{
			name: "top failed",
			run: func(_ context.Context, argv []string) ([]byte, error) {
				switch argv[0] {
				case "ps":
					return []byte(cannedPS), nil
				case "top":
					return nil, errors.New("exit status 1")
				}
				return nil, fmt.Errorf("unexpected command %q", argv[0])
			},
			want: "top:",
		},
		{
			name: "top printed no table",
			run: func(_ context.Context, argv []string) ([]byte, error) {
				switch argv[0] {
				case "ps":
					return []byte(cannedPS), nil
				case "top":
					return []byte("no table here\n"), nil
				}
				return nil, fmt.Errorf("unexpected command %q", argv[0])
			},
			want: "no process table",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := sampleProber(t, tc.run, func(string, int64) ([]byte, error) {
				return []byte(tickLog), nil
			})
			rec := record(t, p)
			if !strings.Contains(rec.Owners.Err, tc.want) {
				t.Errorf("owners error = %q, want it to name %q", rec.Owners.Err, tc.want)
			}
			// Tick and seats do not go through the runner, so they carry
			// while the owners probe refuses.
			if rec.Seats.Err != "" || rec.Tick.Err != "" {
				t.Errorf("record = %+v, want the probes beside owners to carry", rec)
			}
		})
	}
}

// TestHostLoadProbeRefusesBothReads checks a host that answers neither the kernel
// file nor sysctl reports an error instead of an idle host.
func TestHostLoadProbeRefusesBothReads(t *testing.T) {
	t.Parallel()
	p := &Prober{
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("no /proc/loadavg") },
		Run: func(_ context.Context, argv []string) ([]byte, error) {
			return nil, fmt.Errorf("%s failed", argv[0])
		},
		Clock: clockwork.NewFakeClockAt(sampleEpoch),
	}
	l, err := p.load(context.Background())
	if err == nil {
		t.Fatalf("load = %+v, want an error when neither read answers", l)
	}
	if l != (Load{}) {
		t.Errorf("load = %+v, want no reading beside the error", l)
	}
	if !strings.Contains(err.Error(), "sysctl -n vm.loadavg") {
		t.Errorf("error = %q, want it to name the sysctl fallback", err)
	}
}

// TestHostLoadRunArgvRefusesAnEmptyCommand checks the command runner refuses an empty
// argv instead of starting nothing.
func TestHostLoadRunArgvRefusesAnEmptyCommand(t *testing.T) {
	t.Parallel()
	if _, err := runArgv(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("runArgv ran an empty command")
	}
}

// TestHostLoadTownSeatCountReadsTheSeats reads the seat capacity from a town's
// settings file.
func TestHostLoadTownSeatCountReadsTheSeats(t *testing.T) {
	t.Parallel()
	town := writeTownSettings(t, `{"polecat_pool":{"seats":{"claude":{"cap":2},"codex":{"cap":3}}}}`)
	got, err := townSeatCount(town)
	if err != nil {
		t.Fatalf("townSeatCount: %v", err)
	}
	if want := 5; got != want {
		t.Errorf("seat count = %d, want %d", got, want)
	}
}

// TestHostLoadTownSeatCountRefusesAMissingFile checks a town with no settings
// file reports an error, not a town with no seats.
func TestHostLoadTownSeatCountRefusesAMissingFile(t *testing.T) {
	t.Parallel()
	_, err := townSeatCount(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "town settings") {
		t.Errorf("townSeatCount = %v, want a town settings error", err)
	}
}

// TestHostLoadTownSeatCountRefusesBrokenSettings checks a settings file this package
// cannot read is an error, not a town with no seats.
func TestHostLoadTownSeatCountRefusesBrokenSettings(t *testing.T) {
	t.Parallel()
	town := writeTownSettings(t, `{"polecat_pool":`)
	_, err := townSeatCount(town)
	if err == nil || !strings.Contains(err.Error(), "town settings") {
		t.Errorf("townSeatCount = %v, want a town settings error", err)
	}
}

// writeTownSettings writes settings/config.json under a fresh town and returns
// the town's root.
func writeTownSettings(t *testing.T, settings string) string {
	t.Helper()
	town := t.TempDir()
	dir := filepath.Join(town, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// hasOwner reports whether rows holds the named owner.
func hasOwner(rows []Row, owner string) bool {
	for _, r := range rows {
		if r.Owner == owner {
			return true
		}
	}
	return false
}
