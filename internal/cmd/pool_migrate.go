package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt pool migrate converts a town config that still holds the two-seat keys
// into the seat map (gt-mukgk). Those keys are rejected at load, so the typed
// loaders cannot read the file it works on: it reads and writes the files as
// raw JSON trees, and checks the result loads before it writes anything.

var poolMigrateDryRun bool

var poolMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Convert the two-seat keys in the town config to polecat_pool.seats",
	Long: `Convert a town config that still holds the two-seat keys to polecat_pool.seats.
Those keys no longer load: the daemon stops at start with a message that names
this command.

  polecat_pool.agent, max_seats        -> seats.<agent>.cap (overflow_agent and
                                          max_overflow are the same keys)
  spec_dispatch hooked_agent,
    max_hooked                         -> seats.<agent>.cap
  polecat_pool.pro_agent, pro_max,
    pro_label                          -> removed (the pro seat is gone)
  spec_dispatch prefer_hooked          -> removed (a bead with no agent label
                                          draws uniformly among the free seats)
  role_agents.polecat                  -> removed (the seats name the agents)
  agent on a dispatch "spec" entry
    in scheduled_slings                -> removed (the spec dispatcher picks the
                                          seat)

A pool seat with no max_seats keeps the cap the dispatcher gave it, 2. An agent
named by both old seats keeps the seat that came first in the dispatcher's
order: the pool seat, or the second seat when prefer_hooked was set. A seat
already in polecat_pool.seats is never overwritten.

It reads the files as raw JSON, so it works on a file the strict loader refuses.
It checks that the converted files load before it writes, backs each file up
beside it as <file>.bak-<UTC timestamp>-pool-migrate, and writes atomically.
--dry-run prints what it would change and writes nothing. A second run finds
nothing to do.

The cutover is one operator step on a quiet town: gt pool migrate, then restart
the daemon.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runPoolMigrate(cmd.OutOrStdout(), townRoot, poolMigrateDryRun, time.Now())
	},
}

func init() {
	poolMigrateCmd.Flags().BoolVar(&poolMigrateDryRun, "dry-run", false, "Print what would change and write nothing")
	poolCmd.AddCommand(poolMigrateCmd)
}

const (
	// poolMigrateDefaultSeatCap is the cap the dispatcher gave the pool's seat
	// when max_seats was unset, so a migrated seat runs at the cap it ran at.
	poolMigrateDefaultSeatCap = 2
	// poolMigrateDefaultHookedAgent is the agent the second seat ran when
	// max_hooked named a cap but hooked_agent no agent.
	poolMigrateDefaultHookedAgent = "claude-sonnet"
	// poolMigrateBackupTag ends the backup name, after the UTC timestamp.
	poolMigrateBackupTag = "-pool-migrate"
)

// legacySeat is one of the two seats the old keys described.
type legacySeat struct {
	agent string
	cap   int
	// from names the keys it was read from, for the line that reports it.
	from string
}

// runPoolMigrate converts the town's config files. now stamps the backups.
func runPoolMigrate(w io.Writer, townRoot string, dryRun bool, now time.Time) error {
	settingsPath := config.TownSettingsPath(townRoot)
	patrolPath := poolMigratePatrolFile(townRoot, settingsPath)

	settings, err := config.ReadRawJSON(settingsPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintln(w, "nothing to migrate")
		return nil
	case err != nil:
		return err
	}
	var patrol map[string]any
	if patrolPath != "" {
		patrol, err = config.ReadRawJSON(patrolPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	// Plan on copies: the plan converts in place, and the write below plans
	// again from the patrol document as read.
	plan, err := planPoolMigrate(townRoot, cloneTree(settings), cloneTree(patrol))
	if err != nil {
		return err
	}
	if len(plan.changes) == 0 {
		fmt.Fprintln(w, "nothing to migrate")
		return nil
	}
	for _, c := range plan.changes {
		fmt.Fprintln(w, c)
	}
	if dryRun {
		fmt.Fprintln(w, "dry run: nothing written")
		return nil
	}

	stamp := now.UTC().Format("20060102T150405Z") + poolMigrateBackupTag
	// The settings file first: it takes the converted seats. A run that stops
	// between the two files leaves the patrol file's keys for the next run,
	// which finds the seats in place and removes the keys.
	wroteSettings := false
	err = config.UpdateRawJSON(settingsPath, func(tree map[string]any) (bool, error) {
		before := cloneTree(tree)
		if _, err := planPoolMigrate(townRoot, tree, cloneTree(patrol)); err != nil {
			return false, err
		}
		// A plan that only removes keys from the separate patrol file leaves
		// this one as it was, and it is not rewritten.
		if reflect.DeepEqual(before, tree) {
			return false, nil
		}
		if err := backupConfigFile(settingsPath, stamp); err != nil {
			return false, err
		}
		wroteSettings = true
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("migrating %s: %w", settingsPath, err)
	}
	if wroteSettings {
		fmt.Fprintf(w, "wrote %s (backup %s.bak-%s)\n", config.SourcePathRel(townRoot, settingsPath), filepath.Base(settingsPath), stamp)
	}
	if plan.patrolChange {
		err = config.UpdateRawJSON(patrolPath, func(tree map[string]any) (bool, error) {
			if !takePatrolSeatKeys(tree).changed {
				return false, nil
			}
			if err := backupConfigFile(patrolPath, stamp); err != nil {
				return false, err
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("migrating %s: %w", patrolPath, err)
		}
		fmt.Fprintf(w, "wrote %s (backup %s.bak-%s)\n", config.SourcePathRel(townRoot, patrolPath), filepath.Base(patrolPath), stamp)
	}
	return nil
}

// poolMigratePatrolFile is the file holding the daemon's patrol document when it
// is not a section of the settings file: mayor/daemon.json on a five-file town.
// It is empty when the settings file carries the document, which the two-file
// layout does.
func poolMigratePatrolFile(townRoot, settingsPath string) string {
	path := config.WritePath(config.DaemonPatrolConfigPath(townRoot))
	if filepath.Clean(path) == filepath.Clean(settingsPath) {
		return ""
	}
	return path
}

// poolMigratePlan is what a migration changes: the report lines, in the order
// the keys are converted, and whether the separate patrol document changed.
type poolMigratePlan struct {
	changes      []string
	patrolChange bool
}

// planPoolMigrate converts settings in place, and patrol when it is a separate
// document, and reports each change. It refuses a conversion whose result would
// not load, so a run writes nothing it could not read back.
func planPoolMigrate(townRoot string, settings, patrol map[string]any) (*poolMigratePlan, error) {
	// The patrol document is the settings file's "daemon" section on a
	// two-file town.
	doc, separate := patrol, true
	if inTree, ok := settings["daemon"].(map[string]any); ok {
		doc, separate = inTree, false
	}
	found := takePatrolSeatKeys(doc)
	poolLines, err := migratePoolKeys(settings, found.hooked, found.prefer)
	if err != nil {
		return nil, err
	}
	changes := append(poolLines, found.lines...)
	if len(changes) == 0 {
		return &poolMigratePlan{}, nil
	}
	if err := checkMigrated(townRoot, settings, patrol); err != nil {
		return nil, fmt.Errorf("the migrated config would not load, so nothing is written: %w", err)
	}
	return &poolMigratePlan{changes: changes, patrolChange: separate && found.changed}, nil
}

// patrolSeatKeys is what takePatrolSeatKeys found in a patrol document.
type patrolSeatKeys struct {
	// hooked is the second seat the old keys described, nil when they
	// described none; prefer is whether prefer_hooked put it first.
	hooked *legacySeat
	prefer bool
	// lines report the keys removed that converted to no seat. A key that
	// became a seat is reported by the conversion.
	lines []string
	// changed is whether the document held any key to remove.
	changed bool
}

// takePatrolSeatKeys removes the second seat's keys and the agent pin on spec
// scheduled entries from a patrol document.
func takePatrolSeatKeys(doc map[string]any) patrolSeatKeys {
	var out patrolSeatKeys
	patrols, _ := doc["patrols"].(map[string]any)
	if spec, ok := patrols["spec_dispatch"].(map[string]any); ok {
		agent := stringOf(spec["hooked_agent"])
		capacity, _ := intOf(spec["max_hooked"])
		switch {
		case capacity > 0:
			if agent == "" {
				agent = poolMigrateDefaultHookedAgent
			}
			out.hooked = &legacySeat{agent: agent, cap: capacity, from: fmt.Sprintf("spec_dispatch hooked_agent %q, max_hooked %d", agent, capacity)}
		case agent != "":
			// A named agent with no cap above zero was a closed second seat.
			out.hooked = &legacySeat{agent: agent, cap: 0, from: fmt.Sprintf("spec_dispatch hooked_agent %q (max_hooked unset: closed)", agent)}
		}
		if p, ok := spec["prefer_hooked"].(bool); ok {
			out.prefer = p
		}
		_, hadAgent := spec["hooked_agent"]
		_, hadCap := spec["max_hooked"]
		_, hadPrefer := spec["prefer_hooked"]
		for _, key := range []string{"hooked_agent", "max_hooked", "prefer_hooked"} {
			delete(spec, key)
		}
		out.changed = out.changed || hadAgent || hadCap || hadPrefer
		if hadPrefer {
			out.lines = append(out.lines, "removed spec_dispatch prefer_hooked: a bead with no agent label draws uniformly among the free seats")
		}
		if out.hooked == nil && (hadAgent || hadCap) {
			out.lines = append(out.lines, "removed spec_dispatch hooked_agent, max_hooked: they named no open seat")
		}
	}
	if sl, ok := patrols["scheduled_slings"].(map[string]any); ok {
		entries, _ := sl["entries"].([]any)
		for _, e := range entries {
			entry, ok := e.(map[string]any)
			if !ok || entry["dispatch"] != "spec" {
				continue
			}
			if agent, present := entry["agent"]; present {
				delete(entry, "agent")
				out.changed = true
				name, _ := entry["name"].(string)
				out.lines = append(out.lines, fmt.Sprintf("removed agent %v from scheduled sling %q: the spec dispatcher picks the seat", agent, name))
			}
		}
	}
	return out
}

// migratePoolKeys converts polecat_pool's two-seat keys and removes
// role_agents.polecat from the settings tree, folding the second seat taken
// from the patrol document into the seat map.
func migratePoolKeys(settings map[string]any, hooked *legacySeat, prefer bool) ([]string, error) {
	var lines []string
	pool, _ := settings["polecat_pool"].(map[string]any)

	var poolSeat *legacySeat
	if pool != nil {
		agent := stringOf(pool["agent"])
		if agent == "" {
			agent = stringOf(pool["overflow_agent"])
		}
		capacity, hasCap := intOf(pool["max_seats"])
		if !hasCap {
			capacity, hasCap = intOf(pool["max_overflow"])
		}
		if agent != "" {
			from := fmt.Sprintf("polecat_pool.agent %q, max_seats %d", agent, capacity)
			if !hasCap || capacity <= 0 {
				capacity = poolMigrateDefaultSeatCap
				from = fmt.Sprintf("polecat_pool.agent %q (max_seats unset: the dispatcher's default of %d)", agent, capacity)
			}
			poolSeat = &legacySeat{agent: agent, cap: capacity, from: from}
		}
	}

	// The order the dispatcher kept one seat per agent in: the pool seat, then
	// the second, with prefer_hooked moving the second first.
	order := []*legacySeat{poolSeat, hooked}
	if prefer {
		order = []*legacySeat{hooked, poolSeat}
	}
	for _, s := range order {
		if s == nil {
			continue
		}
		if pool == nil {
			pool = map[string]any{}
			settings["polecat_pool"] = pool
		}
		seats, ok := pool["seats"].(map[string]any)
		if _, present := pool["seats"]; present && !ok {
			return nil, errors.New("polecat_pool.seats is not an object")
		}
		if seats == nil {
			seats = map[string]any{}
			pool["seats"] = seats
		}
		if _, exists := seats[s.agent]; exists {
			lines = append(lines, fmt.Sprintf("%s: polecat_pool.seats.%s already exists, kept as it is", s.from, s.agent))
			continue
		}
		seats[s.agent] = map[string]any{"cap": json.Number(strconv.Itoa(s.cap))}
		lines = append(lines, fmt.Sprintf("%s -> polecat_pool.seats.%s.cap %d", s.from, s.agent, s.cap))
	}

	if pool != nil {
		for _, key := range []string{"agent", "max_seats", "overflow_agent", "max_overflow"} {
			delete(pool, key)
		}
		for _, key := range []string{"pro_agent", "pro_max", "pro_label"} {
			if v, present := pool[key]; present {
				delete(pool, key)
				lines = append(lines, fmt.Sprintf("removed polecat_pool.%s (%v): the pro seat is gone", key, v))
			}
		}
	}

	if roles, ok := settings["role_agents"].(map[string]any); ok {
		if v, present := roles["polecat"]; present {
			delete(roles, "polecat")
			lines = append(lines, fmt.Sprintf("removed role_agents.polecat (%v): polecat_pool.seats names the agents polecats run on", v))
		}
	}
	return lines, nil
}

// checkMigrated decodes the converted trees the way the loaders do, and reads
// the seat map through the seat loader.
func checkMigrated(townRoot string, settings, patrol map[string]any) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	var ts config.TownSettings
	if err := config.DecodeJSONFile(config.TownSettingsPath(townRoot), data, &ts); err != nil {
		return err
	}
	if _, err := config.Seats(&ts, config.AgentResolver(townRoot, &ts)); err != nil {
		return err
	}
	if patrol != nil {
		data, err := json.Marshal(patrol)
		if err != nil {
			return err
		}
		var pc config.DaemonPatrolConfig
		if err := config.DecodeJSONFile(config.DaemonPatrolConfigPath(townRoot), data, &pc); err != nil {
			return err
		}
	}
	return nil
}

// intOf reads a JSON number as an int.
func intOf(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// stringOf reads a JSON string, trimmed; anything else is empty.
func stringOf(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// cloneTree copies a JSON tree, so a plan can change a document without
// changing the one the caller holds. A nil tree stays nil.
func cloneTree(tree map[string]any) map[string]any {
	if tree == nil {
		return nil
	}
	data, err := json.Marshal(tree)
	if err != nil {
		return tree
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return tree
	}
	return out
}
