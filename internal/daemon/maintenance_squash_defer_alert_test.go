package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/doltsquash"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

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

// TestClearSquashDeferredClearsAlertedMark pins what a run's clear takes with
// it: the database's squash-alerted mark goes with its deferral stamp, another
// database's mark stays, and a mark that cannot be removed returns the error
// and keeps the run's stamp, so the run is not reset while a mark it left is
// still standing (gt-w9z5ob).
func TestClearSquashDeferredClearsAlertedMark(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	alertedAt := first.Add(24 * time.Hour)
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		// wantErr requires the clear of "gt" to fail.
		wantErr bool
		// wantStamp is squashDeferredSince("gt") after the clear.
		wantStamp time.Time
		// wantMark is whether squash-alerted:gt is still on the clock.
		wantMark bool
		// wantOtherMark is whether squash-alerted:hq is still on the clock.
		wantOtherMark bool
	}{
		{
			name: "a clear takes the alerted mark with the run's stamp",
			setup: func(t *testing.T, dir string) {
				stampSquashDefer(t, dir, squashDeferClockKey("gt"), first)
				stampSquashDefer(t, dir, squashAlertedClockKey("gt"), alertedAt)
				stampSquashDefer(t, dir, squashAlertedClockKey("hq"), alertedAt)
			},
			wantOtherMark: true,
		},
		{
			name: "a mark that cannot be removed returns the error and keeps the run's stamp",
			setup: func(t *testing.T, dir string) {
				stampSquashDefer(t, dir, squashDeferClockKey("gt"), first)
				entry := filepath.Join(dir, squashAlertedClockKey("gt"))
				if err := os.MkdirAll(entry, 0o700); err != nil {
					t.Fatalf("creating the mark's directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(entry, "held"), nil, 0o600); err != nil {
					t.Fatalf("writing the mark's child: %v", err)
				}
			},
			wantErr:   true,
			wantStamp: first,
			wantMark:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
			dir := alertClockPath(d.config.TownRoot)
			tc.setup(t, dir)

			if err := d.clearSquashDeferred("gt"); (err != nil) != tc.wantErr {
				t.Fatalf("clearSquashDeferred(\"gt\") err = %v, wantErr %t", err, tc.wantErr)
			}
			if got := d.squashDeferredSince("gt"); !got.Equal(tc.wantStamp) {
				t.Fatalf("squashDeferredSince(\"gt\") after clear = %v, want %v", got, tc.wantStamp)
			}
			// Lstat, not the clock's read: a mark that cannot be read is still
			// a mark on the clock.
			if _, err := os.Lstat(filepath.Join(dir, squashAlertedClockKey("gt"))); (err == nil) != tc.wantMark {
				t.Fatalf("squash-alerted:gt on the clock = %t, want %t (err %v)", err == nil, tc.wantMark, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, squashAlertedClockKey("hq"))); (err == nil) != tc.wantOtherMark {
				t.Fatalf("squash-alerted:hq on the clock = %t, want %t (err %v)", err == nil, tc.wantOtherMark, err)
			}
		})
	}
}

// TestMarkSquashDeferred pins the stamp of the first deferral: a first mark
// makes squashDeferredSince return the daemon clock's time, a mark later keeps
// that first time, one database's mark leaves another database unstamped, and
// a stamp that cannot be written returns its error (gt-n6kgco).
func TestMarkSquashDeferred(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	later := first.Add(90 * time.Minute)
	tests := []struct {
		name string
		db   string
		// marks holds each mark's time as an offset from first, in order, so
		// the fake clock reaches each mark without a sleep.
		marks []time.Duration
		// blockClock puts a file where the clock's directory goes, so the
		// stamp cannot be written.
		blockClock bool
		otherDB    string
		want       time.Time
		wantOther  time.Time
		wantErr    bool
	}{
		{
			name:  "a first mark stamps the clock's time",
			db:    "gt",
			marks: []time.Duration{0},
			want:  first,
		},
		{
			name:  "a second mark later keeps the first time",
			db:    "gt",
			marks: []time.Duration{0, later.Sub(first)},
			want:  first,
		},
		{
			name:      "a mark of one database leaves another unstamped",
			db:        "gt",
			marks:     []time.Duration{0},
			otherDB:   "hq",
			want:      first,
			wantOther: time.Time{},
		},
		{
			name:       "a stamp that cannot be written returns the error",
			db:         "gt",
			marks:      []time.Duration{0},
			blockClock: true,
			wantErr:    true,
			want:       time.Time{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
			fake := clockwork.NewFakeClockAt(first)
			d.clock = fake
			if tc.blockClock {
				clockPath := alertClockPath(d.config.TownRoot)
				if err := os.MkdirAll(filepath.Dir(clockPath), 0o700); err != nil {
					t.Fatalf("creating the clock's parent: %v", err)
				}
				if err := os.WriteFile(clockPath, nil, 0o600); err != nil {
					t.Fatalf("putting a file where the clock's directory goes: %v", err)
				}
			}
			var prev time.Duration
			for i, at := range tc.marks {
				fake.Advance(at - prev)
				prev = at
				if err := d.markSquashDeferred(tc.db); (err != nil) != tc.wantErr {
					t.Fatalf("mark %d of %q: err = %v, wantErr %t", i+1, tc.db, err, tc.wantErr)
				}
			}
			if got := d.squashDeferredSince(tc.db); !got.Equal(tc.want) {
				t.Fatalf("squashDeferredSince(%q) after the marks = %v, want %v", tc.db, got, tc.want)
			}
			if tc.otherDB != "" {
				if got := d.squashDeferredSince(tc.otherDB); !got.Equal(tc.wantOther) {
					t.Fatalf("squashDeferredSince(%q) after marking %q = %v, want %v", tc.otherDB, tc.db, got, tc.wantOther)
				}
			}
		})
	}
}

