package doltserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BeadsRuntimeConfig is the Dolt server a .beads directory's metadata.json
// points at.
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
		DoltDatabase   string `json:"dolt_database"`
		DoltServerHost string `json:"dolt_server_host"`
		DoltServerPort int    `json:"dolt_server_port"`
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
	port := metadata.DoltServerPort
	if port == 0 {
		if data, err := os.ReadFile(filepath.Join(beadsDir, "dolt-server.port")); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && parsed > 0 {
				port = parsed
			}
		}
	}
	if port == 0 {
		port = DefaultPort
	}
	database := metadata.DoltDatabase
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
