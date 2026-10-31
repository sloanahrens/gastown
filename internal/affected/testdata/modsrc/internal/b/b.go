// Package b imports a, so it depends on a directly.
package b

import "example.com/m/internal/a"

// Value is a fixture value derived from a.
var Value = a.Value + 1
