package registry

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// collectConfigPath is where the registry container mounts the configuration
// garbage-collect reads. The command is fixed text plus the validated
// container name, so nothing else a run reads can reach it.
const collectConfigPath = "/etc/docker/registry/config.yml"

// collectCeilingSeconds is the longest the collect may run inside the registry
// container, whatever the caller's deadline leaves, so a caller that sets no
// deadline still gets a bounded collect. Eight minutes fits inside the ten the
// disk-maintenance retention budgets for planning, deletes and collect, so a
// collect the retention starts is ended by the budget it runs under (gt-dl4nv.13).
const collectCeilingSeconds = 8 * 60

// CollectTimeoutSeconds is collectCeilingSeconds as the number the command
// carries. A caller reads it to know the longest a collect can run when its own
// deadline does not bind first; the command always carries this value or less.
const CollectTimeoutSeconds = "480"

// collectGrace is the margin between the container's timeout ending the collect
// and the caller's deadline: the time docker exec has to start the collect, have
// the container's timeout end it and return, so the retention returns with the
// collect over.
const collectGrace = time.Second

// collectSkipped prefixes every ledger line that says why a collect did not
// run: a skipped collect and a quiet one look alike in a ledger otherwise.
const collectSkipped = "collect skipped: "

// gc reclaims the registry's own space after the deletes result records, and
// sets Result.Collected when the collect ran. It returns the lines the ledger
// records: each reason it did not run, and the command's failure.
//
// Collect drops blobs no kept tag shares, so it runs only when a delete
// succeeded and no push is in flight. A guard gc cannot read — an unwired
// push reader, runner or container name — refuses the collect instead of
// reading as "no push, nothing waiting": a wrong answer there lets collect
// walk storage a push is writing. A failed collect is the same refusal to act
// on an unknown, so it is recorded and does not fail the apply.
//
// The caller's deadline is what ends a collect that never returns: it sets the
// container's timeout (see collectTimeout), so the collect is over before the
// deadline and docker exec returns on its own. A docker client that still has
// not answered at the deadline is abandoned and recorded a failure, so the
// retention returns and releases the plugin lock.
func (t *Target) gc(ctx context.Context, result *diskmaint.Result) []string {
	if len(result.Done) == 0 {
		return nil
	}
	switch {
	case t.cfg.PushInFlight == nil:
		return []string{collectSkipped + "no push-in-flight reader configured"}
	case t.cfg.PushInFlight():
		return []string{collectSkipped + "a push is in flight"}
	case t.cfg.Run == nil:
		return []string{collectSkipped + "no runner configured"}
	case !ValidContainer(t.cfg.Container):
		// The name is refused, not quoted: it is the one argument of the
		// command that is not fixed text.
		return []string{collectSkipped + "container name failed validation"}
	}
	if err := ctx.Err(); err != nil {
		return []string{"collect: the caller's context is done: " + err.Error()}
	}
	timeout, ok := collectTimeout(ctx)
	if !ok {
		return []string{"collect: the caller's deadline leaves no time to run it"}
	}
	env := []string{"DOCKER_HOST=" + t.cfg.DockerHost}
	err := awaitBound(ctx, func() error {
		// The command runs under a context the caller's deadline does not
		// cancel: a docker client killed at the deadline leaves the collect
		// running in the container, and the in-container timeout is what ends
		// a collect. What the deadline buys is the timeout's value.
		_, err := t.cfg.Run(context.WithoutCancel(ctx), env, "docker",
			"exec", t.cfg.Container, "timeout", timeout, "registry", "garbage-collect", "--delete-untagged", collectConfigPath)
		return err
	})
	if err != nil {
		return []string{"collect: " + err.Error()}
	}
	result.Collected = true
	return nil
}

// collectTimeout is the number of seconds the collect may run inside the
// container, and whether it may run at all.
//
// The caller's deadline is the bound: the container gets the time left on it
// less collectGrace, so its own timeout ends the collect before the deadline
// and docker exec returns on its own. A deadline less than collectGrace plus a
// whole second away refuses the collect, which the container would end at
// once. collectCeilingSeconds caps the value for a caller with no deadline or
// one that leaves the collect more time than the ceiling.
//
// The grace is the margin in which docker exec starts the collect, the
// container's timeout ends it and the client returns. Startup latency comes out
// of that margin, so a collect that hangs while its exec started more than the
// grace late can outlive the deadline by the excess; that is an accepted risk
// (overseer, 2026-10-29), since it takes a hung collect and a slow exec start
// together.
func collectTimeout(ctx context.Context) (timeout string, ok bool) {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return CollectTimeoutSeconds, true
	}
	seconds := int((time.Until(deadline) - collectGrace) / time.Second)
	if seconds < 1 {
		return "", false
	}
	if seconds > collectCeilingSeconds {
		seconds = collectCeilingSeconds
	}
	return strconv.Itoa(seconds), true
}

// awaitBound runs fn and returns its error, waiting no longer than the deadline
// on ctx. A ctx without a deadline sets no bound and fn is waited for.
//
// fn is not stopped when the bound arrives. The command's context is detached
// from the deadline (see gc) because killing the docker client leaves the
// collect running in the container; the deadline's bound is the container's
// (see collectTimeout). So the wait abandoned here is for a docker client that
// has not answered, after the collect it started has been ended by the
// container's timeout.
func awaitBound(ctx context.Context, fn func() error) error {
	if _, bounded := ctx.Deadline(); !bounded {
		return fn()
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("no answer by the caller's deadline: %w", ctx.Err())
	}
}
