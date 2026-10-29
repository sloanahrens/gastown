// Package pgdsn edits a libpq connection string, a URI or a keyword string,
// without dropping the settings a child command needs.
//
// A tool that runs psql's siblings passes the connection string to them as an
// argument. Password settings are the one thing that must not travel that way,
// because every local user can read a process's arguments; the functions here
// take them out and leave sslmode and the rest in place.
package pgdsn

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// isURI reports whether dsn is a URI rather than a keyword string.
func isURI(dsn string) bool { return strings.Contains(dsn, "://") }

// keywordPassword matches a password setting in a libpq keyword string.
var keywordPassword = regexp.MustCompile(`(?i)\s*\bpassword\s*=\s*('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|\S+)`)

// keywordDatabase matches a dbname setting in a libpq keyword string.
var keywordDatabase = regexp.MustCompile(`(?i)\s*\bdbname\s*=\s*('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|\S+)`)

// nameRE is what a database name may be here: the scratch names the callers
// build are plain identifiers, and anything else could end up splicing a
// setting into a keyword string.
var nameRE = regexp.MustCompile(`^[a-z0-9_]+$`)

// WithoutPassword returns dsn with every password setting removed and every
// other setting kept. The error never quotes the DSN: a parse failure can echo
// a password.
func WithoutPassword(dsn string) (string, error) {
	if !isURI(dsn) {
		return keywordPassword.ReplaceAllString(dsn, ""), nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errors.New("cannot read the connection string as a URI")
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	// A password can also ride in the query string (?password=...).
	q := u.Query()
	q.Del("password")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Password returns the password dsn carries, or "" when it carries none. Only
// the string is read: a password from the environment stays the environment's
// business.
func Password(dsn string) (string, error) {
	if !isURI(dsn) {
		m := keywordPassword.FindStringSubmatch(dsn)
		if m == nil {
			return "", nil
		}
		return unquote(m[1]), nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errors.New("cannot read the connection string as a URI")
	}
	if u.User == nil {
		return "", nil
	}
	password, _ := u.User.Password()
	if password != "" {
		return password, nil
	}
	return u.Query().Get("password"), nil
}

// unquote removes the quotes around a libpq keyword value and undoes the
// backslash escapes inside them.
func unquote(v string) string {
	if len(v) < 2 || (v[0] != '\'' && v[0] != '"') || v[len(v)-1] != v[0] {
		return v
	}
	body := v[1 : len(v)-1]
	body = strings.ReplaceAll(body, `\\`, "\\")
	return strings.ReplaceAll(body, `\`+string(v[0]), string(v[0]))
}

// WithDatabase returns dsn with its database set to name and every other
// setting kept, so a child command reaches another database with the same TLS
// and session settings as the string it was given. name must be lower-case
// letters, digits and underscores.
func WithDatabase(dsn, name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("database name %q is not lower-case letters, digits and underscores", name)
	}
	if !isURI(dsn) {
		rest := strings.TrimSpace(keywordDatabase.ReplaceAllString(dsn, ""))
		if rest == "" {
			return "dbname=" + name, nil
		}
		return rest + " dbname=" + name, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errors.New("cannot read the connection string as a URI")
	}
	u.Path = "/" + name
	q := u.Query()
	q.Del("dbname")
	u.RawQuery = q.Encode()
	return u.String(), nil
}
