package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Town is the town a rehearsal run drives: the rig its commands act on and the
// runner that runs one command and returns its stdout. A test supplies its own
// runner, so no test starts a process.
type Town struct {
	Rig string
	// Run executes argv with the town root as its working directory and returns
	// stdout. A non-nil error means the command failed.
	Run func(ctx context.Context, argv []string) ([]byte, error)
}

// Create files task as a bead in the rig and returns the new bead's id. bd's
// --silent answer is the id alone. -C picks the rig; with no rig, the working
// directory does.
func (t Town) Create(ctx context.Context, task Task) (string, error) {
	argv := []string{"bd"}
	if t.Rig != "" {
		argv = append(argv, "-C", t.Rig)
	}
	argv = append(argv, "create", "--silent", "--title="+task.Title, "--description="+task.Body)
	out, err := t.Run(ctx, argv)
	if err != nil {
		return "", fmt.Errorf("bd create: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("bd create: its answer named no id")
	}
	return id, nil
}
