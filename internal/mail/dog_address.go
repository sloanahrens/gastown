package mail

import (
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
)

// dogAddressPrefix is the mail namespace the retired LLM dogs used; their
// mailboxes may still hold messages.
const dogAddressPrefix = "deacon/dogs/"

// DogAddress returns the canonical mail address for a named dog.
func DogAddress(name string) string {
	if !isSafeDogName(name) {
		return ""
	}
	return dogAddressPrefix + name
}

// DogAddressName extracts the dog name from a canonical deacon/dogs/<name> address.
func DogAddressName(address string) (string, bool) {
	if !strings.HasPrefix(address, dogAddressPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(address, dogAddressPrefix)
	if !isSafeDogName(name) {
		return "", false
	}
	return name, true
}

func dogAddressFromAgentBeadID(id string) string {
	for _, prefix := range []string{session.HQPrefix + "dog-", "gt-dog-"} {
		if strings.HasPrefix(id, prefix) {
			return DogAddress(strings.TrimPrefix(id, prefix))
		}
	}
	return ""
}

func isDogAgentBeadIDWithoutName(id string) bool {
	return id == session.HQPrefix+"dog" || id == "gt-dog"
}

func dogAddressFromParts(parts []string, dogIndex int) string {
	if dogIndex+1 >= len(parts) {
		return ""
	}
	return DogAddress(strings.Join(parts[dogIndex+1:], "-"))
}

func isSafeDogName(name string) bool {
	return name != "" &&
		name != "." &&
		name != ".." &&
		!strings.ContainsAny(name, `/\\`) &&
		!strings.Contains(name, "..")
}

func isReservedTownSubpath(address string) bool {
	return strings.HasPrefix(address, constants.RoleMayor+"/") ||
		strings.HasPrefix(address, "deacon/")
}
