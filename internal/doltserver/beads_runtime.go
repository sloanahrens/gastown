package doltserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/townconfig"
)

// BeadsRuntimeConfig is the Dolt server a .beads directory's metadata.json
// points at, and the database on it (townconfig.DatabaseForBeadsDir).
type BeadsRuntimeConfig struct {
	Source   string
	Database string
	Host     string
	Port     int
}

// ReadBeadsRuntimeConfig reads beadsDir/metadata.json. ok is false unless the
// directory is a Dolt server-mode store.
func ReadBeadsRuntimeConfig(beadsDir string) (cfg BeadsRuntimeConfig, ok bool) {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return BeadsRuntimeConfig{}, false
	}

	var metadata struct {
		Backend        string `json:"backend"`
		Database       string `json:"database"`
		DoltMode       string `json:"dolt_mode"`
		DoltServerHost string `json:"dolt_server_host"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return BeadsRuntimeConfig{}, false
	}
	if metadata.Backend != "dolt" || metadata.DoltMode != "server" {
		return BeadsRuntimeConfig{}, false
	}

	host := metadata.DoltServerHost
	if host == "" {
		host = "127.0.0.1"
	}
	// The port file is the port source; the deprecated metadata.json key is
	// not read (gt-49f3pn).
	port := 0
	if data, err := os.ReadFile(filepath.Join(beadsDir, "dolt-server.port")); err == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && parsed > 0 {
			port = parsed
		}
	}
	if port == 0 {
		port = DefaultPort
	}
	database := townconfig.DatabaseForBeadsDir(beadsDir)
	if database == "" {
		database = metadata.Database
	}

	return BeadsRuntimeConfig{
		Source:   metadataPath,
		Database: database,
		Host:     host,
		Port:     port,
	}, true
}
