package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/workspace"
)

// daemonComposeFlags are the values `gt daemon compose write` parsed.
type daemonComposeFlags struct {
	image string // gt-daemon:<rev>, the image the rendered file names
	print bool   // print the render and write no file
}

// daemonComposeDeps is what the command takes from the host: where the town is
// found, its daemon settings, the Dolt endpoint its container reaches, the
// filesystem seam the render is written through, and where output goes. A test
// supplies fakes, so no unit test reads a live town or writes its file.
type daemonComposeDeps struct {
	findTown     func() (string, error)
	daemonConfig func(townRoot string) (*config.DaemonPatrolConfig, error)
	// doltHost is the host the daemon in the container reaches the town's Dolt
	// through, and whether the town names an endpoint at all.
	doltHost func(townRoot string) (string, bool)
	write    func(path string, content []byte) error
	out      io.Writer
}

// realDaemonComposeDeps is the host's.
func realDaemonComposeDeps(out io.Writer) daemonComposeDeps {
	return daemonComposeDeps{
		findTown: workspace.FindFromCwdOrError,
		// The settings are read through the control the supervisor and the
		// cutover refuse from, so this command refuses the same towns they do.
		daemonConfig: realDaemonControl().townDaemonConfig,
		// The process asking is not the container, but the daemon this file
		// starts is: container mode is what names the host it reaches Dolt
		// through (gt-qpeei.17).
		doltHost: func(townRoot string) (string, bool) {
			ep, ok := config.ResolveDoltEndpointFor(townRoot, true)
			return ep.Host, ok
		},
		write: writeDaemonComposeFile,
		out:   out,
	}
}

// writeDaemonComposeFile writes content to path, making the compose file's
// directory first: it lives under the town's daemon/ directory, which a town
// that has never rendered it does not have.
func writeDaemonComposeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return daemonimage.WriteComposeFile(path, content)
}

// newDaemonComposeCmd builds `gt daemon compose` over run, which receives the
// flags `write` parsed.
func newDaemonComposeCmd(run func(out io.Writer, f daemonComposeFlags) error) *cobra.Command {
	var f daemonComposeFlags
	parent := &cobra.Command{
		Use:   "compose",
		Short: "Render the town's live daemon compose file",
		Long: `Render the compose file the town's daemon container runs from, at
<town>/daemon/container/compose.yml.

gt daemon cutover --to container starts the container from this file, so it must
name the town's own container, compose project and image. The file is rendered
from the town's settings, never by hand.`,
		RunE: requireSubcommand,
	}
	write := &cobra.Command{
		Use:   "write --image gt-daemon:<rev>",
		Short: "Write the live compose file and start nothing",
		Long: `Write the live compose file from the town in the working directory and start
nothing. The town must set both daemon.container_name and daemon.compose_project:
a field left unset would render the live defaults into a file this town's
cutover then starts.

Dolt is resolved as the container reaches it: dolt.container_host when the town
sets it, dolt.host when it does not, and a loopback or empty host is refused,
because inside the container that host is the container itself.

--print prints the render and writes no file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.OutOrStdout(), f)
		},
	}
	write.Flags().StringVar(&f.image, "image", "", "daemon image tag, gt-daemon:<rev>")
	write.Flags().BoolVar(&f.print, "print", false, "print the render and write no file")
	parent.AddCommand(write)
	return parent
}

var daemonComposeCmd = newDaemonComposeCmd(func(out io.Writer, f daemonComposeFlags) error {
	return runDaemonCompose(f, realDaemonComposeDeps(out))
})

func init() {
	daemonCmd.AddCommand(daemonComposeCmd)
}

// runDaemonCompose renders the town's live compose file and, unless --print,
// writes it. Every refusal comes before the file is written, so a town the
// render or the settings refuse keeps the file it had.
func runDaemonCompose(f daemonComposeFlags, d daemonComposeDeps) error {
	image := strings.TrimSpace(f.image)
	if _, err := daemonimage.ImageTagRev(image); err != nil {
		return err
	}
	townRoot, err := d.findTown()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	cfg, err := d.daemonConfig(townRoot)
	if err != nil {
		return fmt.Errorf("reading the town's daemon settings: %w", err)
	}
	project, err := containerComposeProject(cfg)
	if err != nil {
		return err
	}
	host, ok := d.doltHost(townRoot)
	if !ok {
		return fmt.Errorf("the town's Dolt endpoint cannot be resolved, so the daemon in the container has no host to reach it on: set mayor/town.json's dolt.container_host (or dolt.host)")
	}
	spec := templates.ComposeSpec{
		Project:       project,
		ContainerName: cfg.ContainerName(),
		Image:         image,
		TownRoot:      townRoot,
		StopGrace:     templates.MinimumStopGrace,
		DoltHost:      host,
	}
	content, err := templates.RenderDaemonCompose(spec)
	if err != nil {
		return fmt.Errorf("cannot render the live compose file, and no file was written: %w", err)
	}
	if f.print {
		_, err = d.out.Write(content)
		return err
	}
	path := config.ContainerComposeFile(townRoot)
	if err := d.write(path, content); err != nil {
		return fmt.Errorf("cannot write %s, and the old file is as it was: %w", path, err)
	}
	fmt.Fprintf(d.out, "%s Wrote %s: container %s, compose project %s, image %s.\n",
		style.Bold.Render("✓"), path, spec.ContainerName, project, spec.Image)
	return nil
}
