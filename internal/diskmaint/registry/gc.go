package registry

import (
	"context"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// collectConfigPath is where the registry container mounts the configuration
// garbage-collect reads. The command is fixed text plus the validated
// container name, so nothing else a run reads can reach it.
const collectConfigPath = "/etc/docker/registry/config.yml"

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
	_, err := t.cfg.Run(ctx, env, "docker",
		"exec", t.cfg.Container, "registry", "garbage-collect", "--delete-untagged", collectConfigPath)
	if err != nil {
		return []string{"collect: " + err.Error()}
	}
	result.Collected = true
	return nil
}
