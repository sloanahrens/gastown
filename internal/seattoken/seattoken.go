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
// given. With no file it asks issuer for one and writes it, and a failed issue
// removes the file it created, leaving the seat as it found it.
func (s *Store) Ensure(ctx context.Context, seat intent.Seat, issuer Issuer) (string, error) {
	if err := s.check(seat); err != nil {
		return "", err
	}
	if issuer == nil {
		return "", errors.New("seattoken: no issuer")
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("seattoken: token directory: %w", err)
	}
	f, created, err := createTokenFile(path)
	if err != nil {
		return "", err
	}
	if !created {
		// Another launch of this seat won the create; its file is the token.
		return path, nil
	}
	token, err := issuer.Issue(ctx, seat.String())
	value := strings.TrimSpace(token)
	if err == nil && value == "" {
		err = errors.New("the issuer returned an empty token")
	}
	if err == nil {
		_, err = f.WriteString(value + "\n")
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("seattoken: issue a token for %s: %w", seat.String(), err)
	}
	return path, nil
}

// Remove deletes seat's token file; a seat that has none is a success.
func (s *Store) Remove(seat intent.Seat) error {
	if err := s.check(seat); err != nil {
		return err
	}
	path := s.Path(seat)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("seattoken: remove token file %s: %w", path, err)
	}
	return nil
}

// readTokenFile returns the token path holds and whether the file exists; a
// file that cannot be read is an error rather than an absent one.
func readTokenFile(path string) (string, bool, error) {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("seattoken: read token file %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), true, nil
}

// createTokenFile creates path with mode 0600 and returns the open file. When
// another process created it between the read and this call, created is false
// and the file that process wrote is left alone.
func createTokenFile(path string) (f *os.File, created bool, err error) {
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return f, true, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, false, fmt.Errorf("seattoken: create token file %s: %w", path, err)
	}
	held, exists, rerr := readTokenFile(path)
	if rerr != nil {
		return nil, false, rerr
	}
	if !exists || held == "" {
		return nil, false, fmt.Errorf("seattoken: %s exists but holds no token; refusing to take it over", path)
	}
	return nil, false, nil
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
