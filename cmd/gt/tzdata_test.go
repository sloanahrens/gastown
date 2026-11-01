//go:build !integration

package main

import (
	"go/parser"
	"go/token"
	"testing"
)

// The gt binary must embed the time zone database: the daemon's container image
// has no system one. A blank import of time/tzdata in the main package is what
// links it (time.LoadLocation then falls back to the embedded copy when the host
// has none), so the test reads main.go's imports; a unit test cannot remove the
// host's zoneinfo to prove the fallback itself.
func TestGtLinksTheEmbeddedTimeZoneDatabase(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}
	for _, imp := range f.Imports {
		if imp.Path.Value == `"time/tzdata"` && imp.Name != nil && imp.Name.Name == "_" {
			return
		}
	}
	t.Error(`cmd/gt/main.go has no blank import of "time/tzdata"; without it a gt in an image with no zoneinfo cannot load a zone such as America/Chicago`)
}
