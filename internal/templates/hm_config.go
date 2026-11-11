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

// The embedder and reranker blocks hm dials, as hm reads them at the pinned
// commit 7f2647bd (internal/config/config.go): embed.url (Provider.URL, :19 and
// :91), rerank.url (:92) and rerank.rerank_fallback.url (:27). The embedder and
// the local reranker default to a loopback address on the Mac, so a block that
// names no URL for them is written with that default rewritten, never left for
// the container to dial. A hosted reranker has no loopback default to write.
const (
	hmEmbedField        = "embed"
	hmRerankField       = "rerank"
	hmRerankFallbackKey = "rerank_fallback"
	hmDefaultEmbedURL   = "http://localhost:11434" // config.go:128, the Ollama embedder
	hmDefaultRerankURL  = "http://localhost:8012"  // config.go:41, the llama-server reranker
	hmJevLlamacpp       = "jev+llamacpp"           // config.go:36, the composite reranker
	hmLocalRerankerName = "llamacpp"               // config.go:40, the local reranker's provider
)

// hmDefaultEmbedProvider is the embedder hm reads an absent embed block as: the
// Ollama one hmDefaultEmbedURL addresses (config.go:128).
const hmDefaultEmbedProvider = "ollama"

// hmConfigFileMode is the mode the derived hm config is written with: its
// database URL holds the database password, so its owner alone reads it.
const hmConfigFileMode fs.FileMode = 0o600

