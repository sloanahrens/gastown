package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/update"
)

// sweepDaemonStart is the start of the daemon the probes judge records against.
func sweepDaemonStart() time.Time {
	return time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
}

// TestPodSweepLaunchAnswer pins the answer each outcome gives, and a record from
// an earlier daemon start: the started flag and the detail a person reads.
func TestPodSweepLaunchAnswer(t *testing.T) {
	t.Parallel()
	this := sweepDaemonStart()
	tests := []struct {
		name        string
		record      daemon.SweepSmokeRecord
		wantStarted bool
		wantDetail  string
	}{
		{"started", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "started"}, true, "launched"},
		{"busy", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "busy"}, true, "busy"},
		{"declined", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "declined"}, false, "declined"},
		{"failed", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "failed", Detail: "kubectl refused"}, false, "kubectl refused"},
		{"failed with no detail", daemon.SweepSmokeRecord{DaemonStart: this, Outcome: "failed"}, false, "the sweep launch failed"},
		{"stale record", daemon.SweepSmokeRecord{DaemonStart: this.Add(-time.Hour), Outcome: "started"}, false, "before this daemon's start"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			started, detail, err := sweepLaunchAnswer(tt.record, this)
			if err != nil || started != tt.wantStarted || !strings.Contains(detail, tt.wantDetail) {
				t.Errorf("sweepLaunchAnswer = (%v, %q, %v), want started %v with detail containing %q and no error",
					started, detail, err, tt.wantStarted, tt.wantDetail)
			}
		})
	}
}

// TestPodSweepLaunchAnswerRefuses pins the error: an outcome the record does not
// name is not a verdict.
func TestPodSweepLaunchAnswerRefuses(t *testing.T) {
	t.Parallel()
	started, detail, err := sweepLaunchAnswer(daemon.SweepSmokeRecord{DaemonStart: sweepDaemonStart(), Outcome: "unknown"}, sweepDaemonStart())
	if err == nil {
		t.Errorf("sweepLaunchAnswer = (%v, %q, nil), want an error naming no known outcome", started, detail)
	}
}

// TestPodSweepLaunchStart pins the daemon start the record is judged against:
// a town whose state names no start is an error, since a zero start would judge
// every record current.
func TestPodSweepLaunchStart(t *testing.T) {
	t.Parallel()
	town := func() (string, error) { return "/town", nil }
	tests := []struct {
		name    string
		find    func() (string, error)
		state   *daemon.State
		loadErr error
		wantErr bool
	}{
		{"a state with a start is read", town, &daemon.State{StartedAt: sweepDaemonStart()}, nil, false},
		{"a state with no start is an error", town, &daemon.State{}, nil, true},
		{"a town that is not found is an error", func() (string, error) { return "", errors.New("no town") }, nil, nil, true},
		{"a state that cannot be read is an error", town, nil, errors.New("unreadable"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			load := func(string) (*daemon.State, error) { return tt.state, tt.loadErr }
			start, err := daemonStartFrom(tt.find, load)
			if (err != nil) != tt.wantErr || (!tt.wantErr && !start.Equal(sweepDaemonStart())) {
				t.Errorf("daemonStartFrom = (%v, %v), want the start %v and error %v", start, err, sweepDaemonStart(), tt.wantErr)
			}
		})
	}
}

// sweepReply is one kubectl exec's answer: the output, the exit code and the error.
type sweepReply struct {
	out  string
	exit int
	err  error
}

// sweepAbsent is the answer for a launch-smoke record the daemon has not written.
var sweepAbsent = sweepReply{exit: podSweepSmokeAbsent}

// sweepReads answers each exec with the next reply, repeating the last, and counts.
func sweepReads(replies ...sweepReply) (update.RunFunc, *int) {
	reads := new(int)
	return func(context.Context, string, []string, string, ...string) (string, int, error) {
		r := replies[min(*reads, len(replies)-1)]
		*reads++
		return r.out, r.exit, r.err
	}, reads
}

// sweepRecordJSON is the record the daemon writes for an outcome, stamped with its start.
func sweepRecordJSON(daemonStart time.Time, outcome, detail string) string {
	return fmt.Sprintf(`{"daemon_start":%q,"outcome":%q,"detail":%q}`,
		daemonStart.Format(time.RFC3339), outcome, detail)
}

