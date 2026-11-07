package templates

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// LiveHMConfigPath is where the live container's hm reads its config, the path
// its HM_CONFIG names (gt-yjzepk.13). It lies under the mounted config
// directory, so the derived file shadows the Mac's copy there.
const LiveHMConfigPath = LiveConfigDirPath + "/hm-config.json"

// ContainerHMConfigRP is the derived hm config's path under the town root, where
// the compose write leaves the file. The wrapper keeps its own copy, which a test
// pins to this one, since gtwrap does not import the templates package.
const ContainerHMConfigRP = ".runtime/container/hm-config.json"

// hmDatabaseField is the hm config field holding the database URL. hm reads its
// database address from this URL alone and has no environment override for it,
// so it is the one field the container's copy changes.
const hmDatabaseField = "database_url"

// hmContainerHost is the name a container reaches the machine it runs on by,
// the same name the Dolt host and the town's extra hosts use.
const hmContainerHost = "host.docker.internal"

// hmConfigEnv is the variable hm reads its config file's path from.
const hmConfigEnv = "HM_CONFIG"

// hmConfigFileMode is the mode the derived hm config is written with: its
// database URL holds the database password, so its owner alone reads it.
const hmConfigFileMode fs.FileMode = 0o600

// RenderHMConfig derives the live container's hm config from the Mac's: the
// database URL's loopback host becomes hmContainerHost with its port, user and
// database kept, and every other field's value is copied unchanged. The result
// holds the database password, so the command writes it to a file and never
// prints it.
func RenderHMConfig(host []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(host, &doc); err != nil {
		return nil, fmt.Errorf("hm config: the Mac's hm config does not parse as a JSON object: %w", err)
	}
	raw, ok := doc[hmDatabaseField]
	if !ok {
		return nil, fmt.Errorf("hm config: %s is missing; the container's copy rewrites that URL's host", hmDatabaseField)
	}
	var databaseURL string
	if err := json.Unmarshal(raw, &databaseURL); err != nil {
		return nil, fmt.Errorf("hm config: %s is not a string, so there is no URL to rewrite", hmDatabaseField)
	}
	rewritten, err := containerHMURL(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("hm config: %s %s; the Mac's hm config is left as it is", hmDatabaseField, err)
	}
	names := make([]string, 0, len(doc))
	for name := range doc {
		if strings.ContainsFunc(name, hmControl) {
			return nil, errors.New("hm config: a field name holds a control character, which a JSON string cannot carry")
		}
		names = append(names, name)
	}
	// Written by hand, as RenderOMConfig is, so the render has no encoder error
	// to refuse: each field name is a checked string and each other value is the
	// bytes the Mac's config carried.
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("{\n")
	for i, name := range names {
		b.WriteString("  ")
		b.WriteString(strconv.Quote(name))
		b.WriteString(": ")
		if name == hmDatabaseField {
			b.WriteString(strconv.Quote(rewritten))
		} else {
			b.WriteString(string(doc[name]))
		}
		if i < len(names)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return []byte(b.String()), nil
}

// hmControl reports whether r is a character strconv.Quote writes as a Go
// escape that JSON does not accept, so a derived config carrying it would not
// parse.
func hmControl(r rune) bool { return r < 0x20 || r == 0x7f }

// containerHMURL returns the database address a process in the container
// reaches. hm hands the field to a Postgres driver, which accepts two forms: a
// URL (postgres://user:password@host:port/db?...) and a key=value string (host=...
// port=... user=... password=...). A loopback host in either form is the
// container itself, so each form is parsed and its host rewritten to
// hmContainerHost; a non-loopback host is left as it is, since a remote database
// needs no rewrite. A form this cannot parse, a unix-socket host, a numeric
// hostaddr that would still point at the container, or a key=value string with
// no host at all (which defaults to the local machine) is REFUSED with the form
// named, never copied: a copy would leave hm pointed at the container's own
// loopback. The errors never quote the value: it holds the database password.
func containerHMURL(databaseURL string) (string, error) {
	trimmed := strings.TrimSpace(databaseURL)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		out, err := containerHMURLForm(trimmed)
		if err != nil {
			return "", fmt.Errorf("(URL form) %w", err)
		}
		return out, nil
	}
	if strings.Contains(trimmed, "://") {
		return "", errors.New("(URL form) has a scheme other than postgres or postgresql")
	}
	out, err := containerHMKeyValue(trimmed)
	if err != nil {
		return "", fmt.Errorf("(key=value form) %w", err)
	}
	return out, nil
}

