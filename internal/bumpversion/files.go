package bumpversion

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Paths of the files the command edits, relative to the repository root.
const (
	VersionGoPath   = "internal/cmd/version.go"
	PackageJSONPath = "npm-package/package.json"
	FlakePath       = "flake.nix"
	ChangelogPath   = "CHANGELOG.md"
)

// FS is the file seam. Every read and write of a version file goes through it,
// so the tests run against an in-memory tree and never the working copy. Names
// are slash-separated paths relative to the repository root, and a missing file
// reads as an error that matches fs.ErrNotExist.
type FS interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte) error
}

// CurrentVersion returns the version on the Version = "x" line of version.go,
// or "" when there is no such line.
func CurrentVersion(versionGo string) string {
	return quotedValue(versionGo, "Version = ")
}

// SetVersionGo replaces every Version = "old" in version.go with the new version.
func SetVersionGo(text, oldVersion, newVersion string) string {
	return setQuoted(text, "Version = ", oldVersion, newVersion)
}

// SetPackageJSON replaces every "version": "old" in package.json with the new
// version.
func SetPackageJSON(text, oldVersion, newVersion string) string {
	return setQuoted(text, `"version": `, oldVersion, newVersion)
}

// SetFlakeVersion replaces every version = "old" in flake.nix with the new
// version. A flake with two matching lines changes both, as the script's
// global substitution does.
func SetFlakeVersion(text, oldVersion, newVersion string) string {
	return setQuoted(text, "version = ", oldVersion, newVersion)
}

// setQuoted replaces every prefix"old" with prefix"new", the literal form of
// the script's s|prefix"old"|prefix"new"|g.
func setQuoted(text, prefix, oldVersion, newVersion string) string {
	return strings.ReplaceAll(text, prefix+`"`+oldVersion+`"`, prefix+`"`+newVersion+`"`)
}

// EditFile reads name through fsys, applies edit to its text and writes the
// result back, as the script's update_file does with sed -i. A file that cannot
// be read is an error: sed on a missing file stops the script.
func EditFile(fsys FS, name string, edit func(string) string) error {
	data, err := fsys.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	return fsys.WriteFile(name, []byte(edit(string(data))))
}

// UpdateChangelog moves the [Unreleased] heading to a new [version] entry dated
// date: the heading is replaced in place by "## [Unreleased]", a blank line and
// "## [version] - date". Text with no [Unreleased] heading comes back unchanged
// with the script's warning, which is the second return value; it is "" when
// the heading was moved.
func UpdateChangelog(text, version, date string) (string, string) {
	const heading = "## [Unreleased]"
	if !strings.Contains(text, heading) {
		return text, "Warning: No [Unreleased] section in CHANGELOG.md\nYou may need to manually update CHANGELOG.md"
	}
	entry := heading + "\n\n## [" + version + "] - " + date
	return strings.ReplaceAll(text, heading, entry), ""
}

// UpdateChangelogFile applies UpdateChangelog to CHANGELOG.md through fsys. A
// missing file and a file with no [Unreleased] heading are warnings, returned
// as the string with a nil error: the file is left as it is. Any other read
// error is returned.
func UpdateChangelogFile(fsys FS, version, date string) (string, error) {
	data, err := fsys.ReadFile(ChangelogPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "Warning: CHANGELOG.md not found, skipping", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ChangelogPath, err)
	}
	updated, warning := UpdateChangelog(string(data), version, date)
	if warning != "" {
		return warning, nil
	}
	return "", fsys.WriteFile(ChangelogPath, []byte(updated))
}

// Versions is the version each file holds as Verify read it back. FlakeNix is ""
// when flake.nix was not read.
type Versions struct {
	VersionGo   string
	PackageJSON string
	FlakeNix    string
}

// MismatchError reports the version each file holds when one of them is not the
// wanted version. Its message is the per-file report the script prints.
type MismatchError struct {
	Want  string
	Found Versions
	Nix   bool
}

func (e *MismatchError) Error() string {
	lines := []string{
		"Version mismatch detected!",
		"  version.go: " + e.Found.VersionGo,
		"  package.json: " + e.Found.PackageJSON,
	}
	if e.Nix {
		lines = append(lines, "  flake.nix: "+e.Found.FlakeNix)
	}
	return strings.Join(lines, "\n")
}

// Verify reads the version back from version.go, package.json and, when nix is
// true, flake.nix, and checks that every one is want. flake.nix is not read when
// nix is false. A file that cannot be read is an error, never a match, and a want
// that is not a version is refused, so an empty want cannot match a file that
// holds no version.
func Verify(fsys FS, want string, nix bool) error {
	if err := ValidateVersion(want); err != nil {
		return err
	}
	goText, err := readText(fsys, VersionGoPath)
	if err != nil {
		return err
	}
	pkgText, err := readText(fsys, PackageJSONPath)
	if err != nil {
		return err
	}
	found := Versions{
		VersionGo:   CurrentVersion(goText),
		PackageJSON: quotedValue(pkgText, `"version"`),
	}
	if nix {
		flakeText, err := readText(fsys, FlakePath)
		if err != nil {
			return err
		}
		found.FlakeNix = quotedValue(flakeText, "version = ")
	}
	if found.VersionGo == want && found.PackageJSON == want && (!nix || found.FlakeNix == want) {
		return nil
	}
	return &MismatchError{Want: want, Found: found, Nix: nix}
}

func readText(fsys FS, name string) (string, error) {
	data, err := fsys.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return string(data), nil
}

// quotedValue returns the text between the last two quotes on the first line
// containing marker. That is the value the script's sed extracts with
// s/.*"\(.*\)".*/\1/. It returns "" when no line holds marker or the line has
// fewer than two quotes.
func quotedValue(text, marker string) string {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		last := strings.LastIndex(line, `"`)
		if last < 0 {
			return ""
		}
		prev := strings.LastIndex(line[:last], `"`)
		if prev < 0 {
			return ""
		}
		return line[prev+1 : last]
	}
	return ""
}
