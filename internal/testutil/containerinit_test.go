package testutil

import (
	"context"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestUnrelatedInitErrorFailsTheTest pins the fail-closed half of the skip
// decision (gt-cbtl): only a lost connection to the test Dolt container may
// skip a container-backed suite. A genuine bd, Dolt or Beads.Init regression
// has to redden the test instead of silencing it.
func TestUnrelatedInitErrorFailsTheTest(t *testing.T) {
	container := beads.NewIsolatedWithPort(t.TempDir(), 55107)
	production := beads.New(t.TempDir())

	// gt-6uhq's observed stderr against a stalled container: bd lost the
	// connection before the command ran.
	containerGone := fmt.Errorf("bd init --prefix pt1a2b --quiet --server-port 55107: %s\n%s",
		"[mysql] read tcp 127.0.0.1:65473->127.0.0.1:55107: i/o timeout",
		"Error: failed to open database: schema skew check: probing schema_migrations existence: invalid connection")

	tests := []struct {
		name string
		b    *beads.Beads
		err  error
		want containerInitOutcome
	}{
		{
			name: "container connection lost",
			b:    container, err: containerGone,
			want: containerInitGone,
		},
		{
			name: "container refusing connections",
			b:    container,
			err:  fmt.Errorf("bd init: failed to open database: dial tcp 127.0.0.1:55107: connect: connection refused"),
			want: containerInitGone,
		},
		{
			name: "bd refusal is not a lost connection",
			b:    container,
			err:  fmt.Errorf("bd init --prefix toolongprefix: Error: prefix must be 2-8 characters"),
			want: containerInitFailed,
		},
		{
			// A wedged init spent its whole five-minute budget; skipping it
			// would only postpone the same answer (gt-824d). The message
			// carries a connection marker alongside the deadline, so this
			// pins that the deadline is read before the marker is.
			name: "wedged init subprocess",
			b:    container,
			err:  fmt.Errorf("bd init --server-port 55107: %s: %w", "read tcp: i/o timeout", context.DeadlineExceeded),
			want: containerInitFailed,
		},
		{
			name: "bd not runnable",
			b:    container,
			err:  fmt.Errorf(`bd init: exec: "bd": executable file not found in $PATH`),
			want: containerInitFailed,
		},
		{
			// The skip is scoped to the test container, so no production
			// wrapper can turn a real failure into a skip.
			name: "connection text on a production wrapper",
			b:    production, err: containerGone,
			want: containerInitFailed,
		},
		{name: "no error", b: container, err: nil, want: containerInitFailed},
		{name: "no wrapper", b: nil, err: containerGone, want: containerInitFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyContainerInit(tt.b, tt.err); got != tt.want {
				t.Errorf("classifyContainerInit(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
