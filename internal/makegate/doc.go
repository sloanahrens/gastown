// Package makegate holds the Makefile's gate contract as Go tests, one recipe
// family per test file. The gate-shape cases read the recipe shape with the
// checkout's real make, so every test file is integration-tagged; an untagged
// run reports no test files and says nothing about the Makefile.
//
// This file carries no build tag: it is the package's only non-test file, and
// with every file tag-excluded the untagged `go test` fails with "build
// constraints exclude all Go files" instead of reporting no test files.
package makegate
