package townstate

import (
	"path"
	"strings"
)

// SecretShape reports whether a file name has the shape of a secret: the
// lower-cased base name ends in .env, .key, .pem or .dsn, or holds "token" or
// "secret". A backup export leaves a secret-shaped entry out and counts it
// under SkipSecret without naming it.
//
// .dsn is caught because deploy/daemon/secrets.yaml mounts ledger-reader.dsn, a
// Postgres DSN whose file holds the reader role's password: the guard over that
// manifest fails without it, and a backup would carry the DSN verbatim
// (gt-4kf6kk).
func SecretShape(name string) bool {
	base := strings.ToLower(path.Base(name))
	switch {
	case strings.HasSuffix(base, ".env"),
		strings.HasSuffix(base, ".key"),
		strings.HasSuffix(base, ".pem"),
		strings.HasSuffix(base, ".dsn"):
		return true
	}
	return strings.Contains(base, "token") || strings.Contains(base, "secret")
}

// secretPath reports whether any segment of a town-root-relative path has the
// shape of a secret. A directory such as secrets/ or token-store/ therefore
// leaves out everything below it, so a credential file with an ordinary name
// under a secret-shaped directory is not carried either.
func secretPath(name string) bool {
	for _, seg := range strings.Split(name, "/") {
		if SecretShape(seg) {
			return true
		}
	}
	return false
}