// hmLoopback reports whether host names the machine it is read on.
func hmLoopback(host string) bool {
	h := strings.Trim(host, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// hmHostElement turns one element of a host list into the container's: a
// loopback name becomes hmContainerHost, a unix-socket path (a leading slash) is
// refused since a socket on the Mac is not in the container, and anything else
// is kept.
func hmHostElement(h string) (string, error) {
	if strings.HasPrefix(h, "/") {
		return "", errors.New("names a unix-socket host, which is not reachable from the container")
	}
	if hmLoopback(h) {
		return hmContainerHost, nil
	}
	return h, nil
}

// containerHMURLForm rewrites the host of a postgres:// URL. The authority is
// parsed, and so are the host and hostaddr query parameters libpq lets override
// it.
func containerHMURLForm(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("does not parse as a URL")
	}
	if strings.Contains(u.Host, ",") {
		return "", errors.New("lists several hosts in the authority, which this does not rewrite; use one host")
	}
	if h := u.Hostname(); h != "" {
		// A parsed authority cannot name a unix socket (a path does not parse as a
		// host), so only the loopback test applies here; the host query parameter
		// below goes through hmHostElement, which refuses a socket path.
		nh := h
		if hmLoopback(h) {
			nh = hmContainerHost
		}
		if nh != h {
			if port := u.Port(); port != "" {
				u.Host = net.JoinHostPort(nh, port)
			} else {
				u.Host = nh
			}
		}
	}
	q := u.Query()
	changedQuery := false
	if v := q.Get("hostaddr"); v != "" {
		for _, el := range strings.Split(v, ",") {
			if hmLoopback(el) {
				return "", errors.New("has a loopback hostaddr, a numeric address that would still point at the container; remove it")
			}
		}
	}
	if v := q.Get("host"); v != "" {
		parts := strings.Split(v, ",")
		for i, el := range parts {
			nh, err := hmHostElement(el)
			if err != nil {
				return "", err
			}
			if nh != el {
				parts[i] = nh
				changedQuery = true
			}
		}
		if changedQuery {
			q.Set("host", strings.Join(parts, ","))
		}
	}
	if changedQuery {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// hmKV is one key=value pair of a libpq-style string: the key, the decoded
// value, and the byte span of the raw value in the string, so the rewrite
// replaces the value alone and keeps every other byte.
type hmKV struct {
	key        string
	value      string
	start, end int
	quoted     bool
}

// parseHMKeyValue reads a libpq key=value string: pairs separated by white
// space, optional white space around "=", a value bare (ending at white space)
// or single-quoted with \' and \\ escapes.
func parseHMKeyValue(s string) ([]hmKV, error) {
	var out []hmKV
	i := 0
	skipSpace := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
	}
	for {
		skipSpace()
		if i >= len(s) {
			return out, nil
		}
		ks := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			i++
		}
		key := s[ks:i]
		if key == "" {
			return nil, errors.New("has a pair with no key")
		}
		skipSpace()
		if i >= len(s) || s[i] != '=' {
			return nil, errors.New("has a key with no \"=\"")
		}
		i++
		skipSpace()
		kv := hmKV{key: key, start: i}
		if i < len(s) && s[i] == '\'' {
			kv.quoted = true
			i++
			var b strings.Builder
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					i++
					closed = true
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				return nil, errors.New("has an unterminated quoted value")
			}
			kv.value = b.String()
		} else {
			vs := i
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
				i++
			}
			kv.value = s[vs:i]
		}
		kv.end = i
		out = append(out, kv)
	}
}

// quoteHMValue writes v as a libpq value: bare when it holds nothing that needs
// quoting, else single-quoted with backslash escapes.
func quoteHMValue(v string, wasQuoted bool) string {
	if !wasQuoted && v != "" && !strings.ContainsAny(v, " \t\n\r'\\") {
		return v
	}
	r := strings.NewReplacer("\\", "\\\\", "'", "\\'")
	return "'" + r.Replace(v) + "'"
}

// containerHMKeyValue rewrites the host of a key=value string, keeping every
// other byte. Both host and hostaddr are read; a string that names no host is
// refused, since libpq then connects to the local machine's socket.
func containerHMKeyValue(s string) (string, error) {
	pairs, err := parseHMKeyValue(s)
	if err != nil {
		return "", err
	}
	sawHost := false
	var b strings.Builder
	last := 0
	for _, kv := range pairs {
		switch kv.key {
		case "hostaddr":
			for _, el := range strings.Split(kv.value, ",") {
				if hmLoopback(el) {
					return "", errors.New("has a loopback hostaddr, a numeric address that would still point at the container; remove it")
				}
			}
		case "host":
			sawHost = true
			parts := strings.Split(kv.value, ",")
			changed := false
			for i, el := range parts {
				nh, err := hmHostElement(el)
				if err != nil {
					return "", err
				}
				if nh != el {
					parts[i] = nh
					changed = true
				}
			}
			if changed {
				b.WriteString(s[last:kv.start])
				b.WriteString(quoteHMValue(strings.Join(parts, ","), kv.quoted))
				last = kv.end
			}
		}
	}
	if !sawHost {
		return "", errors.New("names no host, which defaults to the local machine; name one")
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

// hmConfigTemp is the temporary file WriteHMConfigFile writes through. *os.File
// is one; a test stands in a file that fails a step, to drive that step's
// failure.
type hmConfigTemp interface {
	Write(p []byte) (int, error)
	Chmod(mode fs.FileMode) error
	Close() error
	Name() string
}

// createHMConfigTemp makes the temporary file WriteHMConfigFile writes through.
func createHMConfigTemp(dir, pattern string) (hmConfigTemp, error) {
	return os.CreateTemp(dir, pattern)
}

// WriteHMConfigFile writes a derived hm config to path owner-only, through a
// temporary file beside it and one rename, so a reader never sees half a file
// and a replaced file loses an older, wider mode.
func WriteHMConfigFile(path string, content []byte) error {
	return writeHMConfigFile(path, content, createHMConfigTemp)
}

// writeHMConfigFile is WriteHMConfigFile with the temporary file's source
// injected, so each step's failure can be driven.
func writeHMConfigFile(path string, content []byte, create func(dir, pattern string) (hmConfigTemp, error)) (err error) {
	f, err := create(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(hmConfigFileMode); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
