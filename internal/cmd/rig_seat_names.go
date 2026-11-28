package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// rigSeatNames lists the seat names one rig may hand out: every name its pool
// draws and every name the seats list, lower-cased, each once, sorted. The pool
// is built from the same settings the polecat manager reads (the namepool block
// of settings/config.json, else the rig config's polecat_names, else the
// default). It starts no process and writes nothing.
//
// A settings file that exists but does not load, a rig config that does not
// load, or a theme that does not resolve is an error naming the rig and no
// names. Nothing falls back to a default pool for a file that could not be read.
func rigSeatNames(townRoot, rigName string, seatNames map[string][]string) ([]string, error) {
	// The rig name becomes a path under townRoot, so it must be one directory name.
	if rigName == "" || rigName == "." || rigName == ".." || strings.ContainsAny(rigName, `/\`) {
		return nil, fmt.Errorf("rig name %q is not one directory name under the town root", rigName)
	}
	rigPath := filepath.Join(townRoot, rigName)
	pool, err := rigNamePool(townRoot, rigPath, rigName)
	if err != nil {
		return nil, err
	}
	pool.SetSeatNames(seatNames)
	drawable, err := pool.DrawableNames()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(drawable))
	names := make([]string, 0, len(drawable))
	for _, name := range drawable {
		name = strings.ToLower(name)
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// joinSeatNames joins the seat names of several rigs: every name once, sorted.
// A failing rig, an empty rig list and a join with no names are errors, no names.
func joinSeatNames(rigNames []string, namesOf func(rigName string) ([]string, error)) ([]string, error) {
	seen := make(map[string]bool)
	var names []string
	for _, rigName := range rigNames {
		rigSeats, err := namesOf(rigName)
		if err != nil {
			return nil, fmt.Errorf("rig %s: seat names: %w", rigName, err)
		}
		for _, name := range rigSeats {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return nil, errors.New("the rigs yield no seat names")
	}
	sort.Strings(names)
	return names, nil
}

// townSeatNames lists every seat name any rig of the town may hand out: the
// registered rigs' names joined once and sorted, the town's seats feeding every
// pool. It writes nothing; an unreadable registry or settings file is an error.
func townSeatNames(townRoot string) ([]string, error) {
	rigNames, err := knownRigNames(townRoot)
	if err != nil {
		return nil, err
	}
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return nil, err
	}
	seats, err := config.Seats(ts, nil)
	if err != nil {
		return nil, err
	}
	seatNames := make(map[string][]string, len(seats))
	for _, seat := range seats {
		seatNames[seat.Agent] = seat.Names
	}
	return joinSeatNames(rigNames, func(rigName string) ([]string, error) {
		return rigSeatNames(townRoot, rigName, seatNames)
	})
}

// rigNamePool builds the rig's name pool from its settings, mirroring the
// branches of newManager (internal/polecat/manager.go), except that an
// unreadable file is an error here rather than a fallback.
func rigNamePool(townRoot, rigPath, rigName string) (*polecat.NamePool, error) {
	settingsPath := filepath.Join(rigPath, "settings", "config.json")
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		return nil, fmt.Errorf("rig %s: settings %s do not load: %w", rigName, settingsPath, err)
	}
	if err == nil && settings.Namepool != nil {
		pool := polecat.NewNamePoolWithConfig(rigPath, rigName,
			settings.Namepool.Style, settings.Namepool.Names, settings.Namepool.MaxBeforeNumbering)
		pool.SetTownRoot(townRoot)
		return pool, nil
	}

	rigCfg, err := rig.LoadRigConfigIfPresent(rigPath)
	if err != nil {
		return nil, fmt.Errorf("rig %s: rig config does not load: %w", rigName, err)
	}
	var pool *polecat.NamePool
	if rigCfg != nil && len(rigCfg.PolecatNames) > 0 {
		pool = polecat.NewNamePoolWithConfig(rigPath, rigName, "", rigCfg.PolecatNames, 0)
	} else {
		pool = polecat.NewNamePool(rigPath, rigName)
	}
	pool.SetTownRoot(townRoot)
	return pool, nil
}
