package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Where the cloud patrol's report is read from. The patrol writes under its own
// account, outside the town, so there is no config file field for it: the
// operator names the directory for the machine, and the default is where the
// patrol installed on this host writes (gt-tlvco).

const (
	// CloudReportsDefault is the directory the patrol writes its report to.
	CloudReportsDefault = "/Users/Shared/gt-cloud/reports"
	// CloudReportsEnv is the variable that overrides it.
	CloudReportsEnv = "GT_CLOUD_REPORTS_DIR"
)

// CloudReportsDir is the directory to read the patrol's report from:
// $GT_CLOUD_REPORTS_DIR when it is set, and the patrol's own otherwise.
func CloudReportsDir() string { return cloudReportsDir(os.Getenv) }

// cloudReportsDir is CloudReportsDir with its environment injected, so a test
// can pin the directory without mutating the process environment.
func cloudReportsDir(getenv func(string) string) string {
	return envDir(getenv, CloudReportsEnv, CloudReportsDefault)
}

// Where the app power line reads from. The off markers belong to the wrapper
// that stops an app and the ledgers to the release bot, both under the same
// shared directory as the patrol's reports. The variables are the ones those two
// already read (GT_OFF_DIR for the wrapper, GT_CLOUD_RELEASES_DIR as the
// release-reporter plugin's run.sh does), so the line follows them without a
// setting of its own.

const (
	// CloudOffDefault is the directory holding one <app>.off marker per stopped app.
	CloudOffDefault = "/Users/Shared/gt-cloud/off"
	// CloudOffEnv is the variable that overrides it.
	CloudOffEnv = "GT_OFF_DIR"
	// CloudReleasesDefault is the directory holding the release bot's <app>.jsonl ledgers.
	CloudReleasesDefault = "/Users/Shared/gt-cloud/releases"
	// CloudReleasesEnv is the variable that overrides it.
	CloudReleasesEnv = "GT_CLOUD_RELEASES_DIR"
)

// CloudOffDir is the directory to read the off markers from: $GT_OFF_DIR when it
// is set, and the wrapper's own otherwise.
func CloudOffDir() string { return cloudOffDir(os.Getenv) }

// CloudReleasesDir is the directory to read the release bot's ledgers from:
// $GT_CLOUD_RELEASES_DIR when it is set, and the bot's own otherwise.
func CloudReleasesDir() string {
	return cloudReleasesDir(os.Getenv)
}

// cloudOffDir is CloudOffDir with its environment injected.
func cloudOffDir(getenv func(string) string) string {
	return envDir(getenv, CloudOffEnv, CloudOffDefault)
}

// cloudReleasesDir is CloudReleasesDir with its environment injected.
func cloudReleasesDir(getenv func(string) string) string {
	return envDir(getenv, CloudReleasesEnv, CloudReleasesDefault)
}

// CloudConfig is settings/config.json's "cloud" block: where the town's cloud
// step reads the patrol's report, its heartbeat and the release ledgers. The
// block is a setting rather than a GT_ variable because the environment-read
// guard forbids a new one and the same settings are read on both sides of the
// container boundary.
type CloudConfig struct {
	// Source is the directory the file source reads. Set, it replaces both of
	// the environment-named ledger directories.
	Source string `json:"source,omitempty"`
	// LedgerDSNFile is the file holding the Postgres DSN, readable by the
	// owner alone. Set, it selects Postgres and Source is ignored.
	LedgerDSNFile string `json:"ledger_dsn_file,omitempty"`
}

// CloudNotConfigured is what the cloud line and the plugin findings say for a
// cloud block that is present but empty: the step is off, not broken.
const CloudNotConfigured = "cloud step not configured"

// UnmarshalJSON decodes the block strictly: a mistyped key must be an error,
// not an empty block that turns the cloud step off without the operator
// noticing. The unknown-field check is repeated here because a type that
// decodes itself is decoded by the config kernel's strict decoder opaquely.
func (c *CloudConfig) UnmarshalJSON(data []byte) error {
	type plain CloudConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("cloud: %w", err)
	}
	*c = CloudConfig(p)
	return nil
}

// CloudSettings is where the cloud step reads from, resolved from the town
// settings and, for a town with no block, from the environment the readers have
// always followed.
type CloudSettings struct {
	// Off is a present but empty block: the cloud step is off.
	Off bool
	// Postgres selects the Postgres source, whose DSN is read from DSNFile.
	Postgres bool
	DSNFile  string
	// ReportsDir and ReleasesDir are the file source's directories.
	ReportsDir  string
	ReleasesDir string
	// OffDir is where the app power line reads its off markers; the markers
	// belong to the wrapper on the machine, so they stay files under every
	// source.
	OffDir string
}

// ResolveCloud returns the cloud step's settings. A town with no cloud block
// keeps the environment-named directories the readers had before the block
// existed; a present but empty block is off; ledger_dsn_file selects Postgres
// and otherwise source is the file source's one directory.
func (s *TownSettings) ResolveCloud(getenv func(string) string) CloudSettings {
	cs := CloudSettings{
		ReportsDir:  cloudReportsDir(getenv),
		ReleasesDir: cloudReleasesDir(getenv),
		OffDir:      cloudOffDir(getenv),
	}
	if s == nil || s.Cloud == nil {
		return cs
	}
	switch {
	case s.Cloud.LedgerDSNFile != "":
		cs.Postgres, cs.DSNFile = true, s.Cloud.LedgerDSNFile
	case s.Cloud.Source == "":
		cs.Off = true
		cs.ReportsDir, cs.ReleasesDir = "", ""
	default:
		cs.ReportsDir, cs.ReleasesDir = s.Cloud.Source, s.Cloud.Source
	}
	return cs
}

// envDir is the directory the variable names, or def when it is unset or blank.
// The environment is injected so a test can pin it without mutating the process
// environment.
func envDir(getenv func(string) string, name, def string) string {
	if dir := strings.TrimSpace(getenv(name)); dir != "" {
		return dir
	}
	return def
}
