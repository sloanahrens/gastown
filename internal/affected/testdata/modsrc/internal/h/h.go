// Package h imports g, so g's test binary rebuilds h for the test and a change
// to h reaches g.
package h

import "example.com/m/internal/g"

// Value is a fixture value derived from g.
var Value = g.Value + 1
