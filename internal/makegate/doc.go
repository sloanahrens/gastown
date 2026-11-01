// Package makegate is the Go port of scripts/makefile-gate_test.sh, one recipe
// family per test file. Every test runs the checkout's real make, so every test
// file is integration-tagged; an untagged run of this package reports no test
// files and says nothing about the Makefile.
package makegate
