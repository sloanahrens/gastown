package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/beads"
)

// rigAddWorkspace is the gateway half of gt rig add (docs/design/bd-container.md,
// task G8b). With a bd gateway configured, the new rig's database is initialised
// through bd.real in a temporary workspace, and then the town copy is pushed so the
// rig is usable through the gateway from the moment it exists. Without a gateway
// it does nothing: no temporary workspace, no bd.real, no push.
type rigAddWorkspace struct {
	// gatewayURL is BD_GATEWAY_URL. Empty means no gateway is configured.
	gatewayURL string
	// push sends the town copy to the gateway and returns the manifest hash it stored.
	push func(ctx context.Context, townRoot string) (string, error)
	// bdReal runs bd.real init with argv in dir.
	bdReal func(dir string, argv []string) error
	// tempDir makes the workspace bd.real initialises in; removeDir takes it back.
	tempDir   func() (string, error)
	removeDir func(path string) error
}

// processRigAddWorkspace is what gt rig add reads on the real machine: the gateway
// from the environment, bd.real beside the running gt, and the OS temp directory.
func processRigAddWorkspace(getenv func(string) string) rigAddWorkspace {
	url := getenv(bdgateway.WorkspaceURLEnv)
	pusher := bdgateway.GatewayPusher{URL: url, TokenFile: getenv(bdgateway.WorkspaceTokenEnv)}
	return rigAddWorkspace{
		gatewayURL: url,
		push:       pusher.Push,
		bdReal:     runBdRealInit,
		tempDir: func() (string, error) {
			return os.MkdirTemp("", "gt-rig-workspace-")
		},
		removeDir: os.RemoveAll,
	}
}

// run initialises the rig's database through bd.real in a temporary workspace and
// then pushes the town copy. The init runs first, so the database exists before the
// gateway is sent the metadata that names it; the workspace bd writes stays in the
// temporary directory, and the database itself lives in Dolt. A failure is returned,
// naming the rig and the step, and the caller reports it; the rig add itself is not
// undone, and the daemon's reconcile converges a failed push.
func (w rigAddWorkspace) run(ctx context.Context, townRoot, rig, prefix string) (err error) {
	if w.gatewayURL == "" {
		return nil
	}
	if w.push == nil || w.bdReal == nil || w.tempDir == nil || w.removeDir == nil {
		return fmt.Errorf("rig %s: the bd gateway step has no %s wired; the workspace was not initialised or pushed", rig, unwiredRigAddStep(w))
	}
	dir, err := w.tempDir()
	if err != nil {
		return fmt.Errorf("rig %s: making the temporary workspace for bd.real init: %w", rig, err)
	}
	defer func() {
		if rmErr := w.removeDir(dir); rmErr != nil && err == nil {
			err = fmt.Errorf("rig %s: removing the temporary workspace %s: %w", rig, dir, rmErr)
		}
	}()
	if err := w.bdReal(dir, rigInitArgv(rig, prefix)); err != nil {
		return fmt.Errorf("rig %s: bd.real init in the temporary workspace: %w", rig, err)
	}
	if _, err := w.push(ctx, townRoot); err != nil {
		return fmt.Errorf("rig %s: pushing the workspace to the bd gateway: %w; the daemon's five-minute reconcile retries the push", rig, err)
	}
	return nil
}

// unwiredRigAddStep names the first nil seam, so the refusal says what is missing.
func unwiredRigAddStep(w rigAddWorkspace) string {
	switch {
	case w.push == nil:
		return "pusher"
	case w.bdReal == nil:
		return "bd.real runner"
	case w.tempDir == nil:
		return "temp directory"
	default:
		return "temp directory remover"
	}
}

// rigInitArgv is bd init for one rig's database, with the flags the local init in
// InitBeads uses (no server port: bd picks its default).
func rigInitArgv(rig, prefix string) []string {
	argv := []string{"init"}
	if prefix != "" {
		argv = append(argv, "--prefix", prefix)
	}
	return append(argv, "--database", rig, "--server", "--force", "--skip-agents")
}

// rigInitBdRealTimeout bounds one bd.real init, so a hung init cannot hold up gt rig add.
const rigInitBdRealTimeout = 5 * time.Minute

// runBdRealInit runs bd.real beside the running gt binary, in dir, with BEADS_DIR
// pinned to dir/.beads. It opts in to creating the database, which bd refuses
// otherwise on a server that already carries databases (gt-89d3p). bd's output goes
// to the terminal; the error names the command and never carries that output.
func runBdRealInit(dir string, argv []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding gt to locate bd.real: %w", err)
	}
	env := beads.StripEnvKey(os.Environ(), "BEADS_DIR")
	env = beads.StripEnvKey(env, "BD_ALLOW_NEW_DATABASE")
	env = append(env, "BEADS_DIR="+filepath.Join(dir, ".beads"), "BD_ALLOW_NEW_DATABASE=1")
	ctx, cancel := context.WithTimeout(context.Background(), rigInitBdRealTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(filepath.Dir(exe), "bd.real"), argv...) //nolint:gosec // bd.real beside this binary, argv is built here
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bd.real %s: %w", argv[0], err)
	}
	return nil
}
