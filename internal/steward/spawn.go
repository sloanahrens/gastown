package steward

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/util"
)

// SpawnGit is the git surface AgentSpawner reads: *git.Git over the rig's
// repository in production, a fake in tests.
type SpawnGit interface {
	RefExists(ref string) (bool, error)
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
	WorktreeAddDetached(path, ref string) error
	WorktreeRemove(path string, force bool) error
	WorktreePrune() error
}

var _ SpawnGit = (*git.Git)(nil)

// AgentSpawner runs a job as a headless agent session in a throwaway
// worktree of the rig's repository, at the exact head the event names: the
// submitted commit, never whatever the branch holds by the time the job
// starts (the discipline Land uses, gt-9bioi).
type AgentSpawner struct {
	// TownRoot is the town the agent's environment is resolved against.
	TownRoot string
	// Rig is the rig the work belongs to.
	Rig string
	// Repo is the rig's repository (.repo.git).
	Repo string
	// Role is the GT_ROLE the job runs as; "" means "<rig>/steward".
	Role string
	// Logf receives the spawner's own progress lines.
	Logf func(format string, args ...any)

	// OpenGit opens the rig repository; nil is git.NewGit.
	OpenGit func(repo string) SpawnGit
	// ResolveAgent resolves a preset name to what launches it; nil is
	// config.ResolveAgentConfigWithOverride against the town.
	ResolveAgent func(model string) (*agentconfig.RuntimeConfig, error)
	// Run executes the job's command in dir; nil runs it for real, in its own
	// process group so the job's timeout kills the whole tree.
	Run func(ctx context.Context, cmd *exec.Cmd) error

	// FetchTimeout bounds the fetch that makes the head present.
	FetchTimeout time.Duration
}

// Spawn creates the job's worktree, runs the agent there, records the
// verdict, and removes the worktree: the job's whole footprint.
func (s *AgentSpawner) Spawn(ctx context.Context, req SpawnRequest) SpawnResult {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultJobTimeout
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	g := s.openGit()
	if err := s.worktreeAt(g, req); err != nil {
		return SpawnResult{ExitCode: -1, Err: err}
	}
	// The worktree goes away however the run ends: its branch is on origin,
	// and the ledger's transcript path is the durable record of the session.
	defer func() {
		if err := g.WorktreeRemove(req.Dir, true); err != nil {
			s.logf("steward: %s: removing the worktree %s: %v", req.ID, req.Dir, err)
		}
		_ = g.WorktreePrune()
	}()

	cfg, err := s.resolveAgent(req.Model)
	if err != nil {
		return SpawnResult{ExitCode: -1, Err: fmt.Errorf("resolving agent %q: %w", req.Model, err)}
	}
	cmd := exec.CommandContext(jctx, cfg.Command, append(append([]string{}, cfg.Args...), "-p", req.Prompt)...) //nolint:gosec // G204: the agent command and preset args come from the town's own config
	cmd.Dir = req.Dir
	cmd.Env = s.env(cfg, req)

	run := s.Run
	if run == nil {
		util.SetProcessGroup(cmd)
		run = func(_ context.Context, c *exec.Cmd) error { return c.Run() }
	}
	runErr := run(jctx, cmd)
	timedOut := jctx.Err() != nil && ctx.Err() == nil

	transcript, _ := agentlog.LatestTranscript(req.Dir)
	res := SpawnResult{Transcript: transcript}
	switch {
	case runErr != nil:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
		}
		if timedOut {
			res.TimedOut = true
		} else {
			res.Err = runErr
		}
	}
	verdict, verdictErr := ReadVerdict(filepath.Join(req.Dir, ResultFile))
	if verdictErr != nil {
		res.VerdictErr = fmt.Errorf("%w (exit %d)", verdictErr, res.ExitCode)
	} else {
		res.Verdict = &verdict
	}
	return res
}

