package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// townDeployerServeEnv is what `gt town deployer serve` runs on: the flags' values
// and the seams, so a test drives every refusal with its own steps.
type townDeployerServeEnv struct {
	once    bool
	inbox   string
	answers string
	state   string
	steps   deployerinbox.Steps
	now     func() time.Time
	out     io.Writer
}

// runTownDeployerServeIn is what `gt town deployer serve` runs. Every refusal
// comes before a step runs, so a refused run changes nothing.
func runTownDeployerServeIn(ctx context.Context, env townDeployerServeEnv) error {
	if !env.once {
		return errors.New("gt town deployer serve needs --once: it answers one request and exits, and no watch loop is built")
	}
	if env.steps == nil {
		return errors.New("gt town deployer serve has no plan, apply or rollback steps wired, so it answers no request")
	}
	done := filepath.Join(env.state, "done")
	info, err := os.Stat(done)
	if err != nil {
		return fmt.Errorf("gt town deployer serve needs the directory %s to move a request into: %w", done, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("gt town deployer serve needs %s to be a directory to move a request into", done)
	}

	// os.ReadDir returns the entries sorted by name, and a request's name is
	// its UTC stamp, so the first entry is the oldest.
	entries, err := os.ReadDir(env.inbox)
	if err != nil {
		return fmt.Errorf("reading the inbox %s: %w", env.inbox, err)
	}
	if len(entries) == 0 {
		return nil
	}
	name := entries[0].Name()
	path := filepath.Join(env.inbox, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the request %s: %w", path, err)
	}
	req, err := deployerinbox.ParseRequest(name, data)
	if err != nil {
		return fmt.Errorf("the oldest request is not valid, so it is left in the inbox: %w", err)
	}

	answer := deployerinbox.Handle(ctx, env.steps, req, env.now())
	if err := deployerinbox.WriteAnswer(deployerinbox.OSAnswerFS{}, env.answers, answer); err != nil {
		return fmt.Errorf("writing the answer to the request %s: %w", req.ID, err)
	}
	if err := os.Rename(path, filepath.Join(done, name)); err != nil {
		return fmt.Errorf("moving the request %s into %s: %w", path, done, err)
	}
	if _, err := fmt.Fprintf(env.out, "id: %s\nstatus: %s\nanswer: %s\n", req.ID, answer.Status, filepath.Join(env.answers, req.ID+".json")); err != nil {
		return fmt.Errorf("the request %s is answered, but printing its status failed: %w", req.ID, err)
	}
	return nil
}
