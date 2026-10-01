package unittier

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRefusingToolsLinksTheToolsOnPathButNotTheAllowed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	onPath := func(name string) (string, error) {
		if name == "ss" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	}
	if err := writeRefusingTools(dir, map[string]bool{"ps": true}, onPath); err != nil {
		t.Fatal(err)
	}
	for _, name := range refusedTools {
		_, err := os.Lstat(filepath.Join(dir, name))
		if name == "ps" || name == "ss" {
			if err == nil {
				t.Errorf("%s got a refusing stand-in; want none for an allowed tool (ps) or one not on PATH (ss)", name)
			}
			continue
		}
		if err != nil {
			t.Errorf("no refusing %s: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "git")); err == nil {
		t.Errorf("git got a refusing stand-in; gitfree.txt and WithoutGit govern git")
	}
}

func TestReportRefusedListsEveryStartAndFailsTheRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := "/tmp/a\ttmux -L x kill-server\n/tmp/b\tps -p 1 -o args=\n"
	if err := os.WriteFile(filepath.Join(dir, refusedLog), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	if code := reportRefused(0, dir, &w); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	for _, want := range []string{"SUBPROCESS TRIPWIRE: the unit tier started an external tool 2 time(s)", "/tmp/a\ttmux -L x kill-server", "/tmp/b\tps -p 1 -o args="} {
		if !strings.Contains(w.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, w.String())
		}
	}
	if code := reportRefused(3, dir, &bytes.Buffer{}); code != 3 {
		t.Errorf("a failing code = %d, want it kept (3)", code)
	}
}

func TestReportRefusedPassesWithNoLog(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	if code := reportRefused(0, t.TempDir(), &w); code != 0 || w.Len() != 0 {
		t.Errorf("no log: code %d, output %q; want 0 and nothing", code, w.String())
	}
	if code := reportRefused(0, "", &w); code != 0 || w.Len() != 0 {
		t.Errorf("no tool dir: code %d, output %q; want 0 and nothing", code, w.String())
	}
}

func TestReportRefusedFailsOnAnUnreadableLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, refusedLog), 0o755); err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	if code := reportRefused(0, dir, &w); code != 1 || !strings.Contains(w.String(), "cannot read the refusing tools' log") {
		t.Errorf("unreadable log: code %d, output %q; want 1 and the reason", code, w.String())
	}
}
