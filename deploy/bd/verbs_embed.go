package bd

import _ "embed"

// VerbsJSON is verbs.json as the binary carries it, so a caller classifies a
// call from the one file the gateway image mounts instead of a second table
// (gt-s18esd). The tests of internal/bdgateway hold it equal to the file on
// disk, so a stale copy fails there rather than misclassifying a call.
//
//go:embed verbs.json
var VerbsJSON []byte
