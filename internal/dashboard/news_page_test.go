package dashboard

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// itemURL is the discussion page a story's id names, which is where the page's
// comments link goes.
func itemURL(id int64) string { return newsItemURL + strconv.FormatInt(id, 10) }

// newsStoryMap is one story of a News reading, in the shape the server sends
// it: the page reads the fields by name, so a renamed field stops the pane
// drawing rather than being ignored.
func newsStoryMap(rank int, id int64, title, url, domain string, points, comments int, at time.Time) map[string]any {
	return map[string]any{
		"rank": rank, "id": id, "title": title, "url": url, "domain": domain,
		"points": points, "comments": comments, "at": at.Format(time.RFC3339),
	}
}

// newsReading is the News pane's half of a state frame.
func newsReading(stories []any, at time.Time, err string) map[string]any {
	news := map[string]any{"stories": stories, "at": at.Format(time.RFC3339)}
	if err != "" {
		news["error"] = err
	}
	return map[string]any{"news": news}
}

// The pane belongs in the right column, above the Machine panel, and it is left
// out until the server sends it something: the section starts hidden, so a
// dashboard whose news is switched off (settings/config.json
// dashboard.news.enabled) draws no empty box where the pane would be
// (gt-9ktrr).
func TestNewsPaneSitsAboveMachineInTheRightColumn(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	news := strings.Index(page, `id="newssec"`)
	machine := strings.Index(page, `id="machine"`)
	if news < 0 {
		t.Fatal("index.html has no News pane (#newssec)")
	}
	if machine < 0 {
		t.Fatal("index.html has no Machine pane (#machine)")
	}
	if news > machine {
		t.Error("the News pane does not sit above the Machine panel")
	}
	col := strings.Index(page, `<div class="col">`)
	if col < 0 || col > news {
		t.Error("the News pane is not in the page's first right-hand column")
	}
	if !strings.Contains(page[news:news+80], "hidden") {
		t.Error("the News section does not start hidden, so a server that sends no news draws an empty panel")
	}
	if !strings.Contains(strings.ReplaceAll(page, "\n", ""), `renderTrend(state); renderNews(state); renderMachine(state);`) {
		t.Error("the page's full redraw does not draw the News pane with the other panels")
	}
}

// The pane is five single-line links, each opening where it points in a new
// tab. The title goes to the story and the small link beside it to the
// discussion, which is where a reader who wants the comments wants to land.
func TestNewsPaneDrawsTheLinesWithLinksThatLeaveThePage(t *testing.T) {
	t.Parallel()

	at := time.Now()
	stories := []any{
		newsStoryMap(1, 11, "Postgres 18", "https://www.postgresql.org/about/news/pg18/", "postgresql.org", 412, 96, at.Add(-3*time.Hour)),
		newsStoryMap(2, 12, "The case for boring software", "https://example.com/boring", "example.com", 130, 44, at.Add(-2*time.Hour)),
		newsStoryMap(3, 13, "Ask HN: what broke in your stack this week?", itemURL(13), "", 88, 121, at.Add(-90*time.Minute)),
		newsStoryMap(4, 14, "A tour of the Zilog Z80", "https://oldbit.example/z80", "oldbit.example", 210, 17, at.Add(-40*time.Minute)),
		newsStoryMap(5, 15, "Show HN: a spreadsheet of the town", "https://show.example/sheet", "show.example", 64, 8, at.Add(-20*time.Minute)),
	}

	dom := newPageDOM(t)
	dom.call(t, "renderNews", newsReading(stories, at, ""))

	if sec := dom.element(t, "newssec"); sec.Hidden {
		t.Error("the pane stays hidden with stories to draw")
	}
	box := dom.element(t, "news")
	if box.Kids != 5 {
		t.Errorf("the pane drew %d lines, want 5", box.Kids)
	}
	if box.Class != "" {
		t.Errorf("a current reading is not dimmed, but the pane wears %q", box.Class)
	}

	title := dom.descend(t, "news", 0, 0)
	if title.Text != "1. Postgres 18" {
		t.Errorf("the first line reads %q, want its rank and title", title.Text)
	}
	if title.Href != "https://www.postgresql.org/about/news/pg18/" {
		t.Errorf("the title links to %q, want the story", title.Href)
	}
	if title.Target != "_blank" || title.Rel != "noopener noreferrer" {
		t.Errorf("the title link opens with target=%q rel=%q, want a new tab that cannot reach back", title.Target, title.Rel)
	}

	cmt := dom.descend(t, "news", 0, 1)
	if cmt.Href != itemURL(11) {
		t.Errorf("the comments link goes to %q, want the discussion page", cmt.Href)
	}
	if cmt.Target != "_blank" || cmt.Rel != "noopener noreferrer" {
		t.Errorf("the comments link opens with target=%q rel=%q, want a new tab that cannot reach back", cmt.Target, cmt.Rel)
	}
	if !strings.Contains(cmt.Text, "96") {
		t.Errorf("the comments link reads %q, want the count", cmt.Text)
	}

	// The Ask HN post has no url of its own: the server sends it the discussion
	// page, and the pane links there.
	if ask := dom.descend(t, "news", 2, 0); ask.Href != itemURL(13) {
		t.Errorf("an Ask HN line links to %q, want its discussion page", ask.Href)
	}

	// The hover carries what the one-line row cannot: the whole title, the
	// points, the comments, the story's age and where it is from.
	hover := dom.descend(t, "news", 0).Title
	for _, want := range []string{"Postgres 18", "412 points", "96 comments", "postgresql.org"} {
		if !strings.Contains(hover, want) {
			t.Errorf("the line's hover %q does not carry %q", hover, want)
		}
	}
	if !strings.Contains(hover, "3h") {
		t.Errorf("the line's hover %q does not age the story", hover)
	}
	// The domain is left off where there is none rather than drawn as an empty
	// part of the line.
	if askHover := dom.descend(t, "news", 2).Title; strings.Contains(askHover, " ·  · ") || strings.HasSuffix(askHover, "· ") {
		t.Errorf("an Ask HN line's hover %q ends in a separator with nothing after it", askHover)
	}

	if note := dom.element(t, "newsnote"); !strings.HasPrefix(note.Text, "read ") || !strings.HasSuffix(note.Text, " ago") {
		t.Errorf("a current reading's note reads %q, want when it was read", note.Text)
	}
}

