// Package seatprovider creates and deletes the tracker provider that holds one
// seat's token, so the real token never has to enter the seat's sandbox.
//
// Nothing calls this package yet. Its callers are the seat start and seat end
// paths and the reaper, which arrive in the third build bead and the wiring
// after it (seat-token design question 1, option A). Every call runs against
// openshell's active gateway.
//
// The token reaches the gateway only through the environment of the child
// process that runs "openshell provider create"; it is never in argv. A process
// of the same user can read a child's environment, which is the tradeoff of
// handing a credential to a CLI by variable name.
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

// ErrNoLiveSeats reports a Sweep whose live read named no seat. Sweep refuses it
// because a read of the live seats that came back empty looks the same as a read
// that went wrong, and deleting every provider is not recoverable. This package
// offers no way to delete every provider; a later bead adds one if a shutdown
// needs it.
var ErrNoLiveSeats = errors.New(Name + ": sweeping needs the live seats, and the live read named none")

// ErrNotOwned reports a Delete of a name this package did not create, such as
// the model provider. Delete is for the provider a seat's Create made.
var ErrNotOwned = errors.New(Name + ": not a provider this package created")

// LiveSeats reads the seats that are live now, as a set keyed by seat address.
// Sweep calls it after it has listed the providers, so a seat that registered
// before its Create began is in the set whenever its provider is in the list.
type LiveSeats func(ctx context.Context) (map[string]bool, error)

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

// Delete removes the provider name, which must be one this package created
// (ErrNotOwned otherwise, so a seat end can never delete the model provider).
// A name the gateway does not list is not an error, so a seat end that runs
// twice still succeeds.
func Delete(ctx context.Context, r Runner, name string) error {
	if !ownsName(name) {
		return fmt.Errorf("%w: %q", ErrNotOwned, name)
	}
	names, err := listProviders(ctx, r)
	if err != nil {
		return err
	}
	if !contains(names, name) {
		return nil
	}
	return deleteProvider(ctx, r, name)
}

// Sweep deletes every provider this package created whose seat is not live, and
// returns their names in listing order. It lists the providers first and reads
// the live seats second, which is what keeps it from reaping a provider whose
// seat is starting: the seat is in the live set before its Create begins, so a
// provider that Sweep's list shows belongs to a seat that the later live read
// already includes. Reading live first would open the window the other way.
//
// A live read that fails is the sweep's failure, and one that names no seat is
// ErrNoLiveSeats; nothing is deleted in either case. A failed delete stops the
// sweep, which returns the names it deleted before it.
func Sweep(ctx context.Context, r Runner, live LiveSeats) ([]string, error) {
	names, err := listProviders(ctx, r)
	if err != nil {
		return nil, err
	}
	seats, err := live(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: reading the live seats failed, so nothing was deleted", Name)
	}
	if len(seats) == 0 {
		return nil, ErrNoLiveSeats
	}
	keep := make(map[string]bool, len(seats))
	for seat := range seats {
		keep[NameFor(seat)] = true
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

// deleteArgs returns the argv that deletes name: "provider delete NAME".
//
// Source of the form: openshell 0.1.5, "openshell provider delete --help" prints
// "USAGE  openshell provider delete [OPTIONS] <NAME>..." with the flags
// --gateway and --gateway-endpoint only (read 2026-11-01). Delete and Sweep
// apply ownsName first, so the name starts with namePrefix and can never be read
// as a flag. The form has not been run against a gateway; the live proof of the
// K1b research doc runs it once.
func deleteArgs(name string) []string {
	return []string{"provider", "delete", name}
}

// listProviders returns the name of every provider the gateway lists, in the
// order it prints them. --output json is the format this function decodes, so a
// listing it cannot decode is an error rather than an empty list that reads as a
// clean gateway.
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
