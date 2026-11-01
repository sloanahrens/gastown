package hostload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// TestSampleRecordProbeFailure checks a sample carries its readings and that
// one probe's failure sets that probe's error without taking the others.
func TestSampleRecordProbeFailure(t *testing.T) {
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

// TestSampleWithoutSeamsRefusesEachProbe drives the guards a Prober built
// without its seams hits: every probe reports its error, and none of them
// reports an empty reading as a quiet host.
func TestSampleWithoutSeamsRefusesEachProbe(t *testing.T) {
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

// hasOwner reports whether rows holds the named owner.
func hasOwner(rows []Row, owner string) bool {
	for _, r := range rows {
		if r.Owner == owner {
			return true
		}
	}
	return false
}
