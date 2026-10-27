package polecatimage

import (
	"strings"
	"testing"
)

func TestInstalledCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stdout string
		stderr string
		exit   int
		want   string
	}{
		{"build and branch", "gt version 1.2.3 (dev: master@abc1234)\n", "", 0, "abc1234"},
		{"bare commit", "gt version 0.1 (dev: abc1234def)\n", "", 0, "abc1234def"},
		{"colon in the branch", "gt version 0.1 (dev: feature/x@deadbee)\n", "", 0, "deadbee"},
		{"spaces in the field", "gt version 0.1 (dev: master@ abc1234)\n", "", 0, "abc1234"},
		{"digits only", "gt version 0.1 (dev: master@1234567)\n", "", 0, "1234567"},
		{"last group counts", "gt version (old) (dev: master@fedcba9)\n", "", 0, "fedcba9"},
		{"no closing paren", "gt version 0.1 (dev: master@abc1234", "", 0, "abc1234"},
		{"no group", "gt version 1.2.3\n", "", 0, ""},
		{"no colon", "gt 0.1 (dev) abc1234\n", "", 0, ""},
		{"uppercase", "gt version 0.1 (dev: master@ABC1234)\n", "", 0, ""},
		{"too short", "gt version 0.1 (dev: master@abc123)\n", "", 0, ""},
		{"not hex", "gt version 0.1 (dev: master@abc-1234)\n", "", 0, ""},
		{"empty output", "", "", 0, ""},
		{"a failed read is empty", "gt: not found\n", "", 127, ""},
		{"status does not stop the read", "gt version 0.1 (dev: master@abc1234)\n", "", 2, "abc1234"},
		{"stderr does not count", "gt version 0.1 (dev: master@abc1234)\n", "warning: (dev: 9999999)\n", 0, "abc1234"},
		{"first line only", "gt version 0.1 (dev: master@abc1234)\nsecond (dev: master@fedcba9)\n", "", 0, "abc1234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.reply = func(command) result {
				return result{stdout: tc.stdout, stderr: tc.stderr, exit: tc.exit}
			}
			if got := tr.InstalledCommit(); got != tc.want {
				t.Errorf("InstalledCommit() = %q, want %q", got, tc.want)
			}
			tr.wantArgs(t, "gt", "version")
		})
	}
}

func TestImageCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		res  result
		want string
	}{
		{"label", result{stdout: "abc1234\n"}, "abc1234"},
		{"blank lines after the label", result{stdout: "abc1234\n\n"}, "abc1234"},
		{"no image", result{exit: 1}, ""},
		{"stderr alone does not count", result{stderr: "warning: no such image\n", exit: 1}, ""},
		{"trailing spacing is trimmed", result{stdout: "abc1234 \n"}, "abc1234"},
		{"leading spacing is trimmed", result{stdout: " abc1234\n"}, "abc1234"},
		{"the label docker does not carry", result{stdout: "<no value>\n"}, ""},
		{"no value in the middle of other text is a value", result{stdout: "x <no value> y\n"}, "x <no value> y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.reply = func(command) result { return tc.res }
			if got := tr.ImageCommit(); got != tc.want {
				t.Errorf("ImageCommit() = %q, want %q", got, tc.want)
			}
			tr.wantArgs(t, "docker", "image", "inspect", "--format",
				`{{index .Config.Labels "org.gastown.gt-commit"}}`, "gastown-polecat:dev")
		})
	}
}

func TestSameCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"equal", "abc1234", "abc1234", true},
		{"a is the prefix", "abc1234def", "abc1234", true},
		{"b is the prefix", "abc1234", "abc1234def", true},
		{"seven characters each", "1234567", "1234567", true},
		{"a too short", "abc123", "abc1234", false},
		{"b too short", "abc1234", "abc123", false},
		{"different", "abc1234", "abd1234", false},
		{"empty", "", "", false},
		{"no common prefix", "9999999", "abc1234", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SameCommit(tc.a, tc.b); got != tc.want {
				t.Errorf("SameCommit(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestInstalledCommitDoesNotReadStderr(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = func(command) result {
		return result{stdout: "gt version 0.1\n", stderr: "gt version 0.1 (dev: master@abc1234)\n"}
	}
	if got := tr.InstalledCommit(); got != "" {
		t.Errorf("InstalledCommit() = %q, want empty: only stdout is the version line", got)
	}
}

func TestImageCommitFormatQuotesTheLabel(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Label = "org.example.other"
	tr.reply = func(command) result { return result{} }
	tr.ImageCommit()
	args, _ := tr.argsOf("docker")
	format := args[3]
	if !strings.Contains(format, `"org.example.other"`) {
		t.Errorf("format %q does not quote the configured label", format)
	}
}