// sweepProbes is a pod target's probes read through run, started at
// sweepDaemonStart, with wait as the poll between reads of a missing record.
func sweepProbes(run update.RunFunc, wait func(context.Context) error) podImageLabelProbes {
	return podImageLabelProbes{
		sweep:      podSweepSmokeRecord(podPairTarget(), run),
		sweepStart: func() (time.Time, error) { return sweepDaemonStart(), nil },
		sweepWait:  wait,
	}
}

// noWait fails the test when a wait is taken for a record that was present.
func noWait(t *testing.T) func(context.Context) error {
	return func(context.Context) error {
		t.Error("waited for a record that was present")
		return nil
	}
}

// TestPodSweepLaunch pins the answer one read gives, and the read and start errors.
func TestPodSweepLaunch(t *testing.T) {
	t.Parallel()
	this := sweepDaemonStart()
	boom := errors.New("kubectl refused")
	tests := []struct {
		name        string
		probes      func() (podImageLabelProbes, *int)
		wantStarted bool
		wantDetail  string
		wantErr     string
		wantReads   int
	}{
		{"a started record answers started from one read", func() (podImageLabelProbes, *int) {
			run, reads := sweepReads(sweepReply{out: sweepRecordJSON(this, "started", "")})
			return sweepProbes(run, noWait(t)), reads
		}, true, "launched", "", 1},
		{"a read error is returned", func() (podImageLabelProbes, *int) {
			run, reads := sweepReads(sweepReply{err: boom})
			return sweepProbes(run, noWait(t)), reads
		}, false, "", "kubectl refused", 1},
		{"a run with no sweep wait is refused", func() (podImageLabelProbes, *int) {
			return podImageLabelProbes{sweep: podSweepSmokeRecord(podPairTarget(), nil), sweepStart: func() (time.Time, error) { return this, nil }}, new(int)
		}, false, "", "no launch-smoke record reader", 0},
		{"a start that cannot be read is an error", func() (podImageLabelProbes, *int) {
			return podImageLabelProbes{sweep: podSweepSmokeRecord(podPairTarget(), nil), sweepStart: func() (time.Time, error) { return time.Time{}, boom }, sweepWait: noWait(t)}, new(int)
		}, false, "", "reading the daemon's start", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			probes, reads := tt.probes()
			started, detail, err := probes.SweepLaunch(context.Background())
			if (err != nil) != (tt.wantErr != "") || !strings.Contains(fmt.Sprint(err), tt.wantErr) ||
				started != tt.wantStarted || !strings.Contains(detail, tt.wantDetail) || *reads != tt.wantReads {
				t.Errorf("SweepLaunch = (%v, %q, %v) after %d reads, want started %v, detail containing %q, error %q, %d reads",
					started, detail, err, *reads, tt.wantStarted, tt.wantDetail, tt.wantErr, tt.wantReads)
			}
		})
	}
}

// TestPodSweepLaunchRefuses pins the refusals: a probe with no reader, a read
// that fails, a daemon start that cannot be read, a record whose outcome it does
// not name and a record that is not JSON are each refused with their reason, and
// no error carries the record's output.
func TestPodSweepLaunchRefuses(t *testing.T) {
	t.Parallel()
	boom := errors.New("kubectl refused")
	const secret = "SECRET-MARKER"
	startErr := func(p podImageLabelProbes) podImageLabelProbes {
		p.sweepStart = func() (time.Time, error) { return time.Time{}, boom }
		return p
	}
	failRun := func(context.Context, string, []string, string, ...string) (string, int, error) {
		return "", 0, boom
	}
	startedRun, _ := sweepReads(sweepReply{out: sweepRecordJSON(sweepDaemonStart(), "started", "")})
	unnamedRun, _ := sweepReads(sweepReply{out: sweepRecordJSON(sweepDaemonStart(), secret, "")})
	notJSONRun, _ := sweepReads(sweepReply{out: "not json"})
	tests := []struct {
		name    string
		probe   podImageLabelProbes
		wantErr string
	}{
		{"no reader wired", podImageLabelProbes{}, "no launch-smoke record reader"},
		{"the read fails", sweepProbes(failRun, noWait(t)), "kubectl refused"},
		{"the daemon start cannot be read", startErr(sweepProbes(startedRun, noWait(t))), "reading the daemon's start"},
		{"a record whose outcome is not named", sweepProbes(unnamedRun, noWait(t)), "names no known outcome"},
		{"a record that is not JSON", sweepProbes(notJSONRun, noWait(t)), "is not JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			started, _, err := tt.probe.SweepLaunch(context.Background())
			if err == nil || started || !strings.Contains(fmt.Sprint(err), tt.wantErr) {
				t.Errorf("SweepLaunch = (%v, _, %v), want not started with an error naming %q", started, err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("error %q carries the record's output", err)
			}
		})
	}
}