// TestSquashDeferStampSurvivesARoll pins the persisted deferral stamp against a
// daemon roll: a second Daemon built on the alert clock directory the first one
// marked reads the first's since time, and its own mark keeps that time rather
// than restarting the run's day (gt-h5sb8s).
func TestSquashDeferStampSurvivesARoll(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	later := first.Add(48 * time.Hour)
	tests := []struct {
		name string
		// remark is when the rolled daemon marks the database; nil means it
		// only reads the first daemon's stamp.
		remark *time.Time
	}{
		{name: "a rolled daemon reads the first daemon's stamp"},
		{name: "a rolled daemon's mark keeps the first stamp", remark: &later},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			marked := &Daemon{config: &Config{TownRoot: town}}
			marked.clock = clockwork.NewFakeClockAt(first)
			if err := marked.markSquashDeferred("gt"); err != nil {
				t.Fatalf("the first daemon's mark: %v", err)
			}

			rolled := &Daemon{config: &Config{TownRoot: town}}
			rolled.clock = clockwork.NewFakeClockAt(later)
			if got := rolled.squashDeferredSince("gt"); !got.Equal(first) {
				t.Fatalf("a second daemon's squashDeferredSince(%q) = %v, want the first daemon's %v",
					"gt", got, first)
			}
			if tt.remark != nil {
				if err := rolled.markSquashDeferred("gt"); err != nil {
					t.Fatalf("the rolled daemon's mark: %v", err)
				}
				if got := rolled.squashDeferredSince("gt"); !got.Equal(first) {
					t.Fatalf("squashDeferredSince(%q) after the rolled daemon's mark = %v, want the first stamp %v",
						"gt", got, first)
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

// TestSquashDeferAlertRaisesOnce pins the mark a delivered alert leaves: a call
// a day after the database's first deferral raises one alert and stamps the
// alert clock under squash-alerted:<db> at the call's instant. A call that
// raises nothing and one whose delivery failed leave no mark (gt-jyjnxd). A
// database holding the mark raises no second alert, however many times it is
// asked (gt-w9z5ob).
func TestSquashDeferAlertRaisesOnce(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	refused := errors.New("the notifier refused the alert")
	tests := []struct {
		name string
		db   string
		now  time.Time
		// calls is how many times the function is called at now; zero means
		// once.
		calls int
		// deliveryErr is what the notifier fake returns for the escalation.
		deliveryErr error
		// blockMark puts a directory where the mark's entry goes.
		blockMark bool
		// strangeMark puts a mark that is not a timestamp where the entry goes.
		strangeMark bool
		// loopMark puts a symlink that loops onto itself where the entry goes,
		// so the mark cannot be read though it can be replaced.
		loopMark bool
		// wantCalls counts the escalation attempts; wantErr requires an error
		// (the delivery's when deliveryErr is set); wantMarked is whether the
		// clock holds squash-alerted:<db> at now.
		wantCalls  int
		wantErr    bool
		wantMarked bool
	}{
		{
			name:       "a call a day after the first deferral raises one alert and marks the database",
			db:         "gt",
			now:        first.Add(24 * time.Hour),
			wantCalls:  1,
			wantMarked: true,
		},
		{
			name:       "two calls a day after the first deferral raise one alert",
			db:         "gt",
			now:        first.Add(24 * time.Hour),
			calls:      2,
			wantCalls:  1,
			wantMarked: true,
		},
		{
			name: "two calls a week after the first deferral raise one alert",
			db:   "gt",
			now:  first.Add(7 * 24 * time.Hour),
			// The week's first call raises, later ones read the mark and wait.
			calls:      5,
			wantCalls:  1,
			wantMarked: true,
		},
		{
			name:        "a mark that is not a timestamp raises the alert again",
			db:          "gt",
			now:         first.Add(24 * time.Hour),
			strangeMark: true,
			wantCalls:   1,
			wantMarked:  true,
		},
		{
			name:       "a mark that cannot be read reads as not alerted and is rewritten",
			db:         "gt",
			now:        first.Add(24 * time.Hour),
			loopMark:   true,
			wantCalls:  1,
			wantMarked: true,
		},
		{
			name: "a call before the day is up raises nothing and leaves no mark",
			db:   "gt",
			now:  first.Add(23*time.Hour + 59*time.Minute),
		},
		{
			name:        "a delivery the notifier refused returns its error and leaves no mark",
			db:          "gt",
			now:         first.Add(24 * time.Hour),
			deliveryErr: refused,
			wantCalls:   1,
			wantErr:     true,
		},
		{
			name:      "a mark that cannot be written returns its error and leaves the database unmarked",
			db:        "gt",
			now:       first.Add(24 * time.Hour),
			wantCalls: 1,
			wantErr:   true,
			blockMark: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _ := squashDueDaemon(t)
			clock := alertClock{dir: alertClockPath(d.config.TownRoot)}
			stampSquashDefer(t, clock.dir, squashDeferClockKey("gt"), first)
			recorder := d.notifier.(*notifyfake.Recorder)
			if tc.deliveryErr != nil {
				recorder.Fail(notifyfake.KindEscalate, tc.deliveryErr)
			}
			// The literal key, not the helper's answer, so a renamed key fails.
			key := "squash-alerted:" + tc.db
			if tc.blockMark {
				if err := os.MkdirAll(filepath.Join(clock.dir, key), 0o700); err != nil {
					t.Fatalf("blocking the mark's entry: %v", err)
				}
			}
			if tc.strangeMark {
				if err := os.MkdirAll(clock.dir, 0o700); err != nil {
					t.Fatalf("creating the clock directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(clock.dir, key), []byte("not a timestamp\n"), 0o600); err != nil {
					t.Fatalf("writing the strange mark: %v", err)
				}
			}

			if tc.loopMark {
				if err := os.MkdirAll(clock.dir, 0o700); err != nil {
					t.Fatalf("creating the clock directory: %v", err)
				}
				if err := os.Symlink(key, filepath.Join(clock.dir, key)); err != nil {
					t.Fatalf("writing the looping mark: %v", err)
				}
			}

			var err error
			for i := 0; i < max(tc.calls, 1); i++ {
				if err = d.raiseSquashDeferAlert(tc.db, tc.now); err != nil {
					break
				}
			}
			if tc.wantErr && err == nil {
				t.Fatalf("raiseSquashDeferAlert(%q, %v) err = nil, want an error", tc.db, tc.now)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("raiseSquashDeferAlert(%q, %v): %v", tc.db, tc.now, err)
			}
			if tc.deliveryErr != nil && !errors.Is(err, tc.deliveryErr) {
				t.Fatalf("raiseSquashDeferAlert(%q, %v) err = %v, want %v", tc.db, tc.now, err, tc.deliveryErr)
			}

			if raised := recorder.Escalations(); len(raised) != tc.wantCalls {
				t.Fatalf("raiseSquashDeferAlert(%q, %v) made %d escalation attempts, want %d",
					tc.db, tc.now, len(raised), tc.wantCalls)
			}

			marked, ok := clock.last(key)
			if ok != tc.wantMarked {
				t.Fatalf("the alert clock holds %s = %t, want marked %t", key, ok, tc.wantMarked)
			}
			if tc.wantMarked && !marked.Equal(tc.now) {
				t.Errorf("%s = %v, want the call's instant %v", key, marked, tc.now)
			}
		})
	}
}

// TestSquashDeferAlertRaisesAgainAfterClear pins that a run which ends is not
// carried into the next: once the first run's alert is raised and its clear
// runs, the next run's alert waits its own day from the next run's first
// deferral and is raised, not suppressed by the mark the first run left
// (gt-w9z5ob).
func TestSquashDeferAlertRaisesAgainAfterClear(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	nextRun := first.Add(48 * time.Hour)
	d, _, _ := squashDueDaemon(t)
	dir := alertClockPath(d.config.TownRoot)
	recorder := d.notifier.(*notifyfake.Recorder)
	stampSquashDefer(t, dir, squashDeferClockKey("gt"), first)

	if err := d.raiseSquashDeferAlert("gt", first.Add(24*time.Hour)); err != nil {
		t.Fatalf("raising the first run's alert: %v", err)
	}
	if err := d.clearSquashDeferred("gt"); err != nil {
		t.Fatalf("clearing the first run: %v", err)
	}
	stampSquashDefer(t, dir, squashDeferClockKey("gt"), nextRun)

	if err := d.raiseSquashDeferAlert("gt", nextRun.Add(23*time.Hour+59*time.Minute)); err != nil {
		t.Fatalf("raising before the next run's day: %v", err)
	}
	if n := len(recorder.Escalations()); n != 1 {
		t.Fatalf("after the next run's first call before its day, %d alerts raised, want 1", n)
	}
	if err := d.raiseSquashDeferAlert("gt", nextRun.Add(24*time.Hour)); err != nil {
		t.Fatalf("raising the next run's alert: %v", err)
	}
	if n := len(recorder.Escalations()); n != 2 {
		t.Fatalf("after the next run's day, %d alerts raised, want 2: the first run's mark suppressed it", n)
	}
}

// TestSquashDeferAlertSurvivesARoll pins the alert's mark against a daemon roll:
// a second Daemon on the alert clock directory the first one alerted from reads
// the mark that alert left, so a day after the first deferral it raises no
// second alert for the database (gt-wr0j0c).
func TestSquashDeferAlertSurvivesARoll(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	d, _, _ := squashDueDaemon(t)
	stampSquashDefer(t, alertClockPath(d.config.TownRoot), squashDeferClockKey("gt"), first)
	if err := d.raiseSquashDeferAlert("gt", first.Add(24*time.Hour)); err != nil {
		t.Fatalf("the first daemon's alert: %v", err)
	}
	if raised := d.notifier.(*notifyfake.Recorder).Escalations(); len(raised) != 1 {
		t.Fatalf("the first daemon raised %d alerts, want one: %+v", len(raised), raised)
	}

	rolled := &Daemon{config: &Config{TownRoot: d.config.TownRoot}, notifier: notifyfake.New()}
	rolled.clock = clockwork.NewFakeClockAt(first.Add(24 * time.Hour))
	if err := rolled.raiseSquashDeferAlert("gt", first.Add(24*time.Hour)); err != nil {
		t.Fatalf("the rolled daemon's alert: %v", err)
	}
	if raised := rolled.notifier.(*notifyfake.Recorder).Escalations(); len(raised) != 0 {
		t.Fatalf("the rolled daemon raised %d alerts, want none: the first alert's mark did not survive the roll: %+v",
			len(raised), raised)
	}
}

// TestSquashDeferAlertAtTheSite pins the deferral site: a run the gc flag holds
// stamps the deferral and logs its day, and the alert is raised by the check a
// day later, with the flag held across both checks (gt-2o03sg).
func TestSquashDeferAlertAtTheSite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		blockStamp  bool  // a file where the alert clock's directory goes
		deliveryErr error // what the notifier fake returns for the alert
		wantAlerts  int   // escalations the last check attempts
		wantLog     string
	}{
		{
			name:       "the last check a day after the first raises one alert",
			wantAlerts: 1,
			wantLog:    "squash: gt: the maintenance gc flag is held — the run waits for the next check, deferred since 2026-11-16T12:00:00Z",
		},
		{
			name:       "a stamp that cannot be written raises no alert",
			blockStamp: true,
			wantAlerts: 0,
			wantLog:    "squash: WARNING: gt: marking the squash deferral of \"gt\"",
		},
		{
			name:        "a refused alert delivery is logged",
			deliveryErr: errors.New("the notifier refused the alert"),
			wantAlerts:  1,
			wantLog:     "squash: WARNING: gt: the deferral alert was not delivered: the notifier refused the alert",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, logs := squashDueDaemon(t)
			fake := clockwork.NewFakeClockAt(squashTestNow)
			d.clock = fake
			d.maint.now = fake.Now
			if tc.blockStamp {
				clockPath := alertClockPath(d.config.TownRoot)
				if err := os.MkdirAll(filepath.Dir(clockPath), 0o700); err != nil {
					t.Fatalf("creating the clock's parent: %v", err)
				}
				if err := os.WriteFile(clockPath, nil, 0o600); err != nil {
					t.Fatalf("putting a file where the clock's directory goes: %v", err)
				}
			}
			if !d.maintenanceGCRunning.CompareAndSwap(false, true) {
				t.Fatal("the maintenance gc flag was already held")
			}
			defer d.maintenanceGCRunning.Store(false)
			recorder := d.notifier.(*notifyfake.Recorder)
			if tc.deliveryErr != nil {
				recorder.Fail(notifyfake.KindEscalate, tc.deliveryErr)
			}

			writeSquashBackup(t, squashBackupRoot(t, d), fake.Now().Add(-6*time.Hour), fake.Now().Add(-6*time.Hour+7*time.Second), 1<<20)
			d.runScheduledMaintenance()
			if n := len(recorder.Escalations()); n != 0 {
				t.Fatalf("the first check raised %d alerts, want none: %+v", n, recorder.Escalations())
			}

			fake.Advance(24 * time.Hour)
			writeSquashBackup(t, squashBackupRoot(t, d), fake.Now().Add(-6*time.Hour), fake.Now().Add(-6*time.Hour+7*time.Second), 1<<20)
			d.runScheduledMaintenance()

			// A deferral is never logged with the zero time as its stamp.
			if got := logs.String(); !strings.Contains(got, tc.wantLog) || strings.Contains(got, "deferred since 0001") {
				t.Errorf("the check's log lacks %q or reports a zero stamp: %q", tc.wantLog, got)
			}
			raised := recorder.Escalations()
			if len(raised) != tc.wantAlerts {
				t.Fatalf("the last check raised %d alerts, want %d: %+v", len(raised), tc.wantAlerts, raised)
			}
			if tc.wantAlerts == 1 && tc.deliveryErr == nil {
				alert := raised[0].Escalation
				if want := "squash: gt deferred by the gc flag for 24h"; alert.Reason != want {
					t.Errorf("alert reason = %q, want %q", alert.Reason, want)
				}
			}
		})
	}
}

// TestSquashRunClearsTheDeferral pins the run's outcome after a deferral: the
// run's clear takes the stamp, and a clear that fails keeps it and makes the
// check due again (gt-2o03sg).
func TestSquashRunClearsTheDeferral(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		blockClear bool      // a directory with a child where the alerted mark goes
		wantStamp  time.Time // squashDeferredSince("gt") after the run
		wantLog    string
	}{
		{
			name:      "a run clears the deferral's stamp",
			wantStamp: time.Time{},
		},
		{
			name:       "a clear that fails keeps the stamp and the check is due again",
			blockClear: true,
			wantStamp:  squashTestNow,
			wantLog:    "squash: WARNING: gt: clearing the squash deferral entry",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, logs := squashDueDaemon(t)
			fake := clockwork.NewFakeClockAt(squashTestNow)
			d.clock = fake
			d.maint.now = fake.Now
			var calls int
			scriptSquashRun(d, &calls, doltsquash.RunResult{}, nil)
			if !d.maintenanceGCRunning.CompareAndSwap(false, true) {
				t.Fatal("the maintenance gc flag was already held")
			}
			writeSquashBackup(t, squashBackupRoot(t, d), fake.Now().Add(-6*time.Hour), fake.Now().Add(-6*time.Hour+7*time.Second), 1<<20)
			d.runScheduledMaintenance()
			if got := d.squashDeferredSince("gt"); !got.Equal(squashTestNow) {
				t.Fatalf("squashDeferredSince(\"gt\") after the held check = %v, want %v", got, squashTestNow)
			}
			d.maintenanceGCRunning.Store(false)

			if tc.blockClear {
				entry := filepath.Join(alertClockPath(d.config.TownRoot), squashAlertedClockKey("gt"))
				if err := os.MkdirAll(entry, 0o700); err != nil {
					t.Fatalf("creating the mark's directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(entry, "held"), nil, 0o600); err != nil {
					t.Fatalf("writing the mark's child: %v", err)
				}
			}

			fake.Advance(24 * time.Hour)
			writeSquashBackup(t, squashBackupRoot(t, d), fake.Now().Add(-6*time.Hour), fake.Now().Add(-6*time.Hour+7*time.Second), 1<<20)
			d.runScheduledMaintenance()

			if calls != 1 {
				t.Fatalf("the check started %d run(s) with the flag free, want 1", calls)
			}
			if tc.wantLog != "" && !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("the check's log lacks %q: %q", tc.wantLog, logs.String())
			}
			if got := d.squashDeferredSince("gt"); !got.Equal(tc.wantStamp) {
				t.Fatalf("squashDeferredSince(\"gt\") after the run = %v, want %v", got, tc.wantStamp)
			}
			if due := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), fake.Now().Add(squashCheckRetryAfter+time.Minute), squashCheckInterval); due.due != tc.blockClear {
				t.Errorf("due again after the retry delay = %t, want %t", due.due, tc.blockClear)
			}
		})
	}
}
