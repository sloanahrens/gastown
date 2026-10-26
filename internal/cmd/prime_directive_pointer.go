package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// directivePointer is the hook's one line in place of the directive text when
// the agent's system-prompt file carries the directives (gt-gh2jw). It is "" when
// the file is not in use, or when it does not contain the directive section,
// and the caller then prints the capped text as before.
//
// The line is computed from the same renderer the file was written with
// (renderDirectiveSection over config.ReadRoleDirectives), and it is printed only
// when the file really holds that section, so the pointer cannot name a policy
// the file lacks.
func directivePointer(ctx RoleContext, promptPath string) string {
	if promptPath == "" {
		return ""
	}
	if info, err := os.Stat(promptPath); err != nil || info.IsDir() {
		return ""
	}
	directives, err := config.ReadRoleDirectives(string(ctx.Role), ctx.TownRoot, ctx.Rig)
	if err != nil || len(directives) == 0 {
		return ""
	}
	section := renderDirectiveSection(directives)
	file, err := os.ReadFile(promptPath)
	if err != nil || !strings.Contains(string(file), section) {
		return ""
	}
	sum := sha256.Sum256([]byte(section))
	return fmt.Sprintf("Directives: in your system prompt (%d chars, sha256 %s)",
		len(section), hex.EncodeToString(sum[:])[:12])
}
