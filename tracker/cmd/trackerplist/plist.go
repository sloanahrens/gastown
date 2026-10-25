package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/tracker/server"
)

// Config is the installation the two jobs are generated for.
type Config struct {
	Home        string // the town home; data and logs live under it
	PGBin       string // directory holding the postgres binary
	Port        int    // the postgres TCP port, loopback only
	Listen      string // the loopback address trackerd serves on
	LabelPrefix string // launchd label prefix
}

const (
	// databaseName is the one database the tracker keeps (design decision 9).
	databaseName = "tracker"
	// dbListenAddress is fixed: postgres is reachable on the loopback and the
	// data directory's unix socket, never on another interface.
	dbListenAddress = "127.0.0.1"
	// throttleInterval keeps launchd from restarting trackerd in a tight loop
	// while postgres is still coming up.
	throttleInterval = 10
)

const doctype = `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`

// field and dict keep the plist's key order, which the goldens pin.
type field struct {
	key string
	val any // string, int, bool, dict, or array
}

type dict []field

type array []string

// Validate reports the first configuration value that cannot name a job.
func (c Config) Validate() error {
	switch {
	case c.Home == "":
		return errors.New("-home is empty and HOME is not set")
	case c.PGBin == "":
		return errors.New("-pg-bin is empty")
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("-port must be 1-65535, got %d", c.Port)
	case c.LabelPrefix == "":
		return errors.New("-label-prefix is empty")
	}
	return nil
}

func (c Config) label(job string) string { return c.LabelPrefix + "." + job }

// daemonDir is where launchd writes both jobs' logs.
func (c Config) daemonDir() string { return filepath.Join(c.Home, "gt", "daemon") }

// dataDir holds the postgres cluster and its unix socket.
func (c Config) dataDir() string { return filepath.Join(c.Home, "gt", ".pg-data") }

// DatabasePlist returns the property list for the postgres job.
func DatabasePlist(c Config) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	// launchd starts a job with no locale, and postgres refuses to start
	// without one, so the job sets LC_ALL and LANG itself.
	return render(dict{
		{"Label", c.label("tracker-db")},
		{"ProgramArguments", array{
			filepath.Join(c.PGBin, "postgres"),
			"-D", c.dataDir(),
			"-c", "listen_addresses=" + dbListenAddress,
			"-c", "port=" + strconv.Itoa(c.Port),
			"-c", "unix_socket_directories=" + c.dataDir(),
		}},
		{"RunAtLoad", true},
		{"KeepAlive", true},
		{"EnvironmentVariables", dict{
			{"LC_ALL", "C"},
			{"LANG", "C"},
		}},
		{"StandardOutPath", filepath.Join(c.daemonDir(), "tracker-db.log")},
		{"StandardErrorPath", filepath.Join(c.daemonDir(), "tracker-db.err.log")},
	})
}

// TrackerdPlist returns the property list for the tracker service job.
func TrackerdPlist(c Config) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if err := server.RequireLoopback(c.Listen); err != nil {
		return "", err
	}
	// No password: the database is reachable on the loopback only, and the
	// service's own bearer token is what guards it (design section 11).
	dsn := "postgres://" + dbListenAddress + ":" + strconv.Itoa(c.Port) + "/" + databaseName + "?sslmode=disable"
	// The installer puts its binaries in ~/.local/bin (docs/INSTALLING.md).
	bin := filepath.Join(c.Home, ".local", "bin", "trackerd")
	return render(dict{
		{"Label", c.label("trackerd")},
		{"ProgramArguments", array{
			bin,
			"-listen", c.Listen,
		}},
		{"EnvironmentVariables", dict{
			{"TRACKER_DSN", dsn},
			{"LC_ALL", "C"},
			{"LANG", "C"},
		}},
		{"KeepAlive", true},
		{"ThrottleInterval", throttleInterval},
		{"StandardOutPath", filepath.Join(c.daemonDir(), "trackerd.log")},
		{"StandardErrorPath", filepath.Join(c.daemonDir(), "trackerd.err.log")},
	})
}

// render writes the plist document for job. encoding/xml escapes every value,
// so a path holding &, < or a quote stays well-formed.
func render(job dict) (string, error) {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(doctype + "\n")
	enc := xml.NewEncoder(&b)
	enc.Indent("", "\t")
	root := xml.StartElement{
		Name: xml.Name{Local: "plist"},
		Attr: []xml.Attr{{Name: xml.Name{Local: "version"}, Value: "1.0"}},
	}
	if err := enc.EncodeToken(root); err != nil {
		return "", err
	}
	if err := writeDict(enc, job); err != nil {
		return "", err
	}
	if err := enc.EncodeToken(root.End()); err != nil {
		return "", err
	}
	if err := enc.Flush(); err != nil {
		return "", err
	}
	// encoding/xml never self-closes an empty element; a property list spells
	// a boolean <true/>.
	out := strings.NewReplacer("<true></true>", "<true/>", "<false></false>", "<false/>").Replace(b.String())
	return out + "\n", nil
}

func writeDict(enc *xml.Encoder, d dict) error {
	start := xml.StartElement{Name: xml.Name{Local: "dict"}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	for _, f := range d {
		if err := textElement(enc, "key", f.key); err != nil {
			return err
		}
		if err := writeValue(enc, f.val); err != nil {
			return fmt.Errorf("%s: %w", f.key, err)
		}
	}
	return enc.EncodeToken(start.End())
}

func writeValue(enc *xml.Encoder, v any) error {
	switch x := v.(type) {
	case string:
		return textElement(enc, "string", x)
	case int:
		return textElement(enc, "integer", strconv.Itoa(x))
	case bool:
		name := "false"
		if x {
			name = "true"
		}
		start := xml.StartElement{Name: xml.Name{Local: name}}
		if err := enc.EncodeToken(start); err != nil {
			return err
		}
		return enc.EncodeToken(start.End())
	case dict:
		return writeDict(enc, x)
	case array:
		start := xml.StartElement{Name: xml.Name{Local: "array"}}
		if err := enc.EncodeToken(start); err != nil {
			return err
		}
		for _, s := range x {
			if err := textElement(enc, "string", s); err != nil {
				return err
			}
		}
		return enc.EncodeToken(start.End())
	default:
		return fmt.Errorf("unsupported plist value %T", v)
	}
}

func textElement(enc *xml.Encoder, name, text string) error {
	start := xml.StartElement{Name: xml.Name{Local: name}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if err := enc.EncodeToken(xml.CharData(text)); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}
