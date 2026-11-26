package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// TestSquashDeferAlertCounter pins the per-database count of consecutive gc-flag
// deferrals: each deferral adds one for its database and returns the new count,
// the databases count apart, and a reset starts its database over (gt-2n9wff).
func TestSquashDeferAlertCounter(t *testing.T) {
	t.Parallel()
	type step struct {
		db    string
		reset bool
		want  int
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name:  "three deferrals of one database count one two three",
			steps: []step{{db: "gt", want: 1}, {db: "gt", want: 2}, {db: "gt", want: 3}},
		},
		{
			name:  "a second database counts apart",
			steps: []step{{db: "gt", want: 1}, {db: "gt", want: 2}, {db: "hq", want: 1}, {db: "gt", want: 3}},
		},
		{
			name:  "a reset starts its database over at its next deferral",
			steps: []step{{db: "gt", want: 1}, {db: "gt", want: 2}, {db: "gt", want: 3}, {db: "gt", reset: true}, {db: "gt", want: 1}},
		},
		{
			name:  "a reset of a database never deferred changes nothing",
			steps: []step{{db: "gt", reset: true}, {db: "gt", want: 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var counter squashDeferrals
			for i, s := range tt.steps {
				if s.reset {
					counter.reset(s.db)
					continue
				}
				if got := counter.deferred(s.db); got != s.want {
					t.Fatalf("step %d: deferred(%q) = %d, want %d", i, s.db, got, s.want)
				}
			}
		})
	}
}

// TestSquashDeferAlertMessage pins the alert's clock and its one-line message:
// the alert waits a day, and the message names the database and the whole hours
// the gc flag held its checks, with no other value in it (gt-tdfaj4).
func TestSquashDeferAlertMessage(t *testing.T) {
	t.Parallel()
	if squashDeferAlertAfter != 24*time.Hour {
		t.Fatalf("squashDeferAlertAfter = %v, want 24h", squashDeferAlertAfter)
	}
	tests := []struct {
		name string
		db   string
		held time.Duration
		want string
	}{
		{
			name: "a whole day names the database and 24 hours",
			db:   "gt",
			held: 24 * time.Hour,
			want: "squash: gt deferred by the gc flag for 24h",
		},
		{
			name: "a partial hour reads as the whole hours held",
			db:   "gt",
			held: 25*time.Hour + 30*time.Minute,
			want: "squash: gt deferred by the gc flag for 25h",
		},
		{
			name: "a newline in the database name is one line",
			db:   "gt\nsquash: forged",
			held: 24 * time.Hour,
			want: "squash: gt?squash: forged deferred by the gc flag for 24h",
		},
		{
			name: "a control character in the database name is one line",
			db:   "gt\x07\r",
			held: 24 * time.Hour,
			want: "squash: gt?? deferred by the gc flag for 24h",
		},
		{
			name: "a line separator in the database name is one line",
			db:   "gt\u2028hq",
			held: 24 * time.Hour,
			want: "squash: gt?hq deferred by the gc flag for 24h",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := squashDeferAlertMessage(tt.db, tt.held)
			if got != tt.want {
				t.Fatalf("squashDeferAlertMessage(%q, %v) = %q, want %q", tt.db, tt.held, got, tt.want)
			}
			if strings.IndexFunc(got, unicode.IsControl) != -1 {
				t.Fatalf("squashDeferAlertMessage(%q, %v) = %q, want one line", tt.db, tt.held, got)
			}
		})
	}
}

// TestSquashDeferAlertDueSince pins the pure time-based due decision: the alert
// is due only once the first deferral is a day old and the database has not been
// alerted, so a zero first and an already-alerted database are never due
// (gt-2m37vf).
func TestSquashDeferAlertDueSince(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		first   time.Time
		now     time.Time
		alerted bool
		want    bool
	}{
		{
			name:  "a zero first is not due",
			first: time.Time{},
			now:   start.Add(48 * time.Hour),
			want:  false,
		},
		{
			name:  "23h59m is not due",
			first: start,
			now:   start.Add(23*time.Hour + 59*time.Minute),
			want:  false,
		},
		{
			name:  "24h is due",
			first: start,
			now:   start.Add(24 * time.Hour),
			want:  true,
		},
		{
			name:    "an alerted database is never due",
			first:   start,
			now:     start.Add(24 * time.Hour),
			alerted: true,
			want:    false,
		},
		{
			name:    "an alerted database is not due a week later",
			first:   start,
			now:     start.Add(7 * 24 * time.Hour),
			alerted: true,
			want:    false,
		},
		{
			name:  "a now before the first deferral is not due",
			first: start,
			now:   start.Add(-time.Hour),
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := squashDeferAlertDueSince(tt.first, tt.now, tt.alerted); got != tt.want {
				t.Fatalf("squashDeferAlertDueSince(%v, %v, %t) = %t, want %t", tt.first, tt.now, tt.alerted, got, tt.want)
			}
		})
	}
}

