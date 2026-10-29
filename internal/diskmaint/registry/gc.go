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

// CollectTimeoutSeconds is how long the collect may run inside the registry
// container when the caller sets no deadline of its own. The collect is run
// under `timeout` in the container, not bounded by killing the docker client:
// killing the client leaves the collect running after the plugin lock is
// released, where it can race a push, while a collect ended in the container
// ends for good and lets docker exec return on its own, so the lock is held at
// most this long (gt-dl4nv.13). Thirty minutes is generous for a registry of
// tens of gigabytes; a collect that needs longer is recorded as a failure and
// the next run picks it up.
const CollectTimeoutSeconds = "1800"

// collectGrace is the time between the container's timeout ending the collect
// and the caller's deadline. It is the margin in which docker exec notices the
// collect has ended and returns, so the collect is over and the plugin lock is
// still held when it goes back (gt-dl4nv.13).
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
// The caller's deadline is the bound on a collect that never returns. The
// container enforces it, so the collect ends before the deadline rather than
// the docker client being killed at it: see collectTimeout. A client that
// still does not answer by the deadline is abandoned and recorded a failure,
// which leaves nothing running — the collect it started has already ended.
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
// and docker exec returns while the plugin lock is still held. A deadline
// already less than collectGrace plus one whole second away refuses the
// collect — starting one the container ends at once buys nothing — and a
// context with no deadline leaves CollectTimeoutSeconds as the only bound.
func collectTimeout(ctx context.Context) (timeout string, ok bool) {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return CollectTimeoutSeconds, true
	}
	seconds := int((time.Until(deadline) - collectGrace) / time.Second)
	if seconds < 1 {
		return "", false
	}
	return strconv.Itoa(seconds), true
}

// awaitBound runs fn and returns its error, waiting no longer than the deadline
// on ctx. A ctx without a deadline sets no bound and fn is waited for.
//
// fn is not stopped when the bound arrives. The command's context is detached
// from the deadline (see gc) because killing the docker client leaves the
// collect running in the container; the deadline's bound is the container's
// (see collectTimeout). So what is abandoned here is a docker client that never
// answered, after the collect it started has been ended.
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
