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
// directory does. The body's ## Acceptance section goes to --acceptance, and a
// task whose section is empty is refused before bd runs.
func (t Town) Create(ctx context.Context, task Task) (string, error) {
	acceptance := acceptanceOf(task.Body)
	if acceptance == "" {
		return "", fmt.Errorf("bd create %q: the body's ## Acceptance section is empty", task.Title)
	}
	argv := []string{"bd"}
	if t.Rig != "" {
		argv = append(argv, "-C", t.Rig)
	}
	argv = append(argv, "create", "--silent", "--title="+task.Title, "--description="+task.Body, "--acceptance="+acceptance)
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

// acceptanceOf returns the lines under the body's level 2 "## Acceptance"
// heading, up to the next level 2 heading or the end, trimmed. It is empty when
// the body has no such heading or the section has no text.
func acceptanceOf(body string) string {
	var rows []string
	inSection := false
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimRight(line, " \t\r") == "## Acceptance" {
			inSection = true
			continue
		}
		if inSection && strings.HasPrefix(line, "## ") {
			break
		}
		if inSection {
			rows = append(rows, line)
		}
	}
	return strings.TrimSpace(strings.Join(rows, "\n"))
}
