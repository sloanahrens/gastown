package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

// newUpdateRollbackCmd builds `gt update rollback` over seams.
func newUpdateRollbackCmd(seams updateSeams) *cobra.Command {
	var podContext, podNamespace string
	var podInCluster bool
	c := &cobra.Command{
		Use:   "rollback",
		Short: "Put the daemon back on the image pair its last digest install replaced",
		Long: `Put the daemon back on the image pair its last digest install replaced.

The pair is the one gt update --images recorded before it moved the daemon. The
daemon and its polecat image go back together, and the daemon is recreated on
them and must come back on their commit within the restart bound.

--pod-context and --pod-namespace, given together, name the Deployment of a
daemon that runs as a pod: the pair is restored to that Deployment, and the
daemon's build is read from its pod rather than the state file.
--pod-in-cluster with --pod-namespace names the same Deployment with no context.

The command refuses, and changes nothing, when no digest install has been
recorded, when the recorded pair is the one the daemon already runs, and when
the registry does not hold a digest of the pair. A restore that fails escalates.

Exit status: 0 when the daemon runs the restored pair, 1 when a refusal or the
restore failed, 5 when another update run holds the lock.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdateRollback(cmd, seams, podContext, podNamespace, podInCluster)
		},
	}
	c.Flags().StringVar(&podContext, "pod-context", "", podContextUsage)
	c.Flags().StringVar(&podNamespace, "pod-namespace", "", podNamespaceUsage)
	c.Flags().BoolVar(&podInCluster, "pod-in-cluster", false, podInClusterUsage)
	return c
}

// runUpdateRollback restores the recorded pair under the update lock, so a
// rollback never moves the daemon while a run is installing. The pod flags name
// the Deployment the pair is restored to, as they do for gt update --images.
func runUpdateRollback(cmd *cobra.Command, seams updateSeams, podContext, podNamespace string, podInCluster bool) error {
	errOut := cmd.ErrOrStderr()
	pod, err := podDaemonFromFlags(podContext, podNamespace, podInCluster)
	if err != nil {
		return fmt.Errorf("gt update rollback: %w", err)
	}
	townRoot, err := seams.FindTownRoot()
	if err != nil {
		return fmt.Errorf("gt update rollback: finding the town root: %w", err)
	}
	deps, err := seams.DepsFactory(townRoot)
	if err != nil {
		return fmt.Errorf("gt update rollback: preparing: %w", err)
	}
	deps, err = podDaemonDeps(seams, deps, pod)
	if err != nil {
		return fmt.Errorf("gt update rollback: preparing the pod daemon: %w", err)
	}
	release, err := update.AcquireLock(update.Dir(townRoot))
	if errors.Is(err, update.ErrLocked) {
		fmt.Fprintln(errOut, "gt update rollback: another update run holds the lock; nothing changed")
		return NewSilentExit(5)
	}
	if err != nil {
		return fmt.Errorf("gt update rollback: taking the update lock: %w", err)
	}
	defer release()

	images, err := imageDeps(seams, townRoot, deps, pod)
	if err != nil {
		return fmt.Errorf("gt update rollback: %w", err)
	}
	check, err := imagePairRegistryCheck(seams.Images, townRoot)
	if err != nil {
		return fmt.Errorf("gt update rollback: %w", err)
	}
	prev, err := update.Rollback(cmd.Context(), images, check)
	if err != nil {
		return fmt.Errorf("gt update rollback: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "rolled the daemon back to %s and polecat image %s at commit %s\n", prev.Daemon, prev.Polecat, prev.Commit)
	return nil
}

// imagePairRegistryCheck returns the check a rollback runs before it moves the
// daemon. Each digest of the pair must name the town's own image in the town's
// registry, and the registry must still hold that digest with bytes that hash
// to it. The viewer credential is read here and never reaches an error.
func imagePairRegistryCheck(seams updateImagesSeams, townRoot string) (func(context.Context, update.ImagePair) error, error) {
	switch {
	case seams.Registry == nil:
		return nil, errors.New("no registry is wired")
	case seams.Credentials == nil:
		return nil, errors.New("no credential reader is wired")
	case seams.HTTPClient == nil:
		return nil, errors.New("no HTTP client is wired")
	}
	registry, err := seams.Registry(townRoot)
	if err != nil {
		return nil, err
	}
	if err := registry.Validate(); err != nil {
		return nil, err
	}
	user, token, err := seams.Credentials()
	if err != nil {
		return nil, fmt.Errorf("reading the viewer credential: %w", err)
	}
	reader := registryReader{
		base:          registry.BaseURL(),
		registry:      registry,
		plaintextHost: plaintextHostOf(registry),
		http:          seams.HTTPClient,
		user:          user,
		token:         token,
	}
	return func(ctx context.Context, pair update.ImagePair) error {
		for _, img := range []struct {
			kind, name, ref string
		}{
			{kind: "daemon", name: imagevalues.DaemonImage, ref: pair.Daemon},
			{kind: "polecat", name: imagevalues.PolecatImage, ref: pair.Polecat},
		} {
			_, digest, ok := strings.Cut(img.ref, "@")
			if !ok || registry.Ref(img.name, digest) != img.ref {
				return fmt.Errorf("the recorded %s image %q is not a digest of the town's %s image", img.kind, img.ref, img.name)
			}
			if err := reader.digestAt(ctx, img.kind, img.name, digest); err != nil {
				return err
			}
		}
		return nil
	}, nil
}
