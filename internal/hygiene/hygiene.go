// Package hygiene scans a list of files for the operator's name and contact
// details, and for paths the repository should not track.
//
// A finding names the file, the rule, the line and a hash of the line. It
// never reports the text of a matching line, so a failing test log does not
// print what it matched.
package hygiene

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Rule names. An allow row names the rule it excuses, so these are part of the
// allow.json contract.
const (
	RuleBillingID    = "billing account id"
	RuleHomePath     = "home directory path"
	RulePrivateTemp  = "private temp path"
	RuleAgentSession = "agent session artifact"
	RuleEmail        = "email address"
	RuleBlockedWord  = "blocked word"
	RuleBinaryFile   = "binary file"
	RuleForbiddenDir = "forbidden directory"
)

// ForbiddenDirs are directory names no tracked file may sit under.
var ForbiddenDirs = []string{
	".claude",
	".runtime",
	".beads",
	".dolt",
	"deep-review",
	"pr-sheriff-evidence",
}

//go:embed blocked.sha256
var blockedData string

var (
	billingIDPattern = regexp.MustCompile(`\b[0-9A-F]{6}-[0-9A-F]{6}-[0-9A-F]{6}\b`)
	// The account name starts with a letter or digit so a path such as
	// /home/.config/ is not read as somebody's home.
	homePathPattern    = regexp.MustCompile(`/(Users|home)/([A-Za-z0-9][A-Za-z0-9._-]*)/`)
	privateTempPattern = regexp.MustCompile(`/private/(tmp|var)/`)
	// The pattern is assembled from pieces so this source line does not match
	// the rule it defines.
	agentSessionPattern = regexp.MustCompile(
		`\.claude/` + `projects/` +
			`|session[-_]id["']?\s*[:=]\s*["']?[0-9a-f]{8}-` + `[0-9a-f]{4}`)
	// The domain takes an underscore too, so a label such as Do_thing is read
	// whole and identifierShaped can see it.
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-_]+(?:\.[A-Za-z0-9\-_]+)*`)
)

// Rule is a per-line check; Match reports whether the line holds a finding.
type Rule struct {
	Name  string
	Match func(line string) bool
}

// Finding is one rule hit on one file. Line is 1-based, or 0 for a path-level
// finding; LineHash is empty for a path-level finding.
type Finding struct {
	Path     string
	Rule     string
	Line     int
	LineHash string
}

// String renders the finding's location; the text of a matching line is not
// included.
func (f Finding) String() string {
	if f.Line == 0 {
		return fmt.Sprintf("%s: %s", f.Path, f.Rule)
	}
	return fmt.Sprintf("%s:%d: %s (line %s)", f.Path, f.Line, f.Rule, f.LineHash)
}

// Allow excuses one rule on one exact file, or one line of it when Line holds
// the line hash.
type Allow struct {
	Path   string `json:"path"`
	Rule   string `json:"rule"`
	Line   string `json:"line,omitempty"`
	Reason string `json:"reason"`
}

// ExcusedForbiddenPath is a file under a forbidden directory whose path-level
// finding an allow row excuses. Its contents are not scanned, so this record
// says, beside the findings, that the file is still there and why it is
// allowed; a report that prints only findings would otherwise hide it.
type ExcusedForbiddenPath struct {
	Path   string
	Reason string
}

// nonPersonalHomeNames are account names that documentation, fixtures and CI
// paths use for a placeholder. A home path whose account name is exactly one
// of these is not a finding; the whole name must match, so names such as
// "user2" or "operatorX" still count as somebody's.
var nonPersonalHomeNames = map[string]struct{}{
	"user": {}, "username": {}, "me": {}, "you": {}, "u": {}, "x": {},
	"op": {}, "operator": {}, "someone": {}, "other": {}, "name": {},
	"NAME": {}, "example": {}, "test": {}, "tester": {}, "fixture": {},
	"dev": {}, "ci": {}, "ops": {}, "agent": {}, "builder": {}, "coder": {},
	"go": {}, "bin": {}, "bob": {}, "ann": {},
	// The town's service account and the macOS shared directory.
	"gt": {}, "Shared": {},
}

// DefaultRules returns the shape rules, in reporting order.
func DefaultRules() []Rule {
	return []Rule{
		{Name: RuleBillingID, Match: billingIDPattern.MatchString},
		{Name: RuleHomePath, Match: homePathFinding},
		{Name: RulePrivateTemp, Match: privateTempPattern.MatchString},
		{Name: RuleAgentSession, Match: agentSessionPattern.MatchString},
		{Name: RuleEmail, Match: emailFinding},
	}
}

// homePathFinding reports whether the line holds a home path whose account
// name is not a documented placeholder.
func homePathFinding(line string) bool {
	for start := 0; start < len(line); {
		loc := homePathPattern.FindStringSubmatchIndex(line[start:])
		if loc == nil {
			return false
		}
		name := line[start+loc[4] : start+loc[5]]
		root := line[start+loc[2] : start+loc[3]]
		if _, ok := nonPersonalHomeNames[name]; !ok && !homeSegmentMidPath(line, start+loc[0], root) {
			return true
		}
		// Keep the trailing separator so a second path that shares it is
		// still examined.
		start += loc[1] - 1
	}
	return false
}

// homeSegmentMidPath reports whether the home root at index at follows a name
// inside a longer path, as a home directory does inside a host or fixture
// directory. A path whose first segment is a home root is not exempt, so a
// second home segment under one still reports. The exemption is for the home
// root only: a Users segment in the middle stays a finding.
func homeSegmentMidPath(line string, at int, root string) bool {
	if root != "home" || at == 0 || !isNameByte(line[at-1]) {
		return false
	}
	begin := at
	for begin > 0 && (isNameByte(line[begin-1]) || line[begin-1] == '/') {
		begin--
	}
	first, _, _ := strings.Cut(strings.TrimLeft(line[begin:], "/"), "/")
	return first != "home" && first != "Users"
}

// isNameByte reports whether b can sit in a file or ref name: a letter, a digit,
// or one of . _ - ~ + %. A slash separates segments and is not a name byte.
func isNameByte(b byte) bool {
	return isASCIILetter(b) || b >= '0' && b <= '9' || strings.IndexByte("._-~+%", b) >= 0
}

// HashWord returns the hex SHA-256 of word lower-cased.
func HashWord(word string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(word)))
	return hex.EncodeToString(sum[:])
}

// HashLine returns the first 16 hex characters of the SHA-256 of the trimmed
// line.
func HashLine(line string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(line)))
	return hex.EncodeToString(sum[:])[:16]
}

// LoadBlocked reads a SHA-256 list into a set of hex hashes.
func LoadBlocked(r io.Reader) (map[string]struct{}, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("hygiene: read blocked list: %w", err)
	}
	blocked := make(map[string]struct{})
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) != sha256.Size*2 || !isHex(line) {
			return nil, fmt.Errorf("hygiene: blocked list line %d is not a %d-character hex hash", i+1, sha256.Size*2)
		}
		blocked[strings.ToLower(line)] = struct{}{}
	}
	return blocked, nil
}

// BlockedWords returns the blocklist embedded beside this package.
func BlockedWords() (map[string]struct{}, error) {
	return LoadBlocked(strings.NewReader(blockedData))
}

// LoadAllow decodes allow rows from JSON.
func LoadAllow(r io.Reader) ([]Allow, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var rows []Allow
	if err := dec.Decode(&rows); err != nil {
		return nil, fmt.Errorf("hygiene: decode allow rows: %w", err)
	}
	return rows, nil
}

// SaveAllow writes the rows as indented JSON, sorted by path, rule and line.
func SaveAllow(w io.Writer, rows []Allow) error {
	sorted := append([]Allow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Line < b.Line
	})
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sorted); err != nil {
		return fmt.Errorf("hygiene: encode allow rows: %w", err)
	}
	return nil
}

// Scan reports every finding in paths, applying allow rows as excuses. Each
// path is read from fsys; a read failure is an error, never a clean result.
// An empty rule set or blocklist is an error too, since a scan with nothing to
// apply would report every file clean.
// An allow row that excuses a forbidden-directory file's path finding is
// returned in excused: the file's contents are not scanned, so that record is
// how a report still names the file and the reason it is allowed.
func Scan(fsys fs.FS, paths []string, rules []Rule, blocked map[string]struct{}, allow []Allow) ([]Finding, []ExcusedForbiddenPath, error) {
	if err := validateScanInputs(rules, blocked); err != nil {
		return nil, nil, err
	}
	var findings []Finding
	var excused []ExcusedForbiddenPath
	for _, path := range paths {
		pathFindings, excusedPath, _, err := scanPath(fsys, path, rules, blocked, allow)
		if err != nil {
			return nil, nil, err
		}
		findings = append(findings, pathFindings...)
		if excusedPath != nil {
			excused = append(excused, *excusedPath)
		}
	}
	return findings, excused, nil
}

// validateScanInputs refuses a rule set or blocklist that would report every
// path clean: a scan with nothing to apply is an error, never a result. It
// runs once per scan, before any path is read.
func validateScanInputs(rules []Rule, blocked map[string]struct{}) error {
	if len(rules) == 0 {
		return fmt.Errorf("hygiene: scan has no rules")
	}
	for _, rule := range rules {
		if rule.Name == "" || rule.Match == nil {
			return fmt.Errorf("hygiene: rule with an empty name or no matcher")
		}
	}
	if len(blocked) == 0 {
		return fmt.Errorf("hygiene: scan has no blocklist")
	}
	return nil
}

// scanPath reports the findings in one file and returns the bytes it read, plus
// the forbidden-path record when an allow row excuses that path's finding.
func scanPath(fsys fs.FS, path string, rules []Rule, blocked map[string]struct{}, allow []Allow) ([]Finding, *ExcusedForbiddenPath, []byte, error) {
	if !fs.ValidPath(path) {
		return nil, nil, nil, fmt.Errorf("hygiene: invalid path %q", path)
	}
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("hygiene: read %s: %w", path, err)
	}

	var findings []Finding
	var excusedPath *ExcusedForbiddenPath
	forbidden := isForbiddenPath(path)
	if forbidden {
		if reason, ok := excuseReason(allow, path, RuleForbiddenDir, ""); ok {
			excusedPath = &ExcusedForbiddenPath{Path: path, Reason: reason}
		} else {
			findings = append(findings, Finding{Path: path, Rule: RuleForbiddenDir})
		}
	}
	if hasBlockedWord(path, blocked) && !excused(allow, path, RuleBlockedWord, "") {
		findings = append(findings, Finding{Path: path, Rule: RuleBlockedWord})
	}
	// A file under a forbidden directory is reported by its path alone, as a
	// finding or as an excused path. Its contents are not scanned: a backup
	// export or a session log repeats the same names and paths thousands of
	// times, and the path already says the file must not be tracked.
	if forbidden {
		return findings, excusedPath, data, nil
	}
	if bytes.IndexByte(data, 0) >= 0 {
		if !excused(allow, path, RuleBinaryFile, "") {
			findings = append(findings, Finding{Path: path, Rule: RuleBinaryFile})
		}
		return findings, nil, data, nil
	}
	for i, line := range strings.Split(string(data), "\n") {
		hash := HashLine(line)
		for _, rule := range rules {
			if !rule.Match(line) || excused(allow, path, rule.Name, hash) {
				continue
			}
			findings = append(findings, Finding{Path: path, Rule: rule.Name, Line: i + 1, LineHash: hash})
		}
		if hasBlockedWord(line, blocked) && !excused(allow, path, RuleBlockedWord, hash) {
			findings = append(findings, Finding{Path: path, Rule: RuleBlockedWord, Line: i + 1, LineHash: hash})
		}
	}
	return findings, nil, data, nil
}

// ValidateAllow refuses a row that names no path or reason, a row for an
// untracked path, and a row whose rule no longer fires where it says it does.
func ValidateAllow(fsys fs.FS, paths []string, rules []Rule, blocked map[string]struct{}, allow []Allow) error {
	if err := validateScanInputs(rules, blocked); err != nil {
		return err
	}
	tracked := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		tracked[path] = struct{}{}
	}
	for _, row := range allow {
		if row.Path == "" {
			return fmt.Errorf("hygiene: allow row names no path")
		}
		if row.Rule == "" {
			return fmt.Errorf("hygiene: allow row for %s names no rule", row.Path)
		}
		if strings.TrimSpace(row.Reason) == "" {
			return fmt.Errorf("hygiene: allow row for %s names no reason", row.Path)
		}
		if _, ok := tracked[row.Path]; !ok {
			return fmt.Errorf("hygiene: allow row for %s: path is not tracked", row.Path)
		}
		findings, _, data, err := scanPath(fsys, row.Path, rules, blocked, nil)
		if err != nil {
			return err
		}
		if row.Line == "" {
			if !hasPathFinding(findings, row.Rule) {
				return fmt.Errorf("hygiene: allow row for %s rule %q: the rule does not fire on the path", row.Path, row.Rule)
			}
			continue
		}
		if hasLineFinding(findings, row.Rule, row.Line) {
			continue
		}
		if !lineHashPresent(data, row.Line) {
			return fmt.Errorf("hygiene: allow row for %s rule %q line %q: the line is not in the file", row.Path, row.Rule, row.Line)
		}
		return fmt.Errorf("hygiene: allow row for %s rule %q line %q: the rule does not fire on that line", row.Path, row.Rule, row.Line)
	}
	return nil
}

func excused(allow []Allow, path, rule, lineHash string) bool {
	_, ok := excuseReason(allow, path, rule, lineHash)
	return ok
}

// excuseReason returns the reason of the allow row that excuses one finding.
func excuseReason(allow []Allow, path, rule, lineHash string) (string, bool) {
	for _, row := range allow {
		if row.Path == "" || row.Path != path || row.Rule != rule {
			continue
		}
		if row.Line == lineHash {
			return row.Reason, true
		}
	}
	return "", false
}

func hasPathFinding(findings []Finding, rule string) bool {
	for _, f := range findings {
		if f.Line == 0 && f.Rule == rule {
			return true
		}
	}
	return false
}

func hasLineFinding(findings []Finding, rule, lineHash string) bool {
	for _, f := range findings {
		if f.Line != 0 && f.Rule == rule && f.LineHash == lineHash {
			return true
		}
	}
	return false
}

func lineHashPresent(data []byte, lineHash string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if HashLine(line) == lineHash {
			return true
		}
	}
	return false
}

func isForbiddenPath(path string) bool {
	for _, part := range strings.Split(path, "/") {
		for _, dir := range ForbiddenDirs {
			if part == dir {
				return true
			}
		}
	}
	return false
}

func hasBlockedWord(s string, blocked map[string]struct{}) bool {
	for _, word := range words(s) {
		if _, ok := blocked[HashWord(word)]; ok {
			return true
		}
	}
	return false
}

// words splits s into maximal runs of letters and digits; every other
// character separates words, including underscore.
func words(s string) []string {
	var out []string
	var run strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			run.WriteRune(r)
			continue
		}
		if run.Len() > 0 {
			out = append(out, run.String())
			run.Reset()
		}
	}
	if run.Len() > 0 {
		out = append(out, run.String())
	}
	return out
}

func isHex(s string) bool {
	for _, r := range s {
		if !isHexDigit(r) {
			return false
		}
	}
	return true
}

func isHexDigit(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

func isASCIILetter(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// knownAddresses are the two addresses that belong to nobody: the well-known
// attribution address and GitHub's ssh user. A match is exact, so a lookalike
// local part or domain is still an address.
var knownAddresses = map[string]bool{
	"noreply@anthropic.com": true,
	"git@github.com":        true,
}

// emailFinding reports whether the line holds an address the exemptions do not
// cover.
func emailFinding(line string) bool {
	for _, loc := range emailPattern.FindAllStringIndex(line, -1) {
		at := strings.LastIndex(line[loc[0]:loc[1]], "@") + loc[0]
		local := line[loc[0]:at]
		domain := line[at+1 : loc[1]]
		rest := line[loc[1]:]
		if knownAddresses[local+"@"+domain] {
			continue
		}
		// A colon and then whitespace after the address is how a person
		// writes a host in a document. It is a finding whatever the host
		// looks like: a dotless host there is a host, not a version pin.
		// The one exception is a dotless host whose local part follows a
		// slash: that is a segment of a branch or ref name, not prose.
		prose := colonThenProse(rest) && !(dotless(domain) && loc[0] > 0 && line[loc[0]-1] == '/')
		if !addressDomain(domain) && !prose {
			continue
		}
		if emailIsFinding(local, domain, rest) {
			return true
		}
	}
	return false
}

// colonThenProse reports whether rest starts with a colon followed by
// whitespace, the scp-like prose shape.
func colonThenProse(rest string) bool {
	return strings.HasPrefix(rest, ":") && len(rest) > 1 && unicode.IsSpace(rune(rest[1]))
}

// addressDomain reports whether domain has the shape of an address: at least
// one dot, and a last label that starts with a letter, is two or more letters
// long and is not an identifier (see identifierShaped). Without a dot the match
// is a version pin or an id (checkout@v4, image@sha256, main@a); a one-letter or
// all-digit last label (a@b.c, 127.0.0.1) is not a host name.
func addressDomain(domain string) bool {
	dot := strings.LastIndex(domain, ".")
	if dot < 0 {
		return false
	}
	last := domain[dot+1:]
	if len(last) < 2 || !isASCIILetter(last[0]) {
		return false
	}
	return !identifierShaped(last)
}

// identifierShaped reports whether a last label is a Go identifier rather than a
// top-level domain. A label that holds a digit or an underscore (Retry2, Do_thing)
// is one. So is a camel-case label, where a lower-case letter is followed later
// by an upper-case one (queueLandingColumn). Every other label is a domain label,
// whatever its case: Com, COM, com and Shop all are. The shape alone decides, so
// a capitalised identifier such as Func is read as a domain too.
func identifierShaped(s string) bool {
	var lower bool
	for _, r := range s {
		switch {
		case r == '_' || r >= '0' && r <= '9':
			return true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r) && lower:
			return true
		}
	}
	return false
}

// colonThenPath reports whether rest starts with a colon and then a character
// that is not whitespace: an ssh path or a port.
func colonThenPath(rest string) bool {
	return strings.HasPrefix(rest, ":") && len(rest) > 1 && !unicode.IsSpace(rune(rest[1]))
}

// dotless reports whether domain has no dot: a version pin or a ref, not a host
// name with a top-level domain.
func dotless(domain string) bool {
	return !strings.Contains(domain, ".")
}

func emailIsFinding(local, domain, rest string) bool {
	if strings.HasPrefix(rest, ":") {
		// A path or port right after the colon is read by the host before it:
		// the ssh remote shape for git, and a reserved host for anyone. A space
		// after the colon marks prose, which is a finding.
		if colonThenPath(rest) && (local == "git" || exemptDomain(domain)) {
			return false
		}
		return true
	}
	return !exemptDomain(domain)
}

func exemptDomain(domain string) bool {
	d := strings.ToLower(domain)
	for _, reserved := range []string{"example.com", "example.org", "example.net"} {
		if d == reserved || strings.HasSuffix(d, "."+reserved) {
			return true
		}
	}
	labels := strings.Split(d, ".")
	switch labels[len(labels)-1] {
	case "invalid", "example", "test", "localhost":
		return true
	}
	return d == "gastown.local" || strings.HasSuffix(d, ".gastown.local")
}
