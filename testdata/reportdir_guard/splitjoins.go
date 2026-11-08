// This file is a second reports-path guard fixture: it builds a report's path
// in two joins, through a variable, which
// TestPathGuardFailsOnAFixtureOutsideThePackage must catch. It lives under
// testdata, so the Go tool never builds it.
package reportdirguard

import "path/filepath"

// SplitReportPath builds the path the guard refuses, one join at a time.
func SplitReportPath(runtimeDir, bird string) string {
	dir := filepath.Join(runtimeDir, "reports")
	return filepath.Join(dir, bird, "daily.json")
}
