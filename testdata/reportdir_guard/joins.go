// Package reportdirguard is a fixture for the reports-path guard: it joins the
// reports directory with a daily file by hand, which
// TestPathGuardFailsOnAFixtureOutsideThePackage must catch. It lives under
// testdata, so the Go tool never builds it.
package reportdirguard

import "path/filepath"

// ReportPath builds the path the guard refuses.
func ReportPath(runtimeDir, bird string) string {
	return filepath.Join(runtimeDir, "reports", bird, "daily.json")
}
