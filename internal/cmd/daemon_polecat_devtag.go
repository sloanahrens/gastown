package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/workspace"
)

// polecatDevTagCommand is the gt invocation the pod pair check runs in the
// daemon container to read the dev tag. Only the daemon pod reaches the
// sandbox registry, so the check asks the daemon rather than reading it from
// the Job pod (gt-5yh32c).
var polecatDevTagCommand = []string{"gt", "daemon", "polecat-devtag"}

// polecatDevTagRead is what the command prints on its one line: whether the
// sandbox registry holds the dev tag and, when it does, the digest it names.
type polecatDevTagRead struct {
	Present bool   `json:"present"`
	Digest  string `json:"digest,omitempty"`
}

// daemonPolecatDevTagSeams are the subcommand's dependencies: the town lookup,
// the settings read and the registry client. A test supplies a fixture town and
// an in-process registry, so no test reaches the host.
type daemonPolecatDevTagSeams struct {
	FindTownRoot func() (string, error)
	LoadPatrol   func(townRoot string) (*config.DaemonPatrolConfig, error)
	NewClient    func(base string) *registry.Client
}

// productionDaemonPolecatDevTagSeams reads the daemon's own town and settings,
// and talks to the registry over the client's own HTTP client.
func productionDaemonPolecatDevTagSeams() daemonPolecatDevTagSeams {
	return daemonPolecatDevTagSeams{
		FindTownRoot: workspace.FindFromCwdOrError,
		LoadPatrol:   loadUpdatePatrolConfig,
		NewClient:    func(base string) *registry.Client { return &registry.Client{Base: base} },
	}
}

// newDaemonPolecatDevTagCmd builds `gt daemon polecat-devtag` over seams.
func newDaemonPolecatDevTagCmd(seams daemonPolecatDevTagSeams) *cobra.Command {
	return &cobra.Command{
		Use:   "polecat-devtag",
		Short: "Print the digest the sandbox registry's dev tag of the polecat image names",
		Long: `Print the digest the sandbox registry's dev tag of the polecat image names.

The registry is the daemon's patrols.tier_sweep.runner.image_registry, the
setting the daemon copies the dev tag to at start. The command runs inside the
daemon pod, which is the only pod that reaches that registry; gt update's pair
check execs it there (gt-5yh32c).

The answer is one line of JSON: {"present":true,"digest":"sha256:..."} for a
tag the registry holds, and {"present":false} for a tag it does not. A registry
that cannot be read is an error, never an absent tag.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonPolecatDevTag(cmd, seams)
		},
	}
}

// runDaemonPolecatDevTag reads the dev tag from the registry the daemon
// publishes it to and prints the answer.
func runDaemonPolecatDevTag(cmd *cobra.Command, seams daemonPolecatDevTagSeams) error {
	if err := validateDaemonPolecatDevTagSeams(seams); err != nil {
		return err
	}
	townRoot, err := seams.FindTownRoot()
	if err != nil {
		return fmt.Errorf("gt daemon polecat-devtag: finding the town root: %w", err)
	}
	patrol, err := seams.LoadPatrol(townRoot)
	if err != nil {
		return fmt.Errorf("gt daemon polecat-devtag: reading the daemon settings: %w", err)
	}
	host := polecatDevTagRegistry(patrol)
	if host == "" {
		return errors.New("gt daemon polecat-devtag: the daemon settings name no patrols.tier_sweep.runner.image_registry, so the daemon publishes no dev tag")
	}
	read, err := readPolecatDevTag(cmd.Context(), seams.NewClient(config.ImageRegistryBase(host)))
	if err != nil {
		return fmt.Errorf("gt daemon polecat-devtag: reading %s:%s in %s: %w", imagevalues.PolecatImage, podPolecatTag, host, err)
	}
	out, err := json.Marshal(read)
	if err != nil {
		return fmt.Errorf("gt daemon polecat-devtag: encoding the answer: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}

// polecatDevTagRegistry is the registry the daemon publishes the dev tag to:
// the runner's image_registry setting, the one copyPolecatDevTag reads. A town
// with no runner block publishes none, so it answers "".
func polecatDevTagRegistry(patrol *config.DaemonPatrolConfig) string {
	if patrol == nil || patrol.Patrols == nil || patrol.Patrols.TierSweep == nil || patrol.Patrols.TierSweep.Runner == nil {
		return ""
	}
	return patrol.Patrols.TierSweep.Runner.ImageRegistry
}

// readPolecatDevTag inspects the dev tag for the platform the sandbox pulls.
// A tag the registry does not hold is absent, not an error: the daemon may not
// have copied it yet.
func readPolecatDevTag(ctx context.Context, c *registry.Client) (polecatDevTagRead, error) {
	info, err := c.InspectImage(ctx, imagevalues.PolecatImage, podPolecatTag, updateImagesOS, updateImagesArch)
	if errors.Is(err, registry.ErrNotFound) {
		return polecatDevTagRead{}, nil
	}
	if err != nil {
		return polecatDevTagRead{}, err
	}
	return polecatDevTagRead{Present: true, Digest: info.ManifestDigest}, nil
}

// validateDaemonPolecatDevTagSeams refuses a command that would report a read
// it never made.
func validateDaemonPolecatDevTagSeams(s daemonPolecatDevTagSeams) error {
	switch {
	case s.FindTownRoot == nil:
		return errors.New("gt daemon polecat-devtag: no town lookup is wired")
	case s.LoadPatrol == nil:
		return errors.New("gt daemon polecat-devtag: no settings reader is wired")
	case s.NewClient == nil:
		return errors.New("gt daemon polecat-devtag: no registry client is wired")
	}
	return nil
}
