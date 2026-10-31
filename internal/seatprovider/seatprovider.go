// Package seatprovider creates and deletes the tracker provider that holds one
// seat's token, so the real token never has to enter the seat's sandbox.
//
// Nothing calls this package yet. Its callers are the seat start and seat end
// paths and the reaper, which arrive in the third build bead and the wiring
// after it (design: docs/research/tracker/K1b-seat-token-as-provider-placeholder.md,
// question 1, option A).
package seatprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Name is the prefix of every error this package returns.
const Name = "seat-provider"

// prefix starts every provider name this package creates, so Sweep can tell its
// own providers from every other provider on the gateway.
const prefix = "seat-"

// openshellBinary is the program every provider call runs.
const openshellBinary = "openshell"

// tokenEnv is the variable provider create reads the credential from. argv names
// the variable and never the token, so the token cannot appear in a process
// listing or in the recorded command line.
const tokenEnv = "TRACKER_TOKEN"

// providerType is the openshell profile id the provider is created from, which
// names the tracker profile gt-0f305 writes.
const providerType = "tracker-polecat"

// Runner runs one provider CLI call and returns its stdout. It is the only path
// to the gateway, so a unit test runs no process; env holds the variables added
// to the environment the call inherits.
type Runner interface {
	Run(ctx context.Context, argv []string, env []string) (stdout []byte, err error)
}

// NameFor returns the provider name of a seat's provider. The name depends on the
// seat address alone, so a restart and the reaper compute the name the seat start
// used. The suffix digests the exact address, so two addresses that sanitise to
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
	sum := sha256.Sum256([]byte(seat))
	return prefix + strings.Trim(b.String(), "-") + "-" + hex.EncodeToString(sum[:4])
}

// Create creates the provider that holds seat's token and returns its name. A
// provider already at that name, left by a crashed seat, holds the token that
// seat started with, so it is deleted and created again from this one.
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
		if err := deleteProvider(ctx, r, name); err != nil {
			return "", err
		}
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
// survives, and so does every provider without this package's prefix. A failed
// delete stops the sweep, which returns the names it deleted before it.
func Sweep(ctx context.Context, r Runner, live map[string]bool) ([]string, error) {
	names, err := listProviders(ctx, r)
	if err != nil {
		return nil, err
	}
	liveNames := make(map[string]bool, len(live))
	for seat := range live {
		liveNames[NameFor(seat)] = true
	}
	var deleted []string
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) || liveNames[name] {
			continue
		}
		if err := deleteProvider(ctx, r, name); err != nil {
			return deleted, err
		}
		deleted = append(deleted, name)
	}
	return deleted, nil
}

// createArgs returns the argv that creates name. The credential is the variable
// tokenEnv, which the caller sets in the call's environment.
func createArgs(name string) []string {
	return []string{"provider", "create", "--name", name, "--type", providerType, "--credential", tokenEnv}
}

// deleteArgs returns the argv that deletes name. The -- ends option parsing, so a
// name is never read as a flag. openshell's positional-name form is unverified
// until the design's live proof runs; Create's --name form is the one a profile
// in this repo shows.
func deleteArgs(name string) []string {
	return []string{"provider", "delete", "--", name}
}

// listProviders returns the provider names the gateway lists, in the order it
// prints them, taken from the first column of each line.
func listProviders(ctx context.Context, r Runner) ([]string, error) {
	out, err := r.Run(ctx, []string{"provider", "list"}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: listing providers failed, so which providers exist is unknown", Name)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			names = append(names, fields[0])
		}
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

// contains reports whether names holds want.
func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
