package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

// doltContainerPlanDeps is the command's three filesystem lookups: the default
// mount paths and the config read. A test supplies fakes, so it never reads
// the live town tree or HOME, nor the real config file.
type doltContainerPlanDeps struct {
	townRoot   func() (string, error)
	backupRoot func() (string, error)
	readFile   func(path string) ([]byte, error)
}

// doltContainerPlanRealDeps is doltContainerPlanDeps on the real machine.
func doltContainerPlanRealDeps() doltContainerPlanDeps {
	return doltContainerPlanDeps{
		townRoot:   workspace.FindFromCwdOrError,
		backupRoot: doltbackup.DefaultRoot,
		readFile:   os.ReadFile,
	}
}

// newDoltContainerCmd builds `gt dolt container`, the parent for the commands
// that plan a container.
func newDoltContainerCmd(deps doltContainerPlanDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "container",
		Short: "Plan a Docker container that runs the town's Dolt server",
		Long: `Plan a Docker container that runs the town's Dolt server.

gt never creates the container: it prints the command, and a person reads it,
reviews it and runs it.`,
		Args: cobra.NoArgs,
		RunE: requireSubcommand,
	}
	cmd.AddCommand(newDoltContainerPlanCmd(deps))
	return cmd
}

// newDoltContainerPlanCmd builds `gt dolt container plan`, which prints the
// docker create command and runs nothing.
func newDoltContainerPlanCmd(deps doltContainerPlanDeps) *cobra.Command {
	var (
		name, image, configPath       string
		dataDir, backupDir, restart   string
		memoryGiB, hostPort, contPort int
		asJSON                        bool
	)
	cmd := &cobra.Command{
		Use:   "plan --name <container> --image <image> --config <path>",
		Short: "Print the docker create command for a Dolt container; run nothing",
		Long: `Print the docker create command for a container that serves the town's
Dolt data, for a person to review and run. It runs no process and writes no
file. The plan is one shell-quoted line, or the argv as a JSON array with
--json.

The data and backup directories are mounted at the same absolute path inside
and outside the container, so host tools that read .dolt-data and the backups
the server writes keep working. The Dolt port is published on 127.0.0.1 only:
the host port maps to the port the config file's listener names, which
--container-port overrides.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dataDir == "" {
				townRoot, err := deps.townRoot()
				if err != nil {
					return fmt.Errorf("data directory: %w (or pass --data-dir)", err)
				}
				dataDir = doltserver.DefaultConfig(townRoot).DataDir
			}
			if backupDir == "" {
				root, err := deps.backupRoot()
				if err != nil {
					return fmt.Errorf("backup directory: %w (or pass --backup-dir)", err)
				}
				backupDir = root
			}
			// The config's listener port is the default; an explicit
			// --container-port overrides it, even when it is 0, which
			// ContainerPlan then refuses.
			if !cmd.Flags().Changed("container-port") {
				port, err := doltserver.ContainerPortFromConfig(deps.readFile, configPath)
				if err != nil {
					return fmt.Errorf("%w (or pass --container-port)", err)
				}
				contPort = port
			}
			argv, err := doltserver.ContainerPlan(doltserver.ContainerSpec{
				Name:          name,
				Image:         image,
				DataDir:       dataDir,
				BackupDir:     backupDir,
				ConfigPath:    configPath,
				MemoryGiB:     memoryGiB,
				HostPort:      hostPort,
				ContainerPort: contPort,
				Restart:       restart,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if err := json.NewEncoder(out).Encode(argv); err != nil {
					return fmt.Errorf("writing the plan: %w", err)
				}
				return nil
			}
			quoted := make([]string, len(argv))
			for i, a := range argv {
				quoted[i] = config.ShellQuote(a)
			}
			if _, err := fmt.Fprintln(out, strings.Join(quoted, " ")); err != nil {
				return fmt.Errorf("writing the plan: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Docker container name (required)")
	cmd.Flags().StringVar(&image, "image", "", "Docker image that runs the Dolt SQL server (required)")
	cmd.Flags().StringVar(&configPath, "config", "", "Absolute path to the Dolt config.yaml, mounted read-only (required)")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "Absolute data directory to mount at the same path (default: the town's .dolt-data)")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "", "Absolute backup directory to mount at the same path (default: the daemon's backup root)")
	cmd.Flags().IntVar(&memoryGiB, "memory-gib", 8, "Memory cap in GiB, set on both --memory and --memory-swap")
	cmd.Flags().IntVar(&hostPort, "host-port", 3307, "Host port to publish on 127.0.0.1")
	cmd.Flags().IntVar(&contPort, "container-port", 0, "Port the server listens on inside the container (default: the config's listener.port)")
	cmd.Flags().StringVar(&restart, "restart", "unless-stopped", "Restart policy: no, on-failure, always or unless-stopped")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the argv as a JSON array")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("image")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func init() {
	doltCmd.AddCommand(newDoltContainerCmd(doltContainerPlanRealDeps()))
}
