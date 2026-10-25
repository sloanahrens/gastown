package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// update rewrites the goldens instead of comparing against them.
var update = flag.Bool("update", false, "rewrite testdata goldens")

func fixture() Config {
	return Config{
		Home:        "/Users/fixture",
		PGBin:       "/opt/homebrew/opt/postgresql@17/bin",
		Port:        5433,
		Listen:      "127.0.0.1:7433",
		LabelPrefix: "com.gastown",
	}
}

var jobs = []struct {
	name string
	gen  func(Config) (string, error)
}{
	{"tracker-db", DatabasePlist},
	{"trackerd", TrackerdPlist},
}

func goldenPath(job string) string { return filepath.Join("testdata", job+".plist.golden") }

func readGolden(t *testing.T, job string) string {
	t.Helper()
	data, err := os.ReadFile(goldenPath(job))
	if err != nil {
		t.Fatalf("read golden (regenerate with go test -run TestGoldens -update): %v", err)
	}
	return string(data)
}

// TestGoldens pins each job's whole document. Anything that moves a byte here
// is a change to what the host installs.
func TestGoldens(t *testing.T) {
	t.Parallel()
	for _, job := range jobs {
		t.Run(job.name, func(t *testing.T) {
			t.Parallel()
			got, err := job.gen(fixture())
			if err != nil {
				t.Fatalf("%s: %v", job.name, err)
			}
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatalf("create testdata: %v", err)
				}
				if err := os.WriteFile(goldenPath(job.name), []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			if want := readGolden(t, job.name); got != want {
				t.Errorf("%s golden differs: %s", job.name, firstDiff(want, got))
			}
		})
	}
}

// firstDiff reports the first line that differs, with both sides.
func firstDiff(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return fmt.Sprintf("line %d\n want: %q\n  got: %q", i+1, w, g)
		}
	}
	return "no line differs"
}

// TestGoldensAreWellFormed proves the committed files are valid XML property
// lists; the tests never need plutil, which not every host has.
func TestGoldensAreWellFormed(t *testing.T) {
	t.Parallel()
	for _, job := range jobs {
		t.Run(job.name, func(t *testing.T) {
			t.Parallel()
			decodePlist(t, readGolden(t, job.name))
		})
	}
}

func TestDatabaseJob(t *testing.T) {
	t.Parallel()
	text, err := DatabasePlist(fixture())
	if err != nil {
		t.Fatalf("DatabasePlist: %v", err)
	}
	p := decodePlist(t, text)
	assertString(t, p.get(t, "Label"), "com.gastown.tracker-db")
	assertStrings(t, p.get(t, "ProgramArguments"), []string{
		"/opt/homebrew/opt/postgresql@17/bin/postgres",
		"-D", "/Users/fixture/gt/.pg-data",
		"-c", "listen_addresses=127.0.0.1",
		"-c", "port=5433",
		"-c", "unix_socket_directories=/Users/fixture/gt/.pg-data",
	})
	assertKind(t, p.get(t, "RunAtLoad"), "true")
	assertKind(t, p.get(t, "KeepAlive"), "true")
	env := p.get(t, "EnvironmentVariables")
	assertString(t, env.get(t, "LC_ALL"), "C")
	assertString(t, env.get(t, "LANG"), "C")
	assertString(t, p.get(t, "StandardOutPath"), "/Users/fixture/gt/daemon/tracker-db.log")
	assertString(t, p.get(t, "StandardErrorPath"), "/Users/fixture/gt/daemon/tracker-db.err.log")
}