// worktreeAt puts req.Event.Head in a detached worktree at req.Dir, fetching
// the branch when the commit is not already in the repository: a direct push
// is only on origin until something fetches it (gt-p2rs0).
func (s *AgentSpawner) worktreeAt(g SpawnGit, req SpawnRequest) error {
	if req.Event.Head == "" {
		return fmt.Errorf("event for %s names no head", req.Event.Bead)
	}
	if err := os.MkdirAll(filepath.Dir(req.Dir), 0o700); err != nil {
		return err
	}
	have, err := g.RefExists(req.Event.Head + "^{commit}")
	if err != nil {
		return fmt.Errorf("reading %s in the rig repository: %w", req.Event.Head, err)
	}
	if !have {
		if req.Event.Branch == "" {
			return fmt.Errorf("%s is not in the rig repository and the event names no branch to fetch", req.Event.Head)
		}
		timeout := s.FetchTimeout
		if timeout <= 0 {
			timeout = DefaultFetchTimeout
		}
		refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", req.Event.Branch, req.Event.Branch)
		if err := g.FetchRefspecWithTimeout("origin", refspec, timeout); err != nil {
			return fmt.Errorf("fetching %s for %s: %w", req.Event.Branch, req.Event.Head, err)
		}
		if have, err := g.RefExists(req.Event.Head + "^{commit}"); err != nil {
			return fmt.Errorf("reading %s after fetching: %w", req.Event.Head, err)
		} else if !have {
			return fmt.Errorf("%s is not on origin/%s after fetching it", req.Event.Head, req.Event.Branch)
		}
	}
	if err := g.WorktreeAddDetached(req.Dir, req.Event.Head); err != nil {
		return fmt.Errorf("worktree at %s: %w", req.Event.Head, err)
	}
	return nil
}

// DefaultFetchTimeout bounds the fetch that makes a submitted head present.
const DefaultFetchTimeout = 2 * time.Minute

// resolveAgent resolves a preset name, defaulting to the town's own config.
func (s *AgentSpawner) resolveAgent(model string) (*agentconfig.RuntimeConfig, error) {
	if s.ResolveAgent != nil {
		return s.ResolveAgent(model)
	}
	rigPath := filepath.Join(s.TownRoot, s.Rig)
	rc, _, err := agentconfig.ResolveAgentConfigWithOverride(s.TownRoot, rigPath, model)
	if err != nil {
		return nil, err
	}
	return rc, nil
}

// env is what the job's process reads: the standard agent environment for
// its seat, then the preset's own variables, with daemon.env references
// resolved (a job is a direct exec, so nothing else would resolve them).
func (s *AgentSpawner) env(cfg *agentconfig.RuntimeConfig, req SpawnRequest) []string {
	role := s.Role
	if role == "" {
		role = s.Rig + "/steward"
	}
	vmap := agentconfig.AgentEnv(agentconfig.AgentEnvConfig{
		Role: role, Rig: s.Rig, AgentName: "steward", TownRoot: s.TownRoot, Agent: req.Model,
	})
	vmap["GT_ROLE"] = role
	vmap["GT_RIG"] = s.Rig
	vmap["GT_STEWARD_JOB"] = req.ID
	vmap["GT_STEWARD_BEAD"] = req.Event.Bead
	// Values() resolves now, which is what a direct exec needs: a startup
	// command that reads daemon.env at run time is a shell's trick, and no
	// shell is in this path (gt-y3pgh.5).
	if se, err := agentconfig.ResolveSpawnEnv(s.TownRoot, cfg.Env); err != nil {
		s.logf("steward: %s: resolving the preset's env: %v", req.ID, err)
	} else {
		for k, v := range se.Values() {
			vmap[k] = v
		}
	}
	return mergeEnv(os.Environ(), vmap)
}

// mergeEnv is base with set applied: an inherited variable the job sets is
// dropped, not shadowed, because with a duplicate key which value a child
// sees depends on its libc.
func mergeEnv(base []string, set map[string]string) []string {
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; !ok {
			out = append(out, kv)
		}
	}
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	return out
}

func (s *AgentSpawner) openGit() SpawnGit {
	if s.OpenGit != nil {
		return s.OpenGit(s.Repo)
	}
	return git.NewGit(s.Repo)
}

func (s *AgentSpawner) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// PruneJobDirs removes job worktrees older than retention, and the repository's
// registration of them. A job's worktree outlives the job only when its
// removal failed, so this is the cleanup of last resort (gt-9bioi.1).
func PruneJobDirs(g SpawnGit, root string, retention time.Duration, now time.Time) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // no job dirs is not a failure
	}
	if err != nil {
		return fmt.Errorf("reading the job dirs: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "steward-") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < retention {
			continue
		}
		// The removal is by force, and the directory goes even if git refused
		// it: this is the cleanup of last resort, and the daemon's logs name
		// what the job that owned it did before it died (gt-9bioi.1).
		_ = g.WorktreeRemove(filepath.Join(root, e.Name()), true)
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
	return g.WorktreePrune()
}
