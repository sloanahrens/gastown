package cmd

import (
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// gt config secrets (gt-y3pgh.5): tokens live in settings/daemon.env and
// settings/config.json references them by name (internal/config/secrets.go).

var configSecretsMigrateDryRun bool

var configSecretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Keep agent tokens in settings/daemon.env, referenced by name",
	Long: `Agent tokens live in settings/daemon.env (mode 0600), and an agent's env
block in settings/config.json references them by name:

  "env": {"ANTHROPIC_AUTH_TOKEN": "${DEEPSEEK_FLASH_ANTHROPIC_AUTH_TOKEN}"}

A session's startup command reads a referenced daemon.env value when it runs,
so the token never appears in a command line. A literal token in
settings/config.json draws a warning (gt doctor, and the town-running
commands); after 'gt config set secrets.refuse_literals true' it stops the
town from loading instead.`,
	RunE: requireSubcommand,
}

var configSecretsMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Move literal tokens from settings/config.json into settings/daemon.env",
	Long: `Move every literal token in settings/config.json's agent env blocks into
settings/daemon.env (mode 0600) and rewrite each to a ${VAR} reference.

Output names key paths and daemon.env variable names only, never a value.
Equal tokens share one daemon.env entry; a token daemon.env already holds
reuses its entry. Restart sessions afterwards for them to pick up the
references; nothing running changes.

Examples:
  gt config secrets migrate --dry-run   # show what would move
  gt config secrets migrate`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return configSecretsMigrate(cwdConfigCmdEnv(), configSecretsMigrateDryRun)
	},
}

func init() {
	configSecretsMigrateCmd.Flags().BoolVar(&configSecretsMigrateDryRun, "dry-run", false, "Print the key paths and variable names that would move; write nothing")
	configSecretsCmd.AddCommand(configSecretsMigrateCmd)
	configCmd.AddCommand(configSecretsCmd)
}

func configSecretsMigrate(e configCmdEnv, dryRun bool) error {
	townRoot, err := e.findTown()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	var moves []config.SecretMove
	if dryRun {
		moves, err = config.PlanSecretMigration(townRoot)
	} else {
		moves, err = config.MigrateSecrets(townRoot)
	}
	if err != nil {
		return err
	}
	if len(moves) == 0 {
		fmt.Fprintf(e.out, "No literal tokens in %s\n", townconfig.FileSettings)
		return nil
	}
	verb := "Moved"
	if dryRun {
		verb = "Would move"
	}
	fmt.Fprintf(e.out, "%s %d literal token(s) from %s to %s:\n", verb, len(moves), townconfig.FileSettings, townconfig.FileDaemonEnv)
	for _, m := range moves {
		note := "new entry"
		if !m.Added {
			note = "existing entry"
		}
		fmt.Fprintf(e.out, "  %s -> ${%s} (%s)\n", m.Path(), m.Var, note)
	}
	if dryRun {
		return nil
	}
	fmt.Fprintf(e.out, "\nVerify with 'gt doctor' (town-config-secrets), restart sessions to use the references,\nthen refuse literals from now on: gt config set secrets.refuse_literals true\n")
	return nil
}

// literalSecretsWarned makes the gate's literal-token warning print once per
// process: the daemon checks the config before every session start.
var literalSecretsWarned sync.Once

// warnLiteralSecretsOnce prints town's literal-token warning to w, the first
// time it has one.
func warnLiteralSecretsOnce(w io.Writer, town *townconfig.Town) {
	msg := town.LiteralSecretsWarning()
	if msg == "" {
		return
	}
	literalSecretsWarned.Do(func() { fmt.Fprintln(w, msg) })
}
