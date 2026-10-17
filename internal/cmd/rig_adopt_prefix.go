package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
)

// rigAdoptRequest is what runRigAdopt hands to adoptRegisterIn: the rig name
// and the validated flag values.
type rigAdoptRequest struct {
	name        string
	gitURL      string
	pushURL     string
	upstreamURL string
	prefix      string // --prefix, empty when not given
	force       bool
}

// detectAdoptBeadsPrefix returns the prefix the adopted rig's own beads
// database uses, or "" when it has none to detect. flagPrefix is --prefix; a
// detected prefix that disagrees with it is an error.
//
// It reads the same .beads the adopt goes on to use: the rig-level one, else
// mayor/rig's. It asks bd for issue_prefix first and falls back to the
// "beads_<prefix>" dolt_database name in metadata.json, which survives a clone
// because dolt/ is gitignored since bd v0.50+. configGet is the bd lookup, a
// parameter so a test can answer without a bd binary.
func detectAdoptBeadsPrefix(rigPath, flagPrefix string, configGet func(workDir string) (string, error)) (string, error) {
	for _, beadsDir := range []string{
		filepath.Join(rigPath, ".beads"),
		filepath.Join(rigPath, "mayor", "rig", ".beads"),
	} {
		if _, err := os.Stat(beadsDir); err != nil {
			continue
		}
		return detectBeadsDirPrefix(beadsDir, flagPrefix, configGet)
	}
	return "", nil
}

func detectBeadsDirPrefix(beadsDir, flagPrefix string, configGet func(workDir string) (string, error)) (string, error) {
	metaBytes, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return "", nil
	}
	var meta struct {
		Backend string `json:"backend"`
	}
	if json.Unmarshal(metaBytes, &meta) != nil || meta.Backend != "dolt" {
		return "", nil
	}

	detected := ""
	if got, bdErr := configGet(filepath.Dir(beadsDir)); bdErr == nil {
		detected = got
	}
	if detected == "" {
		if db := config.BeadsMetadataDatabase(beadsDir); strings.HasPrefix(db, "beads_") {
			detected = strings.TrimPrefix(db, "beads_")
		}
	}
	if detected != "" && flagPrefix != "" && strings.TrimSuffix(flagPrefix, "-") != detected {
		return "", fmt.Errorf("prefix mismatch: source repo uses '%s' but --prefix '%s' was provided", detected, flagPrefix)
	}
	return detected, nil
}

// adoptRegisterIn registers an existing rig: it detects the prefix from the
// rig's beads, registers the rig in rigs.json and adds its route. The prefix is
// settled before anything is written, so a --prefix that disagrees with the
// beads fails with the town untouched and a retry is not met by ErrRigExists,
// and a rig adopted without --prefix is registered under the prefix its
// database holds rather than one derived from its name (gt-prqdo).
func adoptRegisterIn(townRoot string, req rigAdoptRequest, configGet func(workDir string) (string, error)) (*rig.Manager, *rig.RegisterRigResult, error) {
	// A missing rigs.json is a town with no rigs yet; one that does not parse is
	// damage and adopting a rig must not replace it with one entry (gt-52mgl).
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfigOrEmpty(rigsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading rigs config: %w", err)
	}
	mgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))

	prefix := req.prefix
	detected, err := detectAdoptBeadsPrefix(filepath.Join(townRoot, req.name), req.prefix, configGet)
	if err != nil {
		return nil, nil, err
	}
	if prefix == "" {
		prefix = detected
	}

	result, err := mgr.RegisterRig(rig.RegisterRigOptions{
		Name:        req.name,
		GitURL:      req.gitURL,
		PushURL:     req.pushURL,
		UpstreamURL: req.upstreamURL,
		BeadsPrefix: prefix,
		Force:       req.force,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("adopting rig: %w", err)
	}

	// Save only this rig's entry: rigsConfig was loaded before the adoption
	// ran, and saving it whole reverts a rig another gt process registered or
	// parked in that window (gt-4iobv).
	if err := config.SetRigEntry(rigsPath, req.name, rigsConfig.Rigs[req.name]); err != nil {
		return nil, nil, fmt.Errorf("saving rigs config: %w", err)
	}

	// Add route to town-level routes.jsonl for prefix-based routing
	if result.BeadsPrefix != "" {
		routePath := req.name
		if _, err := os.Stat(filepath.Join(townRoot, req.name, "mayor", "rig", ".beads")); err == nil {
			routePath = req.name + "/mayor/rig"
		}
		route := beads.Route{
			Prefix: result.BeadsPrefix + "-",
			Path:   routePath,
		}
		if err := beads.AppendRoute(townRoot, route); err != nil {
			fmt.Printf("  %s Could not update routes.jsonl: %v\n", style.Warning.Render("!"), err)
		}
	}
	return mgr, result, nil
}
