// Package seatprovider creates and deletes the tracker provider that holds one
// seat's token, so the real token never has to enter the seat's sandbox.
//
// Nothing calls this package yet. Its callers are the seat start and seat end
// paths and the reaper, which arrive in the third build bead and the wiring
// after it (design: docs/research/tracker/K1b-seat-token-as-provider-placeholder.md,
// question 1, option A). Every call runs against openshell's active gateway.
package seatprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Name is the prefix of every error this package returns.
const Name = "seat-provider"

// namePrefix starts every provider name this package creates, and Sweep deletes
// nothing that fails ownsName. It names the package rather than the seat, so a
// provider another client called seat-something is never swept.
const namePrefix = "gt-seat-provider-"

// openshellBinary is the program every provider call runs.
const openshellBinary = "openshell"

// tokenEnv is the variable provider create reads the credential from. argv names
// the variable and never the token, so the token cannot appear in a process
// listing or in the recorded command line.
const tokenEnv = "TRACKER_TOKEN"

// providerType is the openshell profile id the provider is created from, which
// names the tracker profile gt-0f305 writes.
const providerType = "tracker-polecat"

// digestWidth is the number of hex characters of the seat digest a provider name
// ends with, which ownsName checks before Sweep deletes a provider.
const digestWidth = 8

// ErrExists reports that a provider the gateway lists already holds the name
// Create derived for a seat. Create returns it instead of deleting: the caller
// that knows the seat is dead deletes that provider and creates again.
var ErrExists = errors.New(Name + ": a provider already holds this seat's name")

// ErrNoLiveSeats reports a Sweep call whose live set names no seat. Sweep refuses
// it because a read of the live seats that failed or came back empty looks the
// same as a town with none, and deleting every provider is not recoverable. A
// caller that has established no seat is live calls SweepAll.
var ErrNoLiveSeats = errors.New(Name + ": sweeping needs the live seats; pass SweepAll to delete every provider this package created")

// Runner runs one provider CLI call and returns its stdout. It is the only path
// to the gateway, so a unit test runs no process; env holds the variables added
// to the environment the call inherits.
type Runner interface {
	Run(ctx context.Context, argv []string, env []string) (stdout []byte, err error)
}

// NameFor returns the provider name of a seat's provider. The name depends on the
// seat address alone, so a restart and the reaper compute the name the seat start
// used. The suffix digests the exact address, so two addresses that reduce to
// the same characters still get different providers.
func NameFor(seat string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(seat) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		// A seat address of nothing but punctuation still gets a name ownsName
		// matches, so Sweep can delete the provider it created.
		slug = "seat"
	}
	sum := sha256.Sum256([]byte(seat))
	return namePrefix + slug + "-" + hex.EncodeToString(sum[:4])
}

// Create creates the provider that holds seat's token and returns its name. A
// provider already at that name is ErrExists and is left alone: Create cannot
// tell a live seat from a crashed one, and the provider it would delete may be
// the one that seat's sandbox is using.
func Create(ctx context.Context, r Runner, seat, token string) (string, error) {
	if seat == "" {
		return "", fmt.Errorf("%s: a provider needs a seat address", Name)
	}
	if token == "" {
		return "", fmt.Errorf("%s: a provider for seat %s needs a token", Name, seat)
	}
	name := NameFor(seat)
	names, err := listProviders(ctx, r)
	if err != nil {
		return "", err
	}
	if contains(names, name) {
		return "", fmt.Errorf("%w: %s", ErrExists, name)
	}
	if _, err := r.Run(ctx, createArgs(name), []string{tokenEnv + "=" + token}); err != nil {
		return "", fmt.Errorf("%s: creating provider %s failed", Name, name)
	}
	return name, nil
}

// Delete removes the provider name. A name the gateway does not list is not an
// error, so a seat end that runs twice still succeeds.
func Delete(ctx context.Context, r Runner, name string) error {
	names, err := listProviders(ctx, r)
	if err != nil {
		return err
	}
	if !contains(names, name) {
		return nil
	}
	return deleteProvider(ctx, r, name)
}

// Sweep deletes every provider this package created whose seat is not a key of
// live, and returns their names in listing order. A live seat's provider
// survives. live is the caller's liveness authority: a seat is in it before
// Create runs for that seat, so a sweep cannot reap a provider still being
// created. A live set that names no seat is ErrNoLiveSeats. A failed delete
// stops the sweep, which returns the names it deleted before it.
func Sweep(ctx context.Context, r Runner, live map[string]bool) ([]string, error) {
	if len(live) == 0 {
		return nil, ErrNoLiveSeats
	}
	liveNames := make(map[string]bool, len(live))
	for seat := range live {
		liveNames[NameFor(seat)] = true
	}
	return sweep(ctx, r, liveNames)
}

// SweepAll deletes every provider this package created and returns their names in
// listing order. It exists for the caller that has established no seat is live,
// which Sweep refuses to infer from an empty live set.
func SweepAll(ctx context.Context, r Runner) ([]string, error) {
	return sweep(ctx, r, nil)
}

// sweep deletes each owned provider that is not a key of keep, which nil keeps
// none of. A failed delete stops it, with the names deleted so far.
func sweep(ctx context.Context, r Runner, keep map[string]bool) ([]string, error) {
	names, err := listProviders(ctx, r)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, name := range names {
		if !ownsName(name) || keep[name] {
			continue
		}
		if err := deleteProvider(ctx, r, name); err != nil {
			return deleted, err
		}
		deleted = append(deleted, name)
	}
	return deleted, nil
}

// createArgs returns the argv that creates name, the form internal/openshellprov
// and openshell's provider reference both use. The credential is the variable
// tokenEnv, which the caller sets in the call's environment.
func createArgs(name string) []string {
	return []string{"provider", "create", "--name", name, "--type", providerType, "--credential", tokenEnv}
}

// deleteArgs returns the argv that deletes name. openshell's provider reference
// gives delete positional names, "provider delete NAME [NAME...]", and a name
// NameFor returns starts with a letter, so none can be read as a flag.
func deleteArgs(name string) []string {
	return []string{"provider", "delete", name}
}

// listProviders returns the name of every provider the gateway lists, in the
// order it prints them. --output json is what internal/doctor/openshell_gateway_check.go
// already reads, so a listing this package cannot decode is an error rather than
// an empty list that reads as a clean gateway.
func listProviders(ctx context.Context, r Runner) ([]string, error) {
	out, err := r.Run(ctx, []string{"provider", "list", "--output", "json"}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: listing providers failed, so which providers exist is unknown", Name)
	}
	var listing struct {
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return nil, fmt.Errorf("%s: provider list did not print the JSON this package reads, so which providers exist is unknown", Name)
	}
	names := make([]string, len(listing.Providers))
	for i, p := range listing.Providers {
		names[i] = p.Name
	}
	return names, nil
}

// deleteProvider deletes name and reports no gateway output, which could echo the
// credential back.
func deleteProvider(ctx context.Context, r Runner, name string) error {
	if _, err := r.Run(ctx, deleteArgs(name), nil); err != nil {
		return fmt.Errorf("%s: deleting provider %s failed", Name, name)
	}
	return nil
}

// ownsName reports whether name is one this package created: namePrefix, then a
// seat slug, then the digest NameFor appends. Sweep deletes what it matches, so a
// provider another client named with the same prefix but no digest is left alone.
func ownsName(name string) bool {
	rest, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return false
	}
	i := strings.LastIndex(rest, "-")
	if i <= 0 || len(rest)-i-1 != digestWidth {
		return false
	}
	for _, r := range rest[i+1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// contains reports whether names holds want.
func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
