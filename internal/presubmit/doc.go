// Package presubmit holds the checks `make presubmit` runs on a branch's own
// diff (gt-ssyxd). A check reads the diff and whatever else it judges through
// readers and returns its findings, so its unit tests run no process; the
// command that wires it into the Makefile supplies the real readers.
package presubmit
