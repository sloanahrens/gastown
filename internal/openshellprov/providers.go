package openshellprov

import (
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
)

// seatProviderPrefix begins the provider name and the key file a seat's Forgejo
// provider is built from.
const seatProviderPrefix = "forgejo-seat-"

// maxSeatNameLength is the longest seat name, in characters.
const maxSeatNameLength = 31

// seatName is the form a seat name must have: lower-case letters and digits in
// groups joined by single hyphens (gt-t1yp6a).
var seatName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// CreateSeatProviders creates provider forgejo-seat-NAME of type forgejo-polecat
// for each name, from the key file forgejo-seat-NAME.env in the config directory.
// Every name is checked before the first openshell call, so a refused name
// creates nothing; the first seat whose file is missing or sets no token stops
// the run, and an existing provider is left alone.
func (r *Run) CreateSeatProviders(names []string) error {
	for _, name := range names {
		if len(name) > maxSeatNameLength || !seatName.MatchString(name) {
			return fmt.Errorf("%s: seat name %q must be at most %d characters of lower-case letters and digits in groups joined by single hyphens", Name, name, maxSeatNameLength)
		}
	}
	for _, name := range names {
		provider := seatProviderPrefix + name
		file := filepath.Join(r.Paths.ConfigDir, provider+".env")
		if err := r.CreateProvider(provider, "forgejo-polecat", file, "FORGEJO_TOKEN", "FORGEJO_TOKEN"); err != nil {
			return fmt.Errorf("provider %s: %w", provider, err)
		}
	}
	return nil
}

// CreateProvider creates provider name of type typ, unless the gateway already
// lists it. The secret is the value of key in file, and it reaches openshell only
// as the value of the environment variable credential in the child's environment:
// argv names the variable, so the secret never appears in a command line.
func (r *Run) CreateProvider(name, typ, file, key, credential string) error {
	exists, err := r.Client.ProviderExists(name)
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(r.Out, "%s: provider %s exists\n", Name, name)
		return nil
	}
	data, err := r.Files.ReadFile(file)
	if err != nil {
		return fmt.Errorf("%s: %s is unreadable, so provider %s cannot be created", Name, file, name)
	}
	secret := ParseKeyValue(data, key)
	if secret == "" {
		return fmt.Errorf("%s: %s does not set %s", Name, file, key)
	}
	args := r.Client.args("provider", "create", "--name", name, "--type", typ, "--credential", credential)
	env := append(slices.Clone(r.Env), credential+"="+secret)
	stdout, stderr, runErr := r.Client.Run.Run(openshellBinary, args, env, nil)
	if err := writeMasked(secret, stdout, r.Out); err != nil {
		return err
	}
	if err := writeMasked(secret, stderr, r.Err); err != nil {
		return err
	}
	// The error names the command and not its output, which may hold the secret.
	if runErr != nil {
		return fmt.Errorf("%s: openshell provider create --name %s failed", Name, name)
	}
	return nil
}

// writeMasked writes b to w with every occurrence of secret replaced by ***.
func writeMasked(secret string, b []byte, w io.Writer) error {
	m := Masked(secret, w)
	if _, err := m.Write(b); err != nil {
		return fmt.Errorf("%s: writing openshell's output: %w", Name, err)
	}
	if err := m.Close(); err != nil {
		return fmt.Errorf("%s: writing openshell's output: %w", Name, err)
	}
	return nil
}