// TestPodSweepLaunchWaits pins the re-read: a record missing at the first read
// is judged when a later read finds it, and one that never appears ends not
// started when the context ends. A wait that fails while the context is live
// ends SweepLaunch with an error, and no further read is made.
func TestPodSweepLaunchWaits(t *testing.T) {
	t.Parallel()
	started := sweepRecordJSON(sweepDaemonStart(), "started", "")

	t.Run("a record missing at the first read is read on the second", func(t *testing.T) {
		t.Parallel()
		run, reads := sweepReads(sweepAbsent, sweepReply{out: started})
		waits := 0
		wait := func(context.Context) error {
			waits++
			if waits > 1 {
				t.Fatal("waited again after the record appeared")
			}
			return nil
		}
		ok, detail, err := sweepProbes(run, wait).SweepLaunch(context.Background())
		if err != nil || !ok || *reads != 2 || waits != 1 {
			t.Errorf("SweepLaunch = (%v, %q, %v) after %d reads and %d waits, want started from 2 reads and 1 wait", ok, detail, err, *reads, waits)
		}
	})

	t.Run("a record that never appears ends not started when the context ends", func(t *testing.T) {
		t.Parallel()
		run, reads := sweepReads(sweepAbsent)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waits := 0
		wait := func(ctx context.Context) error {
			waits++
			if waits > 3 {
				t.Fatal("kept reading after the context ended")
			}
			if waits == 3 {
				cancel()
			}
			return ctx.Err()
		}
		ok, detail, err := sweepProbes(run, wait).SweepLaunch(ctx)
		if err != nil || ok || !strings.Contains(detail, "no launch-smoke record") || *reads != 3 || waits != 3 {
			t.Errorf("SweepLaunch = (%v, %q, %v) after %d reads and %d waits, want not started with the missing-record reason and no error, 3 each", ok, detail, err, *reads, waits)
		}
	})

	t.Run("a wait that returns nil with the context ended still ends", func(t *testing.T) {
		t.Parallel()
		run, reads := sweepReads(sweepAbsent)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waits := 0
		wait := func(context.Context) error {
			waits++
			if waits > 1 {
				t.Fatal("kept reading after the context ended")
			}
			cancel()
			return nil
		}
		ok, detail, err := sweepProbes(run, wait).SweepLaunch(ctx)
		if err != nil || ok || !strings.Contains(detail, "no launch-smoke record") || *reads != 1 || waits != 1 {
			t.Errorf("SweepLaunch = (%v, %q, %v) after %d reads and %d waits, want not started with the missing-record reason and no error, one each", ok, detail, err, *reads, waits)
		}
	})

	t.Run("a wait that fails while the context is live is returned as an error", func(t *testing.T) {
		t.Parallel()
		run, reads := sweepReads(sweepAbsent)
		boom := errors.New("the wait failed")
		waits := 0
		wait := func(context.Context) error {
			waits++
			if waits > 1 {
				t.Fatal("read again after the wait failed")
			}
			return boom
		}
		ok, _, err := sweepProbes(run, wait).SweepLaunch(context.Background())
		if ok || !errors.Is(err, boom) || *reads != 1 || waits != 1 {
			t.Errorf("SweepLaunch = (%v, _, %v) after %d reads and %d waits, want not started with an error wrapping the wait's failure, one each", ok, err, *reads, waits)
		}
	})
}

// TestPodSweepLaunchPollEnded pins podSweepPoll's ended-context branch: an ended
// context returns its error without waiting the poll interval.
func TestPodSweepLaunchPollEnded(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := podSweepPoll(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("podSweepPoll with an ended context = %v, want the context's error", err)
	}
}
