package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// noDeadline is the budget of a test whose sources answer at once: far enough
// away that none can reach it, so the test never waits for the clock.
const noDeadline = time.Minute

// expired is the budget of a test about a missed deadline: it is spent before
// the source starts, so the deadline has already ended when Collect looks and
// the test keeps no wall-clock wait.
const expired = time.Duration(0)

// sourceFunc is a Source a test writes inline: a group and what Collect
// returns.
type sourceFunc struct {
	group string
	fn    func(context.Context, Env) (map[string]any, error)
}

func (s sourceFunc) Group() string { return s.group }

func (s sourceFunc) Collect(ctx context.Context, env Env) (map[string]any, error) {
	return s.fn(ctx, env)
}

// fixedNow is the clock of a test that does not test the timestamp.
func fixedNow() time.Time {
	return time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
}

// envAt is an Env whose clock stands at now.
func envAt(now time.Time) Env {
	return Env{Now: func() time.Time { return now }}
}

// healthy is a source that reports one column, so a test can see the groups
// beside a failed one still fill.
func healthy(group string) Source {
	return sourceFunc{group: group, fn: func(context.Context, Env) (map[string]any, error) {
		return map[string]any{"open": 3}, nil
	}}
}

func TestCollectFailingSourcesLeaveNullGroup(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		source  func(t *testing.T) Source
		wantErr string
	}{
		{
			name: "returns an error",
			source: func(*testing.T) Source {
				return sourceFunc{group: "bad", fn: func(context.Context, Env) (map[string]any, error) {
					return nil, errors.New("bd list: no Dolt server")
				}}
			},
			wantErr: "no Dolt server",
		},
		{
			name: "panics",
			source: func(*testing.T) Source {
				return sourceFunc{group: "bad", fn: func(context.Context, Env) (map[string]any, error) {
					panic("bd list exploded")
				}}
			},
			wantErr: "panic: bd list exploded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := Collect(context.Background(), envAt(now), []Source{tc.source(t), healthy("queue")}, noDeadline)
			if rec.V != Version {
				t.Errorf("V = %d, want %d", rec.V, Version)
			}
			if rec.TS != "2026-10-24T13:00:00Z" {
				t.Errorf("TS = %q, want the env clock in UTC", rec.TS)
			}
			if cols, ok := rec.Groups["bad"]; !ok || cols != nil {
				t.Errorf("Groups[bad] = %v, %v; want the failed group present as null", cols, ok)
			}
			if got := rec.Groups["queue"]["open"]; got != 3 {
				t.Errorf("Groups[queue][open] = %v, want 3 beside the failed group", got)
			}
			if len(rec.Errors) != 1 || rec.Errors[0].Group != "bad" {
				t.Fatalf("Errors = %v, want the one bad entry", rec.Errors)
			}
			if got := rec.Errors[0].Error; !strings.Contains(got, tc.wantErr) {
				t.Errorf("Errors[bad].Error = %q, want it to name %q", got, tc.wantErr)
			}
		})
	}
}

// A source that misses its deadline leaves its group null and an errors entry
// naming the miss. The spent budget ends the deadline before the source
// starts, so no case waits on the clock.
func TestCollectMissedDeadlineLeavesNullGroup(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		source  func(t *testing.T) Source
		wantErr string
	}{
		{
			name: "blocks until its context ends",
			source: func(*testing.T) Source {
				return sourceFunc{group: "bad", fn: func(ctx context.Context, _ Env) (map[string]any, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}}
			},
			wantErr: "context deadline exceeded",
		},
		{
			name: "ignores its context",
			source: func(t *testing.T) Source {
				release := make(chan struct{})
				t.Cleanup(func() { close(release) })
				return sourceFunc{group: "bad", fn: func(context.Context, Env) (map[string]any, error) {
					<-release
					return map[string]any{"late": true}, nil
				}}
			},
			wantErr: "no result within " + expired.String(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := Collect(context.Background(), envAt(now), []Source{tc.source(t)}, expired)
			if rec.TS != "2026-10-24T13:00:00Z" {
				t.Errorf("TS = %q, want the env clock in UTC", rec.TS)
			}
			if cols, ok := rec.Groups["bad"]; !ok || cols != nil {
				t.Errorf("Groups[bad] = %v, %v; want the missed group present as null", cols, ok)
			}
			if len(rec.Errors) != 1 || rec.Errors[0].Group != "bad" {
				t.Fatalf("Errors = %v, want the one bad entry", rec.Errors)
			}
			if got := rec.Errors[0].Error; !strings.Contains(got, tc.wantErr) {
				t.Errorf("Errors[bad].Error = %q, want it to name %q", got, tc.wantErr)
			}
		})
	}
}

// A group name two sources claim keeps the first source's columns; the second
// is an errors entry rather than a silent overwrite.
func TestCollectDuplicateGroupKeepsTheFirstSource(t *testing.T) {
	t.Parallel()
	first := sourceFunc{group: "queue", fn: func(context.Context, Env) (map[string]any, error) {
		return map[string]any{"open": 3}, nil
	}}
	second := sourceFunc{group: "queue", fn: func(context.Context, Env) (map[string]any, error) {
		return map[string]any{"open": 99}, nil
	}}
	rec := Collect(context.Background(), envAt(fixedNow()), []Source{first, second}, noDeadline)
	if got := rec.Groups["queue"]["open"]; got != 3 {
		t.Errorf("Groups[queue][open] = %v, want the first source's 3", got)
	}
	if len(rec.Errors) != 1 || rec.Errors[0].Group != "queue" || !strings.Contains(rec.Errors[0].Error, "twice") {
		t.Errorf("Errors = %v, want one entry naming the duplicated group", rec.Errors)
	}
}

