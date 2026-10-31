// Package a is a leaf of the fixture module: no package it imports is in the
// module, so a change to it can reach every importer only through them.
package a

// Value is a fixture value the other packages read.
const Value = 1
