package registry

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// collectConfigPath is where the registry container mounts the configuration
// garbage-collect reads. The command is fixed text plus the validated
// container name, so nothing else a run reads can reach it.
const collectConfigPath = "/etc/docker/registry/config.yml"

// CollectTimeoutSeconds is how long the collect may run inside the registry
// container before the container's own timeout ends it. The collect is run
// under `timeout` in the container, not bounded by killing the docker client:
// killing the client leaves the collect running after the plugin lock is
// released, where it can race a push, while a collect ended in the container
// ends for good and lets docker exec return on its own, so the lock is held at
// most this long (gt-dl4nv.13). Thirty minutes is generous for a registry of
// tens of gigabytes; a collect that needs longer is recorded as a failure and
// the next run picks it up.
const CollectTimeoutSeconds = "1800"

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
// A collect that has not answered by the caller's deadline is recorded
// unanswered and the apply returns, so a docker exec nobody hears from cannot
// hold the plugin lock past the deadline the caller set (gt-dl4nv.13).
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
	env := []string{"DOCKER_HOST=" + t.cfg.DockerHost}
	err := awaitBound(ctx, func() error {
		_, err := t.cfg.Run(ctx, env, "docker",
			"exec", t.cfg.Container, "timeout", CollectTimeoutSeconds, "registry", "garbage-collect", "--delete-untagged", collectConfigPath)
		return err
	})
	if err != nil {
		return []string{"collect: " + err.Error()}
	}
	result.Collected = true
	return nil
}

// awaitBound runs fn and returns its error, waiting no longer than the deadline
// on ctx. A ctx without a deadline sets no bound and fn is waited for, the
// container's timeout then being the collect's only bound.
//
// fn is not stopped when the bound arrives: killing the docker client leaves
// the collect running in the container, so the container's own timeout is what
// ends a collect, and ending it there ends it for good (gt-dl4nv.13).
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
		return fmt.Errorf("no answer by the bound: %w", ctx.Err())
	}
}