// A refresh that failed keeps the five stories it last read, dimmed, under a
// note that says how old they are and why: a reader who is shown a stale front
// page must be able to tell it from a live one (gt-9ktrr).
func TestNewsPaneDimsTheLastGoodStoriesUnderAStaleNote(t *testing.T) {
	t.Parallel()

	at := time.Now().Add(-12 * time.Minute)
	stories := []any{newsStoryMap(1, 11, "Postgres 18", "https://www.postgresql.org/about/news/pg18/", "postgresql.org", 412, 96, at)}
	dom := newPageDOM(t)
	dom.call(t, "renderNews", newsReading(stories, at, "unreachable"))

	box := dom.element(t, "news")
	if box.Kids != 1 {
		t.Errorf("the pane drew %d lines, want the last good story", box.Kids)
	}
	if !strings.Contains(box.Class, "stale") {
		t.Errorf("the last good stories are drawn %q, not dimmed as stale", box.Class)
	}
	note := dom.element(t, "newsnote")
	if !strings.HasPrefix(note.Text, "stale ") {
		t.Errorf("the note reads %q, want a stale reading named as one", note.Text)
	}
	if !strings.Contains(note.Text, "unreachable") {
		t.Errorf("the note %q does not say why the reading is stale", note.Text)
	}
	if !strings.Contains(note.Class, "badc") {
		t.Errorf("the stale note wears %q, want the page's failure colour", note.Class)
	}
}

// Before any successful read there is nothing to show, so the pane says it is
// unavailable rather than drawing an empty front page as a quiet one.
func TestNewsPaneSaysUnavailableBeforeAnySuccess(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderNews", newsReading([]any{}, time.Time{}, "http 502"))

	box := dom.element(t, "news")
	if box.Kids != 1 || !strings.Contains(box.Text, "unavailable") {
		t.Errorf("the pane reads %q, want it to say it is unavailable", box.Text)
	}
	if !strings.Contains(box.Text, "http 502") {
		t.Errorf("the pane %q does not say why", box.Text)
	}
	if strings.Contains(box.Class, "stale") {
		t.Error("a pane with nothing to show is not a stale reading of stories")
	}
}

// The pane is drawn only when the server sends one: with the pane switched off
// there is no news key, and the section is hidden rather than left as an empty
// box. A reconnect that has not heard from the poller yet is the same picture.
func TestNewsPaneIsLeftOutWithoutAReading(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		state map[string]any
	}{
		{"no news key at all", map[string]any{}},
		{"a reader with nothing to report", map[string]any{"news": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			// A drawn pane first, so the test holds the redraw to clearing it.
			dom.call(t, "renderNews", newsReading([]any{newsStoryMap(1, 11, "Postgres 18", "https://example.com/pg", "example.com", 1, 1, time.Now())}, time.Now(), ""))
			dom.call(t, "renderNews", tc.state)

			if sec := dom.element(t, "newssec"); !sec.Hidden {
				t.Error("the section is still shown with no news to draw")
			}
			if box := dom.element(t, "news"); box.Kids != 0 {
				t.Errorf("the pane still holds %d elements", box.Kids)
			}
			if note := dom.element(t, "newsnote"); note.Text != "" {
				t.Errorf("the note still reads %q", note.Text)
			}
		})
	}
}

// A story's title is another site's text. It goes through the page's element
// helper, which writes textContent, so a title carrying markup reads as that
// markup and never as a tag the browser would run (gt-9ktrr).
func TestNewsPaneWritesATitleAsTextNeverAsMarkup(t *testing.T) {
	t.Parallel()

	title := `<img src=x onerror="alert(1)"> free money`
	dom := newPageDOM(t)
	dom.call(t, "renderNews", newsReading([]any{newsStoryMap(1, 11, title, "https://example.com/x", "example.com", 1, 0, time.Now())}, time.Now(), ""))

	if got := dom.descend(t, "news", 0, 0).Text; got != "1. "+title {
		t.Errorf("the line reads %q, want the title's own characters", got)
	}
	if draw := pageFunc(t, "renderNews"); strings.Contains(draw, "innerHTML") || strings.Contains(draw, "insertAdjacentHTML") {
		t.Error("renderNews writes markup rather than text")
	}
}
