// Package g has an internal test, which makes go list rebuild it for its test
// binary, and an external test that imports h, which imports g. h is therefore
// rebuilt specially for g's test binary and reported as "h [g.test]": the
// recompiled-dependency shape whose ForTest names a package other than its own.
package g

// Value is a fixture value.
const Value = 7