// TestSquashDeferAlertCounterConcurrent pins that the counter the Daemon holds
// counts every deferral when the deferrals run on different goroutines.
func TestSquashDeferAlertCounterConcurrent(t *testing.T) {
	t.Parallel()
	const deferrals = 50
	var d Daemon
	finished := make(chan struct{}, deferrals)
	for i := 0; i < deferrals; i++ {
		go func() {
			d.squashDeferrals.deferred("gt")
			finished <- struct{}{}
		}()
	}
	guard, cancel := context.WithTimeout(context.Background(), workspaceHangGuard)
	defer cancel()
	for i := 0; i < deferrals; i++ {
		select {
		case <-finished:
		case <-guard.Done():
			t.Fatalf("only %d of %d concurrent deferrals finished within the hang guard", i, deferrals)
		}
	}
	if got := d.squashDeferrals.deferred("gt"); got != deferrals+1 {
		t.Fatalf("deferred after %d concurrent deferrals = %d, want %d", deferrals, got, deferrals+1)
	}
}

// TestSquashDeferredSince pins the read of the persisted deferral stamp: a
// database stamped under its own key on the alert clock reads back that
// instant, and a database with no stamp, a stamp for another database, an
// entry that cannot be read and an entry that is not a timestamp all read as
// the zero time (gt-b8na21).
func TestSquashDeferredSince(t *testing.T) {
	t.Parallel()
	raised := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name  string
		db    string
		setup func(t *testing.T, dir string)
		want  time.Time
	}{
		{
			name: "a stamp for the database reads back its instant",
			db:   "gt",
			setup: func(t *testing.T, dir string) {
				stampSquashDefer(t, dir, "squash-defer:gt", raised)
			},
			want: raised,
		},
		{
			name:  "a database with no stamp reads as the zero time",
			db:    "gt",
			setup: func(t *testing.T, dir string) {},
			want:  time.Time{},
		},
		{
			name: "another database's stamp is not this database's",
			db:   "hq",
			setup: func(t *testing.T, dir string) {
				stampSquashDefer(t, dir, "squash-defer:gt", raised)
			},
			want: time.Time{},
		},
		{
			name: "an entry that cannot be read reads as the zero time",
			db:   "gt",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "squash-defer:gt"), 0o700); err != nil {
					t.Fatalf("creating the entry directory: %v", err)
				}
			},
			want: time.Time{},
		},
		{
			name: "an entry that is not a timestamp reads as the zero time",
			db:   "gt",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("creating the clock directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "squash-defer:gt"), []byte("not a timestamp\n"), 0o600); err != nil {
					t.Fatalf("writing the garbage entry: %v", err)
				}
			},
			want: time.Time{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
			tc.setup(t, alertClockPath(d.config.TownRoot))

			got := d.squashDeferredSince(tc.db)
			if !got.Equal(tc.want) {
				t.Fatalf("squashDeferredSince(%q) = %v, want %v", tc.db, got, tc.want)
			}
		})
	}
}

