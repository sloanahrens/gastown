package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// directiveHeader opens every directive section in a system-prompt file: the
// line renderDirectiveSection writes before each directive.
const directiveHeader = "## Directive: "

// directivePointer is the hook's one line in place of the directive text when
// the agent's system-prompt file carries the directives (gt-gh2jw). It names the
// file's own size and sha256, the bytes the runtime uploaded, so the line is the
// file's hash and changes with it (gt-uhzl7). It is "" when the file is not in
// use or holds no directive section, and the caller then prints the capped text.
func directivePointer(promptPath string) string {
	if promptPath == "" {
		return ""
	}
	data, err := os.ReadFile(promptPath)
	if err != nil || !strings.Contains(string(data), directiveHeader) {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("Directives: in your system prompt (%d bytes, sha256 %s)",
		len(data), hex.EncodeToString(sum[:])[:12])
}
