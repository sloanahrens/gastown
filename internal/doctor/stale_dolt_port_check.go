package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// StaleDoltPortCheck detects stale Dolt port files that point to wrong ports.
// This can cause bd commands to fail with "database not found" errors when
// they connect to the wrong Dolt server.
// Fix also removes the deprecated dolt_server_port key from metadata.json.
type StaleDoltPortCheck struct {
	FixableCheck
	stalePorts []stalePortInfo
}

type stalePortInfo struct {
	path        string
	port        int
	correctPort int
}

// NewStaleDoltPortCheck creates a new stale Dolt port check.
func NewStaleDoltPortCheck() *StaleDoltPortCheck {
	return &StaleDoltPortCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stale-dolt-port",
				CheckDescription: "Detect stale Dolt port files pointing to wrong ports",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks for stale Dolt port files. metadata.json carries no port this
// check judges: bd reads the port from the port file (gt-49f3pn).
func (c *StaleDoltPortCheck) Run(ctx *CheckContext) *CheckResult {
	c.stalePorts = nil

	// Get the correct port from the town's Dolt endpoint. A town that names
	// none has no port to compare against: skip rather than assume one, since
	// Fix deletes port files and rewrites metadata.json to the correct port.
	correctPort := c.getCorrectPort(ctx)
	if correctPort <= 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "Dolt port unknown; no port in mayor/town.json or .dolt-data/config.yaml to compare against",
			FixHint: "Run 'gt config set dolt.port <port>' to name the Dolt server's port",
		}
	}

	// Find all dolt-server.port files
	portFiles := c.findPortFiles(ctx.TownRoot)

	var details []string
	for _, portFile := range portFiles {
		data, err := os.ReadFile(portFile)
		if err != nil {
			continue
		}

		portStr := strings.TrimSpace(string(data))
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}

		// Check if port matches the correct port
		if port != correctPort {
			c.stalePorts = append(c.stalePorts, stalePortInfo{
				path:        portFile,
				port:        port,
				correctPort: correctPort,
			})
			relPath, _ := filepath.Rel(ctx.TownRoot, portFile)
			details = append(details, fmt.Sprintf("Stale port file %s has port %d (should be %d)", relPath, port, correctPort))
		}
	}

	// Also check for stale dolt config directories with wrong ports
	staleConfigs := c.findStaleDoltConfigs(ctx.TownRoot, correctPort)
	for _, config := range staleConfigs {
		relPath, _ := filepath.Rel(ctx.TownRoot, config)
		details = append(details, fmt.Sprintf("Stale Dolt config directory: %s (contains wrong port configuration)", relPath))
	}

	if len(c.stalePorts) == 0 && len(staleConfigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All Dolt port files are consistent",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d stale port file(s), %d stale config dir(s)", len(c.stalePorts), len(staleConfigs)),
		Details: details,
		FixHint: "Run 'gt doctor fix stale-dolt-port' to fix port inconsistencies",
	}
}

// Fix removes stale Dolt port files and removes the deprecated dolt_server_port
// key from metadata.json files that carry it (gt-49f3pn). The key goes only
// where the same beads directory has a port file, which Fix writes from the
// town's endpoint when none is left; with neither, the key is bd's only port
// source and is kept, and the file is reported.
func (c *StaleDoltPortCheck) Fix(ctx *CheckContext) error {
	// Remove stale port files
	for _, info := range c.stalePorts {
		if err := os.Remove(info.path); err != nil {
			return fmt.Errorf("could not remove stale port file %s: %w", info.path, err)
		}
	}

	correctPort := c.getCorrectPort(ctx)

	// Every file is tried: one unparseable metadata.json is reported but does
	// not stop the key coming out of the others (gt-49f3pn).
	var errs []error
	for _, metaFile := range c.findMetadataFiles(ctx.TownRoot) {
		if err := c.removeDeprecatedPort(metaFile, correctPort); err != nil {
			errs = append(errs, fmt.Errorf("could not fix metadata.json %s: %w", metaFile, err))
		}
	}

	return errors.Join(errs...)
}

// getCorrectPort returns the town's Dolt port from the config resolver
// (mayor/town.json, then .dolt-data/config.yaml), or 0 when it names none.
// A town.json dolt block that names no port is not an endpoint: LoadTownConfig
// rejects a dolt.port outside 1..65535, so ResolveDoltEndpoint falls through to
// config.yaml exactly as it does for an absent block (gt-co6me).
func (c *StaleDoltPortCheck) getCorrectPort(ctx *CheckContext) int {
	ep, ok := config.ResolveDoltEndpoint(ctx.TownRoot)
	if !ok || ep.Port <= 0 {
		return 0
	}
	return ep.Port
}

