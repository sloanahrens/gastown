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

// backupSecretEntry reports whether a backup export leaves out the entry at a
// town-root-relative name: the entry's own name has a secret shape, or a
// directory above it does. The first segment of a path with more is a row root
// (a rig), and a top-level directory is a row root too: neither is dropped by
// its name, so a rig whose own name holds "token" or "secret" keeps its rows,
// while settings/secrets/ and anything below it is left out (gt-4kf6kk).
func backupSecretEntry(name string, isDir bool) bool {
	segs := strings.Split(name, "/")
	if len(segs) == 1 && isDir {
		return false
	}
	for i, seg := range segs {
		if i == 0 && len(segs) > 1 {
			continue
		}
		if SecretShape(seg) {
			return true
		}
	}
	return false
}
