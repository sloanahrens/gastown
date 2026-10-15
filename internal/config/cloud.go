package config

import (
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
func CloudOffDir() string { return envDir(os.Getenv, CloudOffEnv, CloudOffDefault) }

// CloudReleasesDir is the directory to read the release bot's ledgers from:
// $GT_CLOUD_RELEASES_DIR when it is set, and the bot's own otherwise.
func CloudReleasesDir() string {
	return envDir(os.Getenv, CloudReleasesEnv, CloudReleasesDefault)
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
