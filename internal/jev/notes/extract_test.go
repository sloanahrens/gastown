package notes

import (
	"reflect"
	"testing"
)

func TestExtractClassifiesBacktickedSpans(t *testing.T) {
	t.Parallel()
	text := "# A note\n" +
		"Edit `internal/jev/stale/stale.go:120` and `docs/reference.md`, not `~/gt/.runtime/jev/stale.jsonl`.\n" +
		"Run `gt done --status=ESCALATED` or `bd show gt-abc`, never `bd sync`.\n" +
		"Set `patrols.jev_stale.enabled`; the file is `daemon_config.go`; the word `and/or` is prose.\n" +
		"A `plain` span and a `max_usd_per_day` key.\n"
	got := Extract(text)

	// "and/or" is shaped like a path; the resolver drops it, because "and" is
	// no top-level directory of the repo.
	if want := []string{"and/or", "docs/reference.md", "internal/jev/stale/stale.go"}; !reflect.DeepEqual(got.Paths, want) {
		t.Errorf("paths = %v, want %v", got.Paths, want)
	}
	if want := []string{"max_usd_per_day", "patrols.jev_stale.enabled"}; !reflect.DeepEqual(got.Keys, want) {
		t.Errorf("keys = %v, want %v", got.Keys, want)
	}
	want := []Command{
		{Bin: "gt", Words: []string{"done"}, Flags: []string{"--status"}},
		{Bin: "bd", Words: []string{"show", "gt-abc"}},
		{Bin: "bd", Words: []string{"sync"}},
	}
	if !reflect.DeepEqual(got.Commands, want) {
		t.Errorf("commands = %+v, want %+v", got.Commands, want)
	}
}

// A fenced block names commands one per line; its comments and other lines are
// not tokens, and a pipe ends what belongs to the command.
func TestExtractReadsCommandLinesInFencedBlocks(t *testing.T) {
	t.Parallel()
	text := "Steps:\n\n```bash\n# gt ignored --comment\n$ gt dolt status --verbose | grep --color ok\ngit status\nhm query --json \"text\"\n```\n"
	got := Extract(text)
	want := []Command{
		{Bin: "gt", Words: []string{"dolt", "status"}, Flags: []string{"--verbose"}},
		{Bin: "hm", Words: []string{"query"}, Flags: []string{"--json"}},
	}
	if !reflect.DeepEqual(got.Commands, want) {
		t.Errorf("commands = %+v, want %+v", got.Commands, want)
	}
	if len(got.Paths) != 0 || len(got.Keys) != 0 {
		t.Errorf("paths = %v, keys = %v, want none", got.Paths, got.Keys)
	}
}

func TestExtractWordsStopAtTheFirstArgument(t *testing.T) {
	t.Parallel()
	got := Extract("`gt bead create \"title\" --rig=beads` and `gt mail send gastown/crew/x -s Hi`")
	want := []Command{
		{Bin: "gt", Words: []string{"bead", "create"}, Flags: []string{"--rig"}},
		{Bin: "gt", Words: []string{"mail", "send"}},
	}
	if !reflect.DeepEqual(got.Commands, want) {
		t.Errorf("commands = %+v, want %+v", got.Commands, want)
	}
}

func TestExtractNothingToCheck(t *testing.T) {
	t.Parallel()
	if got := Extract("Plain prose with `code` and `gt` alone and `--flag` bare."); !got.Empty() {
		t.Errorf("tokens = %+v, want none", got)
	}
}
