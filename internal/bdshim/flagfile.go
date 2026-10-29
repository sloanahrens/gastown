package bdshim

import _ "embed"

// RecordedFlagFile is testdata/bd-flags.txt compiled into the binary, so the
// doctor check that compares it with the installed bd needs no checkout.
//
//go:embed testdata/bd-flags.txt
var RecordedFlagFile []byte
