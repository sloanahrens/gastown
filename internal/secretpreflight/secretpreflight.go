// Package secretpreflight names the Secrets the daemon's render needs that a
// cluster does not hold (gt-f2z3mt). Secrets stay operator-created: the render
// names each one and never creates it, so a plan must know which are absent
// before it applies. The check reads metadata only: the default
// `kubectl get secret` table carries name, type, data count and age, and none
// of the data.
//
// Kubernetes RBAC cannot separate existence from data, so reaching this table
// needs `get` on the named Secrets (ticket claude-30uf); the code keeps to
// metadata by never asking for an output format that carries the values.
package secretpreflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Command is the program the check runs.
const Command = "kubectl"

// Runner runs name with args and returns its combined output. A failing
// command's error reports its exit status through ExitCode(), as
// *exec.ExitError does, and the output carries what the command wrote about the
// failure.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Missing returns, sorted, the names from names that the cluster behind
// kubeContext does not hold as Secrets in namespace. A get that exits 1 and
// reports NotFound is the only answer read as missing; every other failure is
// an error naming the Secret, because an answer the check could not read must
// not pass as absent or as present.
func Missing(ctx context.Context, run Runner, kubeContext, namespace string, names []string) ([]string, error) {
	if kubeContext == "" {
		return nil, errors.New("secret preflight: no cluster context, so the check cannot name the cluster it reads")
	}
	if namespace == "" {
		return nil, errors.New("secret preflight: no namespace, so the check cannot name the namespace it reads")
	}
	// Every name is judged before the first get, so a refusal leaves the
	// cluster untouched.
	for _, name := range names {
		if name == "" || strings.HasPrefix(name, "-") {
			return nil, fmt.Errorf("secret preflight: %q is not a Secret name: an empty name or one starting with '-' is not passed to kubectl", name)
		}
	}
	var missing []string
	for _, name := range names {
		argv := []string{"--context", kubeContext, "-n", namespace, "get", "secret", name}
		out, err := run(ctx, Command, argv...)
		if err != nil {
			if isNotFound(out, err) {
				missing = append(missing, name)
				continue
			}
			return nil, fmt.Errorf("check Secret %s in namespace %s: %w", name, namespace, err)
		}
		if !tableNames(out, name) {
			return nil, fmt.Errorf("the get of Secret %s in namespace %s printed a table whose data row does not name it", name, namespace)
		}
	}
	slices.Sort(missing)
	return missing, nil
}

// isNotFound reports whether the failure is kubectl's NotFound: exit status 1
// with NotFound in what the command wrote. Another exit status, or NotFound
// with no exit status to read, is a different failure.
func isNotFound(out []byte, err error) bool {
	var coded interface{ ExitCode() int }
	if !errors.As(err, &coded) || coded.ExitCode() != 1 {
		return false
	}
	return strings.Contains(err.Error(), "NotFound") || bytes.Contains(out, []byte("NotFound"))
}

// tableNames reports whether the table's data row names the Secret. The header
// row is the one whose first cell is NAME, and the get names one Secret, so the
// first non-blank line under that header is the answer. Only that cell is
// compared, so neither a later column nor a line outside the data row can pass
// as the name.
func tableNames(out []byte, name string) bool {
	lines := bytes.Split(out, []byte("\n"))
	for i, line := range lines {
		if fields := bytes.Fields(line); len(fields) == 0 || string(fields[0]) != "NAME" {
			continue
		}
		for _, row := range lines[i+1:] {
			if fields := bytes.Fields(row); len(fields) > 0 {
				return string(fields[0]) == name
			}
		}
		return false
	}
	return false
}
