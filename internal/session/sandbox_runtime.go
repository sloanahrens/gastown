package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
)

// runtimeArchiveName is the staged archive holding the polecat's Claude
// settings and system prompt, which the agent command names by path.
const runtimeArchiveName = "polecat-runtime.tgz"

// runtimeSegmentRe matches a rig or polecat name that is safe as one path
// component: never ".", "..", hidden, or carrying a separator.
var runtimeSegmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// requireRuntimeSegment refuses a rig or polecat name that is not a single
// path component, since both are joined into the host settings directory and
// into the archive's tar entry names.
func requireRuntimeSegment(what, name string) error {
	if !runtimeSegmentRe.MatchString(name) {
		return fmt.Errorf("polecat runtime: %s %q is not a single path segment", what, name)
	}
	return nil
}

// buildRuntimeArchive returns a gzipped tar that extracts under /sandbox to
// gt/<rig>/polecats/.claude/{settings.json,system-prompt-<polecat>.md}: the
// files `claude --settings … --append-system-prompt-file …` reads. The
// settings are rendered for the sandbox; the system prompt has the host town
// root rewritten. A missing settings.json is an error, since the agent command
// names it; a missing system prompt is skipped. A rig or polecat name that is
// not one path segment is an error too.
func buildRuntimeArchive(spec SpawnSpec) ([]byte, error) {
	if err := requireRuntimeSegment("rig", spec.Rig); err != nil {
		return nil, err
	}
	if err := requireRuntimeSegment("polecat", spec.Polecat); err != nil {
		return nil, err
	}
	dir := filepath.Join(spec.TownRoot, spec.Rig, "polecats", ".claude")
	rawSettings, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("polecat settings: %w", err)
	}
	settings, err := renderSandboxSettings(rawSettings)
	if err != nil {
		return nil, err
	}
	files := []struct {
		name string
		data []byte
	}{{"settings.json", settings}}

	promptName := "system-prompt-" + spec.Polecat + ".md"
	prompt, err := os.ReadFile(filepath.Join(dir, promptName))
	switch {
	case err == nil:
		files = append(files, struct {
			name string
			data []byte
		}{promptName, []byte(RewriteTownRoot(string(prompt), spec.TownRoot, sandboxTownRoot))})
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("polecat system prompt: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		name := path.Join("gt", spec.Rig, "polecats", ".claude", f.name)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
