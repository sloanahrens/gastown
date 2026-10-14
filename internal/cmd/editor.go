package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// defaultEditor is the editor used when $EDITOR is unset.
const defaultEditor = "vi"

// editorArgv builds the argv that opens path in the editor named by the
// editor value (normally $EDITOR). The value is split on whitespace so
// EDITOR="code --wait" runs code with --wait; an empty value falls back to
// defaultEditor. Shell quoting is not interpreted.
func editorArgv(editor, path string) []string {
	argv := strings.Fields(editor)
	if len(argv) == 0 {
		argv = []string{defaultEditor}
	}
	return append(argv, path)
}

// openInEditor runs $EDITOR (vi when unset) on path and waits for it to exit.
func openInEditor(path string) error {
	argv := editorArgv(os.Getenv("EDITOR"), path)
	editorCmd := exec.Command(argv[0], argv[1:]...)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("running editor: %w", err)
	}
	return nil
}
