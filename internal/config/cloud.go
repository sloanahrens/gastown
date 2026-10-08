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
	if dir := strings.TrimSpace(getenv(CloudReportsEnv)); dir != "" {
		return dir
	}
	return CloudReportsDefault
}
