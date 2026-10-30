package openshellprov

import (
	"fmt"
	"io"
	"slices"
)

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