func TestTrackerdJob(t *testing.T) {
	t.Parallel()
	text, err := TrackerdPlist(fixture())
	if err != nil {
		t.Fatalf("TrackerdPlist: %v", err)
	}
	p := decodePlist(t, text)
	assertString(t, p.get(t, "Label"), "com.gastown.trackerd")
	assertStrings(t, p.get(t, "ProgramArguments"), []string{
		"/Users/fixture/.local/bin/trackerd", "-listen", "127.0.0.1:7433",
	})
	assertKind(t, p.get(t, "KeepAlive"), "true")
	throttle := p.get(t, "ThrottleInterval")
	assertKind(t, throttle, "integer")
	assertText(t, throttle, "10")

	env := p.get(t, "EnvironmentVariables")
	assertString(t, env.get(t, "LC_ALL"), "C")
	assertString(t, env.get(t, "LANG"), "C")
	dsn := env.get(t, "TRACKER_DSN")
	assertString(t, dsn, "postgres://127.0.0.1:5433/tracker?sslmode=disable")
	if strings.Contains(dsn.text, "@") || strings.Contains(dsn.text, "password") {
		t.Errorf("TRACKER_DSN carries a credential: %q", dsn.text)
	}
	assertString(t, p.get(t, "StandardOutPath"), "/Users/fixture/gt/daemon/trackerd.log")
	assertString(t, p.get(t, "StandardErrorPath"), "/Users/fixture/gt/daemon/trackerd.err.log")
}

// TestTrackerdRefusesNonLoopbackListen keeps the job from exposing the service
// on a routable address.
func TestTrackerdRefusesNonLoopbackListen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		listen  string
		wantErr string // "" when the address is acceptable
	}{
		{"127.0.0.1:7433", ""},
		{"127.0.0.2:9000", ""},
		{"[::1]:7433", ""},
		{"0.0.0.0:7433", "loopback"},
		{"localhost:7433", "loopback"},
		{"192.168.1.5:7433", "loopback"},
		{"127.0.0.1", "listen address"},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			t.Parallel()
			cfg := fixture()
			cfg.Listen = tc.listen
			_, err := TrackerdPlist(cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("TrackerdPlist(-listen %s) = %v, want an error mentioning %q", tc.listen, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Errorf("TrackerdPlist(-listen %s): %v", tc.listen, err)
			}
		})
	}
}

// TestValuesAreXMLEscaped feeds a home and label holding every character that
// ends an XML text node; they must survive a parse unchanged.
func TestValuesAreXMLEscaped(t *testing.T) {
	t.Parallel()
	cfg := fixture()
	cfg.Home = `/Users/a&b<c>"d'`
	cfg.LabelPrefix = `com.&<"gt`
	for _, job := range jobs {
		t.Run(job.name, func(t *testing.T) {
			t.Parallel()
			text, err := job.gen(cfg)
			if err != nil {
				t.Fatalf("%s: %v", job.name, err)
			}
			for _, escaped := range []string{"&amp;", "&lt;", "&#34;"} {
				if !strings.Contains(text, escaped) {
					t.Errorf("%s output does not escape with %s:\n%s", job.name, escaped, text)
				}
			}
			if strings.Contains(text, cfg.Home+"/gt") {
				t.Errorf("%s output holds the raw home path unescaped", job.name)
			}
			p := decodePlist(t, text)
			assertString(t, p.get(t, "Label"), cfg.LabelPrefix+"."+job.name)
			assertString(t, p.get(t, "StandardOutPath"), cfg.Home+"/gt/daemon/"+job.name+".log")
		})
	}
}

// TestPortFlowsIntoBothJobs ties the two files together: trackerd must reach
// the postgres port the database job was told to listen on.
func TestPortFlowsIntoBothJobs(t *testing.T) {
	t.Parallel()
	cfg := fixture()
	cfg.Port = 6000
	dbText, err := DatabasePlist(cfg)
	if err != nil {
		t.Fatalf("DatabasePlist: %v", err)
	}
	dbArgs := decodePlist(t, dbText).get(t, "ProgramArguments")
	var args []string
	for _, item := range dbArgs.array {
		args = append(args, item.text)
	}
	if !slices.Contains(args, "port=6000") {
		t.Errorf("database ProgramArguments = %q, want port=6000", args)
	}
	serviceText, err := TrackerdPlist(cfg)
	if err != nil {
		t.Fatalf("TrackerdPlist: %v", err)
	}
	dsn := decodePlist(t, serviceText).get(t, "EnvironmentVariables").get(t, "TRACKER_DSN")
	assertString(t, dsn, "postgres://127.0.0.1:6000/tracker?sslmode=disable")
}

