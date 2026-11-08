// Package reportdirimports is a fixture for the reportdir import guard: it
// imports the bird pool, which TestReportdirImports must see in a dependency
// walk. It lives under testdata, so the Go tool never builds it.
package reportdirimports

import (
	_ "github.com/steveyegge/gastown/internal/bird"
)
