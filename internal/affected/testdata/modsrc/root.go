// Package m sits at the module root, so a file outside every package cannot be
// placed by climbing to "." and mistaking this package for its owner.
package m

// Value is a fixture value.
const Value = 0