// TestClearSquashDeferred pins the clear of the persisted deferral stamp: after
// a mark, a clear returns squashDeferredSince to the zero time, a second clear
// is a no-op, a mark after the clear stamps a new time, another database's
// stamp survives, and a stamp that cannot be removed returns its error
// (gt-jykwan).
func TestClearSquashDeferred(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	later := first.Add(48 * time.Hour)
	stamped := func(t *testing.T, dir string) { stampSquashDefer(t, dir, "squash-defer:gt", first) }
	tests := []struct {
		name      string
		db        string
		setup     func(t *testing.T, dir string)
		clears    int
		remark    *time.Time
		otherDB   string
		wantOther time.Time
		wantErr   bool
		want      time.Time
	}{
		{
			name:  "a mark is cleared back to the zero time",
			db:    "gt",
			setup: stamped,
			want:  time.Time{},
		},
		{
			name:   "a second clear is a no-op",
			db:     "gt",
			setup:  stamped,
			clears: 2,
			want:   time.Time{},
		},
		{
			name:   "a mark after the clear stamps a new time",
			db:     "gt",
			setup:  stamped,
			remark: &later,
			want:   later,
		},
		{
			name:  "a database with no stamp clears without error",
			db:    "gt",
			setup: func(t *testing.T, dir string) {},
			want:  time.Time{},
		},
		{
			name: "another database's stamp is left in place",
			db:   "gt",
			setup: func(t *testing.T, dir string) {
				stamped(t, dir)
				stampSquashDefer(t, dir, "squash-defer:hq", later)
			},
			otherDB:   "hq",
			wantOther: later,
			want:      time.Time{},
		},
		{
			name: "a stamp that cannot be removed returns the error",
			db:   "gt",
			setup: func(t *testing.T, dir string) {
				entry := filepath.Join(dir, "squash-defer:gt")
				if err := os.MkdirAll(entry, 0o700); err != nil {
					t.Fatalf("creating the entry directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(entry, "held"), nil, 0o600); err != nil {
					t.Fatalf("writing the entry's child: %v", err)
				}
			},
			wantErr: true,
			want:    time.Time{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
			dir := alertClockPath(d.config.TownRoot)
			tc.setup(t, dir)

			for i := 0; i < max(tc.clears, 1); i++ {
				if err := d.clearSquashDeferred(tc.db); (err != nil) != tc.wantErr {
					t.Fatalf("clear %d of %q: err = %v, wantErr %t", i+1, tc.db, err, tc.wantErr)
				}
			}
			if tc.remark != nil {
				stampSquashDefer(t, dir, squashDeferClockKey(tc.db), *tc.remark)
			}
			if got := d.squashDeferredSince(tc.db); !got.Equal(tc.want) {
				t.Fatalf("squashDeferredSince(%q) after clear = %v, want %v", tc.db, got, tc.want)
			}
			if tc.otherDB != "" {
				if got := d.squashDeferredSince(tc.otherDB); !got.Equal(tc.wantOther) {
					t.Fatalf("squashDeferredSince(%q) after clearing %q = %v, want %v", tc.otherDB, tc.db, got, tc.wantOther)
				}
			}
		})
	}
}

// stampSquashDefer writes at on the alert clock under dir, the way the
// deferral's mark will, so a row reads a real clock entry and not a file the
// test made up.
func stampSquashDefer(t *testing.T, dir, key string, at time.Time) {
	t.Helper()
	if err := (alertClock{dir: dir}).stamp(key, at); err != nil {
		t.Fatalf("stamping the alert clock: %v", err)
	}
}

// TestSquashDeferAlertRaise pins the one function that raises the deferral
// alert: it reads the database's persisted since-stamp, and a call before the
// day is up raises nothing while a call a day after it raises one alert naming
// the database and the hours held (gt-ehjn9r).
func TestSquashDeferAlertRaise(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		db   string
		now  time.Time
		// want is the alert's reason; empty means no alert is raised.
		want string
	}{
		{
			name: "23h59m after the first deferral raises nothing",
			db:   "gt",
			now:  first.Add(23*time.Hour + 59*time.Minute),
		},
		{
			name: "24h after the first deferral raises one alert",
			db:   "gt",
			now:  first.Add(24 * time.Hour),
			want: "squash: gt deferred by the gc flag for 24h",
		},
		{
			name: "a database the flag never deferred raises nothing",
			db:   "hq",
			now:  first.Add(48 * time.Hour),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _, _ := squashDueDaemon(t)
			stampSquashDefer(t, alertClockPath(d.config.TownRoot), squashDeferClockKey("gt"), first)

			if err := d.raiseSquashDeferAlert(tt.db, tt.now); err != nil {
				t.Fatalf("raiseSquashDeferAlert(%q, %v): %v", tt.db, tt.now, err)
			}

			raised := d.notifier.(*notifyfake.Recorder).Escalations()
			if tt.want == "" {
				if len(raised) != 0 {
					t.Fatalf("raiseSquashDeferAlert(%q, %v) raised %d alerts, want none: %+v",
						tt.db, tt.now, len(raised), raised)
				}
				return
			}
			if len(raised) != 1 {
				t.Fatalf("raiseSquashDeferAlert(%q, %v) raised %d alerts, want one: %+v",
					tt.db, tt.now, len(raised), raised)
			}
			alert := raised[0].Escalation
			if alert.Reason != tt.want {
				t.Errorf("alert reason = %q, want %q", alert.Reason, tt.want)
			}
			if wantKey := "squash:gc-flag-held:" + tt.db; alert.Fingerprint != wantKey {
				t.Errorf("alert fingerprint = %q, want %q", alert.Fingerprint, wantKey)
			}
		})
	}
}
