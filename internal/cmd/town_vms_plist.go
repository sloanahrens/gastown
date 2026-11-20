package cmd

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"text/template"
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