// RenderHMConfig derives the live container's hm config from the Mac's: the
// database URL's loopback host becomes hmContainerHost with its port, user and
// database kept, the embedder's and the reranker's loopback URLs become it too,
// and every other field's value is copied unchanged. Where the Mac's file names
// no URL for the embedder or the local reranker, hm would dial its loopback
// default, so the derived file names that default with the host rewritten. The
// result holds the database password, so the command writes it to a file and
// never prints it.
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
	if doc[hmEmbedField], err = rewriteHMEmbed(doc[hmEmbedField]); err != nil {
		return nil, err
	}
	if doc[hmRerankField], err = rewriteHMRerank(doc[hmRerankField]); err != nil {
		return nil, err
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

// hmBlock decodes one config block to its fields. An absent or null block is an
// empty one, which hm reads as its defaults.
func hmBlock(raw json.RawMessage, what string) (map[string]json.RawMessage, error) {
	block := map[string]json.RawMessage{}
	if raw == nil || string(raw) == "null" {
		return block, nil
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, fmt.Errorf("hm config: %s is not an object, so its URL cannot be rewritten", what)
	}
	return block, nil
}

// jsonString is s as a JSON string. It cannot fail: a quote, a backslash and a
// control character are escaped, and every other character is written as it is,
// so the render has no encoder error to refuse (as RenderHMConfig writes its
// fields by hand).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// hmBlockJSON writes a config block back as a JSON object: its fields sorted,
// each value the bytes the Mac's file carried. A field name holding a control
// character is refused, since a JSON string cannot carry it.
func hmBlockJSON(block map[string]json.RawMessage) (json.RawMessage, error) {
	names := make([]string, 0, len(block))
	for name := range block {
		if strings.ContainsFunc(name, hmControl) {
			return nil, errors.New("hm config: a field name in a service block holds a control character, which a JSON string cannot carry")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("{")
	for i, name := range names {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(jsonString(name))
		b.WriteString(":")
		b.Write(block[name])
	}
	b.WriteString("}")
	return json.RawMessage(b.String()), nil
}

// containerHMServiceURL returns the address a container reaches a service URL
// by: a loopback host becomes hmContainerHost with its port and path kept, and
// any other host is kept. An empty URL is kept. A URL that does not parse is
// refused; the error never quotes it.
func containerHMServiceURL(raw string) (string, error) {
	if raw == "" {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("does not parse as a URL with a host")
	}
	if !hmLoopback(u.Hostname()) {
		return raw, nil
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(hmContainerHost, port)
	} else {
		u.Host = hmContainerHost
	}
	return u.String(), nil
}

// rewriteHMURL rewrites block's url field to the container's address. A block
// that names no url takes def, rewritten, when def is set; with no def it is left
// without one, since hm then reads no URL from it.
func rewriteHMURL(block map[string]json.RawMessage, what, def string) error {
	current, named := def, false
	if raw, ok := block["url"]; ok && string(raw) != "null" {
		named = true
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("hm config: %s url is not a string, so there is no URL to rewrite", what)
		}
	}
	if !named && def == "" {
		return nil
	}
	rewritten, err := containerHMServiceURL(current)
	if err != nil {
		return fmt.Errorf("hm config: %s url %s; the Mac's hm config is left as it is", what, err)
	}
	block["url"] = json.RawMessage(jsonString(rewritten))
	return nil
}

// rewriteHMEmbed returns the embed block with its url in the container's address.
// hm reads an absent embed block as its default embedder, a provider and a URL
// together. An absent block is written with both, the URL rewritten, so the
// derived block reads as that default and not as a URL alone; a block that
// names no url gains the default one, rewritten.
func rewriteHMEmbed(raw json.RawMessage) (json.RawMessage, error) {
	block, err := hmBlock(raw, hmEmbedField)
	if err != nil {
		return nil, err
	}
	if raw == nil || string(raw) == "null" {
		block["provider"] = json.RawMessage(jsonString(hmDefaultEmbedProvider))
	}
	if err := rewriteHMURL(block, hmEmbedField, hmDefaultEmbedURL); err != nil {
		return nil, err
	}
	return hmBlockJSON(block)
}

// rewriteHMRerank returns the rerank block with its URLs in the container's
// address, reading the block the way hm does (decodeRerank, config.go:208-235):
// a block that names no provider, or jev+llamacpp, has the llamacpp reranker as
// its fallback, and a llamacpp block is the local reranker itself. An absent
// block is hm's default, jev+llamacpp. Only the local reranker's URL gains a
// default; a hosted provider's URL is its own and is never given one here.
func rewriteHMRerank(raw json.RawMessage) (json.RawMessage, error) {
	block, err := hmBlock(raw, hmRerankField)
	if err != nil {
		return nil, err
	}
	var name string
	if v, ok := block["provider"]; ok {
		if err := json.Unmarshal(v, &name); err != nil {
			return nil, errors.New("hm config: rerank.provider is not a string, so the reranker cannot be read")
		}
	}
	_, hasFallback := block[hmRerankFallbackKey]
	absent := raw == nil || string(raw) == "null"
	switch {
	case absent, name == hmJevLlamacpp, name == "" && hasFallback:
		fb, err := hmBlock(block[hmRerankFallbackKey], "rerank.rerank_fallback")
		if err != nil {
			return nil, err
		}
		fbName := ""
		if v, ok := fb["provider"]; ok {
			if err := json.Unmarshal(v, &fbName); err != nil {
				return nil, errors.New("hm config: rerank.rerank_fallback.provider is not a string, so the fallback cannot be read")
			}
		}
		fbDefault := ""
		if fbName == "" || fbName == hmLocalRerankerName {
			fbDefault = hmDefaultRerankURL
		}
		if err := rewriteHMURL(fb, "rerank.rerank_fallback", fbDefault); err != nil {
			return nil, err
		}
		enc, err := hmBlockJSON(fb)
		if err != nil {
			return nil, err
		}
		block[hmRerankFallbackKey] = enc
		// The jev primary names its own URL, which is hosted; only a loopback one
		// is rewritten, and none is added.
		if err := rewriteHMURL(block, hmRerankField, ""); err != nil {
			return nil, err
		}
	case name == "", name == hmLocalRerankerName:
		if err := rewriteHMURL(block, hmRerankField, hmDefaultRerankURL); err != nil {
			return nil, err
		}
	default:
		if err := rewriteHMURL(block, hmRerankField, ""); err != nil {
			return nil, err
		}
	}
	return hmBlockJSON(block)
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
