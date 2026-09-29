package doltserver

import "os"

// DoltHubToken returns the DoltHub API token from the environment.
// Returns empty string if not configured. Used by the wasteland commands
// (gt wl), which fork wl-commons on DoltHub; the town's own beads databases
// never get a DoltHub remote (ADR 0002).
func DoltHubToken() string {
	return os.Getenv("DOLTHUB_TOKEN")
}

// DoltHubOrg returns the default DoltHub organization from the environment.
// Returns empty string if not configured.
func DoltHubOrg() string {
	return os.Getenv("DOLTHUB_ORG")
}
