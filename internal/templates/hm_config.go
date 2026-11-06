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

// containerHMURL returns the database URL a process in the container reaches:
// the Mac's loopback host becomes hmContainerHost, and every other URL is
// returned unchanged, since a remote database needs no rewrite. The error never
// quotes the URL: it holds the database password.
func containerHMURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", errors.New("does not parse as a URL")
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1":
	default:
		return databaseURL, nil
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(hmContainerHost, port)
	} else {
		u.Host = hmContainerHost
	}
	return u.String(), nil
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