func TestGenerationIsDeterministic(t *testing.T) {
	t.Parallel()
	for _, job := range jobs {
		t.Run(job.name, func(t *testing.T) {
			t.Parallel()
			first, err := job.gen(fixture())
			if err != nil {
				t.Fatalf("%s: %v", job.name, err)
			}
			second, err := job.gen(fixture())
			if err != nil {
				t.Fatalf("%s: %v", job.name, err)
			}
			if first != second {
				t.Errorf("%s output differs between two runs", job.name)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"no home":        {func(c *Config) { c.Home = "" }, "HOME"},
		"no pg bin":      {func(c *Config) { c.PGBin = "" }, "-pg-bin"},
		"port zero":      {func(c *Config) { c.Port = 0 }, "-port"},
		"port too large": {func(c *Config) { c.Port = 65536 }, "-port"},
		"no label":       {func(c *Config) { c.LabelPrefix = "" }, "-label-prefix"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := fixture()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	good := fixture()
	if err := good.Validate(); err != nil {
		t.Errorf("Validate() on the fixture: %v", err)
	}
}

func TestRunPrintsTheNamedJob(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	if err := run([]string{"-home", "/Users/fixture", "tracker-db"}, env(nil), &out, &errb); err != nil {
		t.Fatalf("run: %v (stderr %s)", err, errb.String())
	}
	want, err := DatabasePlist(fixture())
	if err != nil {
		t.Fatalf("DatabasePlist: %v", err)
	}
	if out.String() != want {
		t.Errorf("run wrote %s", firstDiff(want, out.String()))
	}
}

func TestRunDefaultsHomeFromEnv(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	if err := run([]string{"trackerd"}, env(map[string]string{"HOME": "/Users/env"}), &out, &errb); err != nil {
		t.Fatalf("run: %v (stderr %s)", err, errb.String())
	}
	cfg := fixture()
	cfg.Home = "/Users/env"
	want, err := TrackerdPlist(cfg)
	if err != nil {
		t.Fatalf("TrackerdPlist: %v", err)
	}
	if out.String() != want {
		t.Errorf("run with HOME wrote %s", firstDiff(want, out.String()))
	}
}

func TestRunRefusesBadInvocations(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no job":             {nil, nil, "tracker-db or trackerd"},
		"two jobs":           {[]string{"tracker-db", "trackerd"}, nil, "one job"},
		"unknown job":        {[]string{"database"}, nil, "database"},
		"unknown flag":       {[]string{"-nope"}, nil, "nope"},
		"bad port":           {[]string{"-home", "/Users/fixture", "-port", "70000", "tracker-db"}, nil, "-port"},
		"non-loopback":       {[]string{"-home", "/Users/fixture", "-listen", "0.0.0.0:1", "trackerd"}, nil, "loopback"},
		"no home and no env": {[]string{"tracker-db"}, nil, "HOME"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out, errb bytes.Buffer
			err := run(tc.args, env(tc.env), &out, &errb)
			if err == nil || !strings.Contains(err.Error()+errb.String(), tc.want) {
				t.Errorf("run(%v) = %v (stderr %q), want an error mentioning %q", tc.args, err, errb.String(), tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("run(%v) wrote %q on failure", tc.args, out.String())
			}
		})
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func assertString(t *testing.T, n *node, want string) {
	t.Helper()
	if n.kind != "string" {
		t.Fatalf("value is <%s>, want <string>", n.kind)
	}
	assertText(t, n, want)
}

func assertText(t *testing.T, n *node, want string) {
	t.Helper()
	if n.text != want {
		t.Errorf("value = %q, want %q", n.text, want)
	}
}

func assertKind(t *testing.T, n *node, kind string) {
	t.Helper()
	if n.kind != kind {
		t.Errorf("element is <%s>, want <%s>", n.kind, kind)
	}
}

func assertStrings(t *testing.T, n *node, want []string) {
	t.Helper()
	if n.kind != "array" {
		t.Fatalf("value is <%s>, want <array>", n.kind)
	}
	var got []string
	for _, item := range n.array {
		got = append(got, item.text)
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("array = %q, want %q", got, want)
	}
}

// node is the part of a property list the tests read. Decoding it with
// encoding/xml is what proves the output is well-formed.
type node struct {
	kind  string
	text  string
	dict  map[string]*node
	array []*node
}

func (n *node) get(t *testing.T, key string) *node {
	t.Helper()
	if n.kind != "dict" {
		t.Fatalf("want a <dict>, got <%s>", n.kind)
	}
	v, ok := n.dict[key]
	if !ok {
		t.Fatalf("key %q is missing", key)
	}
	return v
}

func decodePlist(t *testing.T, data string) *node {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(data))
	root := readPlist(t, dec)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return root
		}
		if err != nil {
			t.Fatalf("parse after </plist>: %v", err)
		}
		if _, ok := tok.(xml.StartElement); ok {
			t.Fatal("content after </plist>")
		}
	}
}

func readPlist(t *testing.T, dec *xml.Decoder) *node {
	t.Helper()
	// The declaration and doctype arrive as proc-inst and directive tokens.
	for {
		se, ok := nextElement(t, dec)
		if !ok {
			t.Fatal("no <plist> element")
		}
		if se.Name.Local != "plist" {
			t.Fatalf("root element is <%s>, want <plist>", se.Name.Local)
		}
		if v := attr(se, "version"); v != "1.0" {
			t.Fatalf("plist version = %q, want 1.0", v)
		}
		top, ok := nextElement(t, dec)
		if !ok {
			t.Fatal("<plist> holds no value element")
		}
		n := readNode(t, dec, top)
		if _, ok := nextElement(t, dec); ok {
			t.Fatal("<plist> holds more than one value element")
		}
		return n
	}
}

// nextElement skips character data and comments, returning the next element
// and whether it opens (true) or closes (false) one.
func nextElement(t *testing.T, dec *xml.Decoder) (xml.StartElement, bool) {
	t.Helper()
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("parse plist: %v", err)
		}
		switch x := tok.(type) {
		case xml.StartElement:
			return x, true
		case xml.EndElement:
			return xml.StartElement{}, false
		}
	}
}

func readNode(t *testing.T, dec *xml.Decoder, start xml.StartElement) *node {
	t.Helper()
	n := &node{kind: start.Name.Local}
	switch start.Name.Local {
	case "dict":
		n.dict = map[string]*node{}
		for {
			keyElem, ok := nextElement(t, dec)
			if !ok {
				return n
			}
			if keyElem.Name.Local != "key" {
				t.Fatalf("dict entry starts with <%s>, want <key>", keyElem.Name.Local)
			}
			var key string
			if err := dec.DecodeElement(&key, &keyElem); err != nil {
				t.Fatalf("parse key: %v", err)
			}
			valueElem, ok := nextElement(t, dec)
			if !ok {
				t.Fatalf("key %q holds no value element", key)
			}
			if _, dup := n.dict[key]; dup {
				t.Fatalf("key %q appears twice", key)
			}
			n.dict[key] = readNode(t, dec, valueElem)
		}
	case "array":
		for {
			item, ok := nextElement(t, dec)
			if !ok {
				return n
			}
			n.array = append(n.array, readNode(t, dec, item))
		}
	default:
		var text string
		if err := dec.DecodeElement(&text, &start); err != nil {
			t.Fatalf("parse <%s>: %v", start.Name.Local, err)
		}
		n.text = text
	}
	return n
}

func attr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
