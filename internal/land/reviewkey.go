package land

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// absentSHA is fileSHA's answer for a file that does not exist. A digest is 64
// hex digits, so the word never collides with one (gt-vq81d1).
const absentSHA = "absent"

// fileSHA is the hex sha256 of the file at path, or absentSHA when no file is
// there. Any other read failure is an error: an unreadable input never reads as
// absent (gt-vq81d1).
func fileSHA(path string) (string, error) {
	return fileSHAFrom(os.ReadFile, path)
}

// fileSHAFrom is fileSHA over the read it is given, so a test can make the read
// fail without touching the filesystem (gt-vq81d1).
func fileSHAFrom(read func(string) ([]byte, error), path string) (string, error) {
	data, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return absentSHA, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// omRepoConfigAt is the text of .om.json at base and the text of the file its
// context_file key names, both read through g. base is the landing's base
// commit, the one OMThresholdAt reads the bar from. A missing context_file key
// gives an empty contextFile and no error. A missing .om.json, invalid JSON, an
// empty context_file value, or a named file that is missing or unreadable is an
// error, and both texts come back empty (gt-vq81d1).
func omRepoConfigAt(g Repo, base string) (omJSON, contextFile string, err error) {
	if g == nil {
		return "", "", fmt.Errorf("reading %s at %s: no repository to read it from", omConfigFile, base)
	}
	omJSON, err = g.ShowFileAtRev(base, omConfigFile)
	if err != nil {
		return "", "", fmt.Errorf("reading %s at %s: %w", omConfigFile, base, err)
	}
	var cfg struct {
		ContextFile *string `json:"context_file"`
	}
	if err = json.Unmarshal([]byte(omJSON), &cfg); err != nil {
		return "", "", fmt.Errorf("parsing %s at %s: %w", omConfigFile, base, err)
	}
	if cfg.ContextFile == nil {
		return omJSON, "", nil
	}
	if *cfg.ContextFile == "" {
		return "", "", fmt.Errorf("%s at %s names an empty context_file", omConfigFile, base)
	}
	contextFile, err = g.ShowFileAtRev(base, *cfg.ContextFile)
	if err != nil {
		return "", "", fmt.Errorf("reading context_file %s at %s: %w", *cfg.ContextFile, base, err)
	}
	return omJSON, contextFile, nil
}
