package daemon

import (
	"context"
	"testing"
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