// A zero Env has no clock to call: the record takes the wall clock instead of
// panicking.
func TestCollectNilNowUsesTheWallClock(t *testing.T) {
	t.Parallel()
	before := time.Now()
	rec := Collect(context.Background(), Env{}, []Source{healthy("queue")}, noDeadline)
	ts, err := time.Parse(time.RFC3339, rec.TS)
	if err != nil {
		t.Fatalf("TS = %q, want an RFC 3339 stamp from the wall clock: %v", rec.TS, err)
	}
	if ts.Before(before.Add(-time.Minute)) || ts.After(time.Now().Add(time.Minute)) {
		t.Errorf("TS = %v, want a time within a minute of now", ts)
	}
}

// A snapshot whose own context ends reports the cancellation, not a timeout
// the source never missed.
func TestCollectParentCancellationIsNotATimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	blocked := sourceFunc{group: "bad", fn: func(context.Context, Env) (map[string]any, error) {
		<-release
		return map[string]any{"late": true}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := Collect(ctx, envAt(fixedNow()), []Source{blocked}, noDeadline)
	if cols, ok := rec.Groups["bad"]; !ok || cols != nil {
		t.Errorf("Groups[bad] = %v, %v; want the group present as null", cols, ok)
	}
	if len(rec.Errors) != 1 || rec.Errors[0].Group != "bad" {
		t.Fatalf("Errors = %v, want the one bad entry", rec.Errors)
	}
	got := rec.Errors[0].Error
	if !strings.Contains(got, "context canceled") {
		t.Errorf("Errors[bad].Error = %q, want it to name the cancellation", got)
	}
	if strings.Contains(got, "no result within") {
		t.Errorf("Errors[bad].Error = %q, want the cancellation not reported as a timeout", got)
	}
}

func TestCollectEverySourceFailingStillReturnsRecord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	failing := func(group string) Source {
		return sourceFunc{group: group, fn: func(context.Context, Env) (map[string]any, error) {
			return nil, errors.New("no reading")
		}}
	}
	rec := Collect(context.Background(), envAt(now), []Source{failing("host"), failing("queue")}, time.Second)
	if rec.V != Version {
		t.Errorf("V = %d, want %d", rec.V, Version)
	}
	if rec.TS != "2026-10-24T11:00:00Z" {
		t.Errorf("TS = %q, want env.Now() converted to UTC", rec.TS)
	}
	if len(rec.Errors) != 2 {
		t.Errorf("Errors = %v, want one entry per failed source", rec.Errors)
	}
	for _, group := range []string{"host", "queue"} {
		if cols, ok := rec.Groups[group]; !ok || cols != nil {
			t.Errorf("Groups[%s] = %v, %v; want null", group, cols, ok)
		}
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal the all-failing record: %v", err)
	}
	var back Record
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("the all-failing record does not read back: %v", err)
	}
	if back.TS != rec.TS || back.V != Version {
		t.Errorf("decoded V, TS = %d, %q; want %d, %q", back.V, back.TS, Version, rec.TS)
	}
}

func TestCollectPlacementSourceFillsFixedKey(t *testing.T) {
	t.Parallel()
	placement := sourceFunc{group: PlacementGroup, fn: func(context.Context, Env) (map[string]any, error) {
		return map[string]any{"dolt": "docker-desktop"}, nil
	}}
	rec := Collect(context.Background(), envAt(fixedNow()), []Source{placement, healthy("host")}, time.Second)
	if _, ok := rec.Groups[PlacementGroup]; ok {
		t.Errorf("placement was written as a group key, want the fixed placement key")
	}
	if got := rec.Placement["dolt"]; got != "docker-desktop" {
		t.Errorf("Placement[dolt] = %v, want docker-desktop", got)
	}
	if _, ok := rec.Groups["host"]; !ok {
		t.Errorf("Groups = %v, want the other groups beside placement", rec.Groups)
	}
}

func TestCollectFailingPlacementSourceLeavesNullPlacement(t *testing.T) {
	t.Parallel()
	placement := sourceFunc{group: PlacementGroup, fn: func(context.Context, Env) (map[string]any, error) {
		return nil, errors.New("placement.json: unknown field")
	}}
	rec := Collect(context.Background(), envAt(fixedNow()), []Source{placement}, time.Second)
	if rec.Placement != nil {
		t.Errorf("Placement = %v, want null", rec.Placement)
	}
	if len(rec.Errors) != 1 || rec.Errors[0].Group != PlacementGroup {
		t.Errorf("Errors = %v, want the one placement entry", rec.Errors)
	}
}

func TestCollectReservedGroupNameIsAnError(t *testing.T) {
	t.Parallel()
	// "ts" is a fixed key: a source claiming it would overwrite the record's
	// own timestamp, so Collect drops the columns and records why.
	reserved := sourceFunc{group: "ts", fn: func(context.Context, Env) (map[string]any, error) {
		return map[string]any{"n": 1}, nil
	}}
	now := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
	rec := Collect(context.Background(), envAt(now), []Source{reserved}, time.Second)
	if rec.TS != "2026-10-24T13:00:00Z" {
		t.Errorf("TS = %q, want the env clock untouched", rec.TS)
	}
	if _, ok := rec.Groups["ts"]; ok {
		t.Errorf("Groups = %v, want the reserved group dropped", rec.Groups)
	}
	if len(rec.Errors) != 1 || !strings.Contains(rec.Errors[0].Error, "reserved") {
		t.Errorf("Errors = %v, want one entry naming the reserved group", rec.Errors)
	}
}
