package land

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/constants"
)

// LandingsFile is a rig's append-only landing log: one JSON LandingRecord per
// line, written by the landing worker after the target's tip is read back.
// It is the record nothing reaps (MR wisps were reaped within a day, so no
// landing survived before D2) and the list the red-main owner and the
// branch-protection audit read "who landed what" from.
type LandingsFile struct {
	Path string
}

// RigLandingsFile is <town>/.runtime/landings/<rig>.jsonl.
func RigLandingsFile(townRoot, rig string) (*LandingsFile, error) {
	if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." {
		return nil, fmt.Errorf("invalid rig name %q for a landings file", rig)
	}
	return &LandingsFile{Path: filepath.Join(constants.TownRuntimePath(townRoot), "landings", rig+".jsonl")}, nil
}

// Append writes rec as one line. The directory must be 0700-safe (owned by
// this user, not group- or world-writable) and the file must not be a
// symlink, so no other account can redirect or forge the log.
func (f *LandingsFile) Append(rec LandingRecord) error {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating landings dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("checking landings dir: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("refusing landings dir %s: mode %v is not a private directory", dir, info.Mode())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing landings dir %s: owned by uid %d, not %d", dir, st.Uid, os.Getuid())
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding landing record: %w", err)
	}
	line = append(line, '\n')
	fh, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("opening landings file: %w", err)
	}
	if _, err := fh.Write(line); err != nil {
		_ = fh.Close()
		return fmt.Errorf("appending landing record: %w", err)
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		return fmt.Errorf("syncing landings file: %w", err)
	}
	return fh.Close()
}
