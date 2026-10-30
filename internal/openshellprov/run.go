package openshellprov

import (
	"io"
	"path/filepath"
)

// NewRun builds the run the command makes from its host, its parsed options and
// the paths it resolved. Progress lines go to out and errors to errw.
func NewRun(h Host, opts Options, paths Paths, out, errw io.Writer) *Run {
	return &Run{
		Client: Client{Run: h.Exec, Gateway: opts.Gateway},
		Gt:     Gt{Run: h.Exec, Gateway: opts.Gateway, Root: paths.Root, Err: errw},
		Paths:  paths,
		Files:  h.Files,
		Env:    h.Env(),
		Out:    out,
		Err:    errw,
	}
}

// Execute runs the whole import in the script's order. The gateway endpoint is
// checked first, so a gateway gt refuses is left untouched; then the profiles are
// imported; then the deepseek and forgejo-polecat providers are created.
func (r *Run) Execute() error {
	if err := r.Gt.CheckEndpoint(); err != nil {
		return err
	}
	if err := r.ImportProfiles(); err != nil {
		return err
	}
	if err := r.CreateProvider("deepseek", "deepseek-claude", filepath.Join(r.Paths.ConfigDir, "deepseek.env"), "DEEPSEEK_API_KEY", "ANTHROPIC_AUTH_TOKEN"); err != nil {
		return err
	}
	return r.CreateProvider("forgejo-polecat", "forgejo-polecat", filepath.Join(r.Paths.ConfigDir, "forgejo-polecat.env"), "FORGEJO_TOKEN", "FORGEJO_TOKEN")
}
