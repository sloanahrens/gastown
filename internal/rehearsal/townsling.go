package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const slingBound = 5 * time.Minute

// Sling assigns id to agent in the town's rig and returns nil on success; a run
// past its five minute bound fails wrapping context.DeadlineExceeded.
func (t Town) Sling(ctx context.Context, id, agent string) error {
	ctx, cancel := context.WithTimeout(ctx, slingBound)
	defer cancel()
	argv := []string{"gt", "sling", id, t.Rig, "--agent", agent}
	if _, err := t.Run(ctx, argv); err != nil {
		// A killed process's error misses the deadline; join the context's own.
		return fmt.Errorf("gt sling %s: %w", id, errors.Join(err, ctx.Err()))
	}
	return nil
}

// Close closes the bead id with reason through bd in the town's rig.
func (t Town) Close(ctx context.Context, id, reason string) error {
	argv := []string{"bd"}
	if t.Rig != "" {
		argv = append(argv, "-C", t.Rig)
	}
	argv = append(argv, "close", id, "--reason="+reason)
	if _, err := t.Run(ctx, argv); err != nil {
		return fmt.Errorf("bd close %s: %w", id, err)
	}
	return nil
}
