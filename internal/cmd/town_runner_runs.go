package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/runnerroll"
)

// rollRunWorkflows are the workflows the roll waits on: the gate and post-land.
var rollRunWorkflows = []string{"gate.yml", "post-land.yml"}

// newRollRunsRead builds the roll's runs read over the owner/name repos
// rigForgejoRepos returns. A nil client, an empty list or a malformed owner/name
// is an error, never a read with no runs: each value reaches a request path, and
// a roll must not replace a runner it could not look at.
func newRollRunsRead(client runnerroll.ForgejoRunLister, repos []string) (func(context.Context) ([]runnerroll.WorkflowRun, error), error) {
	if client == nil {
		return nil, errors.New("the roll's runs read needs a Forgejo client")
	}
	if len(repos) == 0 {
		return nil, errors.New("the roll's runs read needs at least one repository")
	}
	refs := make([]runnerroll.RepoRef, 0, len(repos))
	for _, repo := range repos {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") ||
			owner == "." || owner == ".." || name == "." || name == ".." {
			return nil, fmt.Errorf("repository %q is not owner/name", repo)
		}
		refs = append(refs, runnerroll.RepoRef{Owner: owner, Name: name})
	}
	return runnerroll.ForgejoRuns(client, refs, rollRunWorkflows), nil
}
