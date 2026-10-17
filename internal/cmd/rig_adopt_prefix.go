package cmd

import (
	"encoding/json"
	"errors"
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

// errNoDetectablePrefix marks a dolt .beads whose prefix neither bd nor the
// database name in metadata.json yields.
var errNoDetectablePrefix = errors.New("no prefix detectable")

// detectAdoptBeadsPrefix returns the prefix the adopted rig's own beads
// database uses, or "" when it has no .beads with a metadata.json. flagPrefix
// is --prefix; a detected prefix that disagrees with it is an error.
//
// It reads the same .beads directories the adopt goes on to use, rig-level
// first and then mayor/rig's, and takes the first prefix either yields. A
// .beads without a metadata.json has nothing to detect and the next one is
// tried. Without --prefix, a metadata.json that cannot be read or parsed, a
// backend other than dolt, and a dolt database that yields no prefix are
// errors rather than a rig registered under a prefix derived from its name;
// the last is raised only after the other directory has had its turn.
// configGet is the bd lookup, a parameter so a test can answer without a bd
// binary.
func detectAdoptBeadsPrefix(rigPath, flagPrefix string, configGet func(workDir string) (string, error)) (string, error) {
	var undetected error
	for _, beadsDir := range []string{
		filepath.Join(rigPath, ".beads"),
		filepath.Join(rigPath, "mayor", "rig", ".beads"),
	} {
		if _, err := os.Stat(beadsDir); err != nil {
			continue
		}
		detected, err := detectBeadsDirPrefix(beadsDir, flagPrefix, configGet)
		if errors.Is(err, errNoDetectablePrefix) {
			if undetected == nil {
				undetected = err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if detected != "" {
			return detected, nil
		}
	}
	return "", undetected
}

// detectBeadsDirPrefix is the prefix of one .beads directory: bd's
// issue_prefix, else the "beads_<prefix>" dolt_database name in metadata.json,
// which survives a clone because dolt/ is gitignored since bd v0.50+. An absent
// backend is dolt, which bd writes by default; any other backend has no
// database name to read, so it is an error unless flagPrefix already settles
// the prefix.
func detectBeadsDirPrefix(beadsDir, flagPrefix string, configGet func(workDir string) (string, error)) (string, error) {
	metaPath := filepath.Join(beadsDir, "metadata.json")
	metaBytes, err := os.ReadFile(metaPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", metaPath, err)
	}
	var meta struct {
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return "", fmt.Errorf("parsing %s: %w", metaPath, err)
	}
	if meta.Backend != "" && meta.Backend != "dolt" {
		if flagPrefix != "" {
			return "", nil
		}
		return "", fmt.Errorf("%s names backend %q, not dolt, so the rig's prefix cannot be detected: pass --prefix", metaPath, meta.Backend)
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
	if detected == "" && flagPrefix == "" {
		return "", fmt.Errorf("%s: %w: pass --prefix", metaPath, errNoDetectablePrefix)
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
