package cmd

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
)

// townVMsPlistTemplate is the com.gastown.colima-vms job: a thin wrapper that
// runs gt town vms start at load and is not restarted when it exits, so a failed
// start is read in its log rather than retried in a loop (gt-6111kv).
const townVMsPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.gastown.colima-vms</string>
	<key>ProgramArguments</key>
	<array>
{{- range .Args}}
		<string>{{xmlText .}}</string>
{{- end}}
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{xmlText .PATH}}</string>
		<key>HOME</key>
		<string>{{xmlText .Home}}</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>{{xmlText .Log}}</string>
	<key>StandardErrorPath</key>
	<string>{{xmlText .Log}}</string>
</dict>
</plist>
`

var townVMsPlistTmpl = template.Must(template.New("town-vms-plist").
	Funcs(template.FuncMap{"xmlText": forwardXMLEscaper.Replace}).
	Parse(townVMsPlistTemplate))

// townVMsPlistView is the data the template reads.
type townVMsPlistView struct {
	Args []string
	PATH string
	Home string
	Log  string
}

// renderTownVMsPlist returns the launchd job that runs gt town vms start at load.
// gt is the binary's absolute path; home sets the job's PATH, HOME and the log
// both its output streams go to. It only renders the plist.
func renderTownVMsPlist(gt, home string) ([]byte, error) {
	var buf bytes.Buffer
	err := writeTownVMsPlist(&buf, gt, home)
	return buf.Bytes(), err
}

// writeTownVMsPlist writes the job to w. The template writes straight to w, so a
// failed write surfaces here.
func writeTownVMsPlist(w io.Writer, gt, home string) error {
	err := townVMsPlistTmpl.Execute(w, townVMsPlistView{
		Args: []string{gt, "town", "vms", "start"},
		PATH: filepath.Join(home, ".local", "bin") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		Home: home,
		Log:  filepath.Join(home, "gt", "daemon", "colima-vms.log"),
	})
	if err != nil {
		return fmt.Errorf("rendering the com.gastown.colima-vms plist: %w", err)
	}
	return nil
}

// newTownVMsPlistCmd builds `plist`: it prints the com.gastown.colima-vms job for the
// overseer's apply step to load. It only renders: it never runs launchctl and never
// touches ~/Library/LaunchAgents. Both flags are required; the refusals of a
// relative, empty or newline value are gt-i6d00y's (gt-097npg).
func newTownVMsPlistCmd() *cobra.Command {
	var gt, home, out string
	cmd := &cobra.Command{
		Use:   "plist --gt <path> --home <dir>",
		Short: "Print the launchd job that runs gt town vms start at load",
		Args:  cobra.NoArgs,
		// A refusal prints no usage: stdout carries the plist and nothing else.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkTownVMsPlistPath("--gt", gt); err != nil {
				return err
			}
			if err := checkTownVMsPlistPath("--home", home); err != nil {
				return err
			}
			if out == "" {
				return writeTownVMsPlist(cmd.OutOrStdout(), gt, home)
			}
			// With --out the same bytes go to the file, mode 0644, replacing any
			// file there, and nothing to stdout: config.ReplaceFile writes beside
			// and renames, so a failed write leaves no partial job (gt-9tan46).
			data, err := renderTownVMsPlist(gt, home)
			if err == nil {
				err = config.ReplaceFile(out, data, 0o644)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&gt, "gt", "", "the absolute path of the Mac gt binary the job runs")
	cmd.Flags().StringVar(&home, "home", "", "the home directory of the user the job runs as")
	cmd.Flags().StringVar(&out, "out", "", "write the job to this file (mode 0644) instead of stdout")
	_ = cmd.MarkFlagRequired("gt")
	_ = cmd.MarkFlagRequired("home")
	return cmd
}

// checkTownVMsPlistPath refuses a value that would render a broken launchd job: an
// empty or relative path (an empty one is not absolute, so the one check covers both), or one
// holding a line break (gt-i6d00y). It runs before anything is printed or written.
func checkTownVMsPlistPath(flag, value string) error {
	switch {
	case strings.ContainsAny(value, "\r\n"):
		return fmt.Errorf("%s holds a line break: %q", flag, value)
	case !filepath.IsAbs(value):
		return fmt.Errorf("%s must be an absolute path, got %q", flag, value)
	}
	return nil
}
