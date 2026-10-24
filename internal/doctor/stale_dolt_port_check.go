package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// StaleDoltPortCheck detects stale Dolt port files that point to wrong ports.
// This can cause bd commands to fail with "database not found" errors when
// they connect to the wrong Dolt server.
// It also checks metadata.json files for port consistency with the running server.
type StaleDoltPortCheck struct {
	FixableCheck
	stalePorts    []stalePortInfo
	staleMetadata []staleMetadataInfo
}

type stalePortInfo struct {
	path        string
	port        int
	correctPort int
}

type staleMetadataInfo struct {
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

// Run checks for stale Dolt port files and metadata.json port consistency.
func (c *StaleDoltPortCheck) Run(ctx *CheckContext) *CheckResult {
	c.stalePorts = nil
	c.staleMetadata = nil

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

	// Check metadata.json files for port consistency
	metadataFiles := c.findMetadataFiles(ctx.TownRoot)
	for _, metaFile := range metadataFiles {
		port := c.getPortFromMetadata(metaFile)
		if port > 0 && port != correctPort {
			c.staleMetadata = append(c.staleMetadata, staleMetadataInfo{
				path:        metaFile,
				port:        port,
				correctPort: correctPort,
			})
			relPath, _ := filepath.Rel(ctx.TownRoot, metaFile)
			details = append(details, fmt.Sprintf("metadata.json %s has port %d (should be %d)", relPath, port, correctPort))
		}
	}

	// Also check for stale dolt config directories with wrong ports
	staleConfigs := c.findStaleDoltConfigs(ctx.TownRoot, correctPort)
	for _, config := range staleConfigs {
		relPath, _ := filepath.Rel(ctx.TownRoot, config)
		details = append(details, fmt.Sprintf("Stale Dolt config directory: %s (contains wrong port configuration)", relPath))
	}

	if len(c.stalePorts) == 0 && len(c.staleMetadata) == 0 && len(staleConfigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All Dolt port files are consistent",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d stale port file(s), %d stale metadata.json(s), %d stale config dir(s)", len(c.stalePorts), len(c.staleMetadata), len(staleConfigs)),
		Details: details,
		FixHint: "Run 'gt doctor fix stale-dolt-port' to fix port inconsistencies",
	}
}

// Fix removes stale Dolt port files and fixes metadata.json port mismatches.
func (c *StaleDoltPortCheck) Fix(ctx *CheckContext) error {
	// Remove stale port files
	for _, info := range c.stalePorts {
		if err := os.Remove(info.path); err != nil {
			return fmt.Errorf("could not remove stale port file %s: %w", info.path, err)
		}
	}

	// Fix metadata.json files with wrong ports
	for _, info := range c.staleMetadata {
		if err := c.fixMetadataPort(info.path, info.correctPort); err != nil {
			return fmt.Errorf("could not fix metadata.json %s: %w", info.path, err)
		}
	}

	return nil
}

// getCorrectPort returns the town's Dolt port from the config resolver
// (mayor/town.json, then .dolt-data/config.yaml), or 0 when it names none.
// A town.json dolt block that names no port is not an endpoint, so it falls
// through to config.yaml like an absent block does (gt-co6me).
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
		locations = append(locations,
			filepath.Join(townRoot, rigName, "mayor", "rig", ".beads", "dolt-server.port"),
			filepath.Join(townRoot, rigName, ".beads", "dolt-server.port"),
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
		rigMeta := filepath.Join(townRoot, rigName, "mayor", "rig", ".beads", "metadata.json")
		if _, err := os.Stat(rigMeta); err == nil {
			files = append(files, rigMeta)
		}
	}

	return files
}

// getPortFromMetadata reads the dolt_server_port from a metadata.json file.
func (c *StaleDoltPortCheck) getPortFromMetadata(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	var metadata struct {
		DoltServerPort int `json:"dolt_server_port"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return 0
	}

	return metadata.DoltServerPort
}

// fixMetadataPort updates the dolt_server_port in a metadata.json file.
func (c *StaleDoltPortCheck) fixMetadataPort(path string, correctPort int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Parse as generic map to preserve all fields
	var metadata map[string]interface{}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}

	// Update the port
	metadata["dolt_server_port"] = correctPort

	// Write back
	newData, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, newData, 0644)
}
