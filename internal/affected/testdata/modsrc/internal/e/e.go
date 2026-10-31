// Package e imports b, so a change to a reaches e through b.
package e

import "example.com/m/internal/b"

// Value is a fixture value derived from b.
var Value = b.Value + 1
