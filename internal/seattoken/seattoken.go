// Package seattoken owns the per-seat tracker token: one file per seat under
// the town's runtime directory, created on first use and reused afterwards.
//
//	<town>/.runtime/seats/<rig>/<role>[.<name>].token
//
// The directory is 0700 and the file 0600, beside the seat's intent record and
// outside settings/, which a sandbox skeleton ships into a seat. Ensure returns
// the path, never the value, so a caller names it in TRACKER_TOKEN_FILE and the
// token enters neither argv nor an error (gt-p85pw.80).
package seattoken

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
)

// Store is the seat token files under one town's runtime directory.
type Store struct{ townRoot string }

// New returns the store of the town rooted at townRoot.
func New(townRoot string) *Store { return &Store{townRoot: townRoot} }

// Issuer mints a tracker token for the actor string of a seat: <rig>/<role>/<name>.
type Issuer interface {
	Issue(ctx context.Context, actor string) (string, error)
}

// Path returns seat's token file. It does not validate seat: Ensure and Remove
// refuse a seat that names no file under the town runtime.
func (s *Store) Path(seat intent.Seat) string {
	stem := seat.Role
	if seat.Name != "" {
		stem += "." + seat.Name
	}
	return filepath.Join(s.townRoot, constants.DirRuntime, "seats", seat.Rig, stem+".token")
}

// Ensure returns seat's token file path, reusing the token the seat already
// has: one token per seat for its lifetime, so a restart keeps the one it was
// given. With no file it asks issuer for one and publishes it. Two launches of
// one seat that race can each be issued a token; the one published first is
// kept, and the other launch reads it back and uses it.
//
// A symlink or a file in the way of the seat's directories or of the token
// path is refused, never followed.
func (s *Store) Ensure(ctx context.Context, seat intent.Seat, issuer Issuer) (string, error) {
	if err := s.check(seat); err != nil {
		return "", err
	}
	if issuer == nil {
		return "", errors.New("seattoken: no issuer")
	}
	if err := makeSeatDirs(s.seatDirs(seat)); err != nil {
		return "", err
	}
	path := s.Path(seat)
	held, exists, err := readTokenFile(path)
	if err != nil {
		return "", err
	}
	if exists && held != "" {
		return path, nil
	}
	if exists {
		// An interrupted earlier run left the file empty. A token file never
		// holds nothing, so it is reclaimed rather than reused.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("seattoken: reclaim empty token file %s: %w", path, err)
		}
	}
	token, err := issuer.Issue(ctx, seat.String())
	value := strings.TrimSpace(token)
	if err == nil && value == "" {
		err = errors.New("the issuer returned an empty token")
	}
	if err != nil {
		return "", fmt.Errorf("seattoken: issue a token for %s: %w", seat.String(), err)
	}
	created, err := publishToken(path, value)
	if err != nil {
		return "", err
	}
	if created {
		return path, nil
	}
	// Another launch of this seat published first; its file is the token.
	held, exists, err = readTokenFile(path)
	if err != nil {
		return "", err
	}
	if !exists || held == "" {
		return "", fmt.Errorf("seattoken: %s exists but holds no token; refusing to use it", path)
	}
	return path, nil
}

// Remove deletes seat's token file; a seat that has none is a success. A
// symlink or a file where one of the seat's directories should be is refused,
// so the delete never reaches a file outside the runtime directory.
func (s *Store) Remove(seat intent.Seat) error {
	if err := s.check(seat); err != nil {
		return err
	}
	for _, dir := range s.seatDirs(seat) {
		exists, err := checkDir(dir)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
	}
	path := s.Path(seat)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("seattoken: remove token file %s: %w", path, err)
	}
	return nil
}

// seatDirs lists the directories from the runtime directory down to seat's,
// outermost first.
func (s *Store) seatDirs(seat intent.Seat) []string {
	runtime := filepath.Join(s.townRoot, constants.DirRuntime)
	seats := filepath.Join(runtime, "seats")
	return []string{runtime, seats, filepath.Join(seats, seat.Rig)}
}

// makeSeatDirs creates each of dirs that is missing, outermost first, at 0700,
// and requires every one to be a real directory once it is in place.
func makeSeatDirs(dirs []string) error {
	for _, dir := range dirs {
		exists, err := checkDir(dir)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("seattoken: token directory: %w", err)
		}
		if _, err := checkDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// checkDir reports whether dir exists. It uses Lstat, so a symlink is refused
// rather than followed: a token written through one would land wherever it
// points.
func checkDir(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("seattoken: token directory %s: %w", dir, err)
	case !info.IsDir():
		return false, fmt.Errorf("seattoken: %s is not a real directory (a symlink or a file); refusing to use it", dir)
	}
	return true, nil
}

// readTokenFile returns the token path holds and whether a file is there. A
// symlink or other non-regular file is an error, not a token to reuse or
// replace, and so is a file that cannot be read.
func readTokenFile(path string) (string, bool, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("seattoken: read token file %s: %w", path, err)
	case !info.Mode().IsRegular():
		return "", false, fmt.Errorf("seattoken: %s is not a regular file (a symlink or a directory); refusing to use it", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("seattoken: read token file %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), true, nil
}

// publishToken makes path hold value in one step: the value goes to a
// temporary file beside path, which is hard-linked into place, and the link
// fails when path already exists. A file at path is therefore never seen empty
// or partly written. created reports whether this call published the file; the
// temporary name is removed on every return.
func publishToken(path, value string) (created bool, err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return false, fmt.Errorf("seattoken: write token file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.WriteString(value + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, fmt.Errorf("seattoken: write token file in %s: %w", dir, err)
	}
	switch err := os.Link(tmp.Name(), path); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	default:
		return false, fmt.Errorf("seattoken: create token file %s: %w", path, err)
	}
}

// check refuses a seat that names no file under .runtime/seats: an empty town
// root or role names none, and a part holding a separator would put the token
// outside the runtime directory.
func (s *Store) check(seat intent.Seat) error {
	if s.townRoot == "" {
		return errors.New("seattoken: no town root")
	}
	if seat.Role == "" {
		return errors.New("seattoken: seat names no role")
	}
	for _, part := range []string{seat.Rig, seat.Role, seat.Name} {
		if strings.ContainsAny(part, `/\`) {
			return fmt.Errorf("seattoken: seat %q holds a path separator", seat)
		}
	}
	if seat.Rig == "." || seat.Rig == ".." {
		return fmt.Errorf("seattoken: seat %q has a rig that is not one directory name", seat)
	}
	return nil
}