// findPortFiles finds all dolt-server.port files in known locations.
// Avoids filepath.Walk over the entire town root, which is extremely slow
// on Docker bind mounts (macOS VirtioFS).
func (c *StaleDoltPortCheck) findPortFiles(townRoot string) []string {
	var files []string

	// Known locations for port files
	locations := []string{
		filepath.Join(townRoot, ".beads", "dolt-server.port"),
		filepath.Join(townRoot, ".dolt-data", ".beads", "dolt-server.port"),
		filepath.Join(townRoot, "daemon", "dolt.port"),
	}

	// Rig .beads directories (same discovery pattern as findMetadataFiles)
	for rigName := range loadRigNames(filepath.Join(townRoot, "mayor", "rigs.json")) {
		rigPath := filepath.Join(townRoot, rigName)
		locations = append(locations,
			filepath.Join(constants.RigBeadsWritePath(rigPath), "dolt-server.port"),
			filepath.Join(constants.LegacyRigBeadsPath(rigPath), "dolt-server.port"),
		)
	}

	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			files = append(files, loc)
		}
	}

	return files
}

// findStaleDoltConfigs finds stale Dolt config directories with wrong ports.
func (c *StaleDoltPortCheck) findStaleDoltConfigs(townRoot string, correctPort int) []string {
	var staleConfigs []string

	// Check for .beads/dolt/ directory which shouldn't exist when using shared Dolt server
	staleDir := filepath.Join(townRoot, ".beads", "dolt")
	if _, err := os.Stat(staleDir); err == nil {
		// Check if it has a config.yaml with wrong port
		configPath := filepath.Join(staleDir, "config.yaml")
		if data, err := os.ReadFile(configPath); err == nil {
			// Check for any port that's not the correct port
			if strings.Contains(string(data), "port:") && !strings.Contains(string(data), fmt.Sprintf("port: %d", correctPort)) {
				staleConfigs = append(staleConfigs, staleDir)
			}
		}
	}

	return staleConfigs
}

// findMetadataFiles finds all metadata.json files that might contain Dolt port config.
func (c *StaleDoltPortCheck) findMetadataFiles(townRoot string) []string {
	var files []string

	// Town root metadata
	townMeta := filepath.Join(townRoot, ".beads", "metadata.json")
	if _, err := os.Stat(townMeta); err == nil {
		files = append(files, townMeta)
	}

	// Rig metadata files
	for rigName := range loadRigNames(filepath.Join(townRoot, "mayor", "rigs.json")) {
		rigPath := filepath.Join(townRoot, rigName)
		for _, beadsDir := range []string{constants.RigBeadsWritePath(rigPath), constants.LegacyRigBeadsPath(rigPath)} {
			rigMeta := filepath.Join(beadsDir, "metadata.json")
			if _, err := os.Stat(rigMeta); err == nil {
				files = append(files, rigMeta)
			}
		}
	}

	return files
}

// removeDeprecatedPort deletes the dolt_server_port key from a metadata.json
// file, preserving every other field. A file without the key is not rewritten.
// The key is deleted only once the beads directory holds a port file: an
// existing one, or one written here from correctPort. With neither (no town
// endpoint) the key stays, since it is the only port source there.
func (c *StaleDoltPortCheck) removeDeprecatedPort(path string, correctPort int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Parse as generic map to preserve all fields
	var metadata map[string]interface{}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}
	if _, ok := metadata["dolt_server_port"]; !ok {
		return nil
	}

	portFile := filepath.Join(filepath.Dir(path), "dolt-server.port")
	if !hasPortFile(portFile) {
		if correctPort <= 0 {
			return errors.New("dolt_server_port kept: no port file to replace it and no town endpoint to write one from")
		}
		if err := atomicfile.WriteFile(portFile, []byte(strconv.Itoa(correctPort)), 0644); err != nil {
			return fmt.Errorf("writing dolt-server.port before removing the key: %w", err)
		}
	}
	delete(metadata, "dolt_server_port")

	newData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, newData, 0644)
}

// hasPortFile reports whether path holds a usable port. A file that does not
// parse is not a port source, so the caller replaces it.
func hasPortFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return err == nil && port > 0
}
