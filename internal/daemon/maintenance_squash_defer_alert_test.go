package daemon

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode"
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
