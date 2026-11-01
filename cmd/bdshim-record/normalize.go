package main

import "github.com/steveyegge/gastown/internal/bdshim/golden"

// normalizeGolden is the recorder's name for the normalization the committed goldens hold. The list
// lives in internal/bdshim/golden so the shim's differential test normalizes a handler's answer with
// the same one instead of a copy of it (gt-p85pw.118).
func normalizeGolden(stdout []byte, keep map[string]bool) []byte {
	return golden.Normalize(stdout, keep)
}
