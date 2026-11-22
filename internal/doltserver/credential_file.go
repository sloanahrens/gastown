package doltserver

import (
	"path/filepath"

	configpkg "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltcred"
)

// townCredentialFileName is the town's Dolt credential file. It sits in the
// settings directory beside config.json, so a town that has no such file has no
// credential rather than a broken one.
const townCredentialFileName = "dolt-credentials"

// readTownCredential returns the Dolt credential held in the town's settings
// directory. An absent file is no credential: the zero Credential and a nil
// error. An unreadable, insecure or malformed file is an error that names the
// file and never a value; the password is not revealed here.
//
// The directory comes from config.TownSettingsPath, so this reads wherever the
// loader reads town settings and the two cannot drift apart.
func readTownCredential(townRoot string) (doltcred.Credential, error) {
	settingsDir := filepath.Dir(configpkg.TownSettingsPath(townRoot))
	return doltcred.Read(filepath.Join(settingsDir, townCredentialFileName))
}
