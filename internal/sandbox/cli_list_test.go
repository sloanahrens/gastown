//go:build !integration

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// wireSandbox and wireList are the `sandbox list -o json` reply, written the
// way the gateway writes it.
type wireSandbox struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

type wireList struct {
	NextPageToken string        `json:"next_page_token,omitempty"`
	Sandboxes     []wireSandbox `json:"sandboxes"`
}

// listReply is one page of rows and the token that follows it, "" on the last.
func listReply(token string, rows ...wireSandbox) string {
	if rows == nil {
		rows = []wireSandbox{}
	}
	b, err := json.Marshal(wireList{NextPageToken: token, Sandboxes: rows})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// fullPage is a listPageSize page of Ready rows named by index, followed by the
// continuation token.
func fullPage(from int, token string) string {
	rows := make([]wireSandbox, 0, listPageSize)
	for i := 0; i < listPageSize; i++ {
		rows = append(rows, wireSandbox{Name: fmt.Sprintf("gt-r-%03d", from+i), Phase: "Ready"})
	}
	return listReply(token, rows...)
}

// pagedRunner answers each `sandbox list` call from pages, keyed by the
// --page-token the call carries ("" for the first page), and records the
// token of every call.
func pagedRunner(pages map[string]string, seen *[]string) Runner {
	return func(_ context.Context, argv []string, _ []byte, _ []string) ([]byte, []byte, error) {
		token := ""
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--page-token" {
				token = argv[i+1]
			}
		}
		*seen = append(*seen, token)
		page, ok := pages[token]
		if !ok {
			return nil, []byte("unknown token " + token), errors.New("exit status 1")
		}
		return []byte(page), nil, nil
	}
}

func TestCLIList(t *testing.T) {
	t.Parallel()

	t.Run("parses names and phases from a captured two-page listing", func(t *testing.T) {
		t.Parallel()
		// The two pages are what `openshell -g k8s sandbox list --page-size 2
		// -o json` printed against the live gateway: page one carries a token,
		// page two does not.
		page1, err := os.ReadFile("testdata/sandbox_list_page1.json")
		if err != nil {
			t.Fatal(err)
		}
		page2, err := os.ReadFile("testdata/sandbox_list_page2.json")
		if err != nil {
			t.Fatal(err)
		}
		got1, token1, err := parseListPage(page1)
		if err != nil {
			t.Fatal(err)
		}
		want1 := []Entry{{Name: "gt-dementus", Phase: "Ready"}, {Name: "gt-furiosa", Phase: "Ready"}}
		if !reflect.DeepEqual(got1, want1) || token1 != "CAESDUxpc3RTYW5kYm94ZXMaIF3A" {
			t.Errorf("page one = %v, token %q; want %v, token %q", got1, token1, want1, "CAESDUxpc3RTYW5kYm94ZXMaIF3A")
		}
		got2, token2, err := parseListPage(page2)
		if err != nil {
			t.Fatal(err)
		}
		want2 := []Entry{{Name: "gt-slit", Phase: "Ready"}}
		if !reflect.DeepEqual(got2, want2) || token2 != "" {
			t.Errorf("page two = %v, token %q; want %v and no token", got2, token2, want2)
		}
	})

	t.Run("follows the paging token across two pages", func(t *testing.T) {
		t.Parallel()
		var seen []string
		pages := map[string]string{
			"":      fullPage(0, "tok-2"),
			"tok-2": listReply("", wireSandbox{Name: "gt-r-alpha", Phase: "Ready"}, wireSandbox{Name: "gt-r-beta", Phase: "Provisioning"}),
		}
		cli := &CLI{Gateway: "openshell", Run: pagedRunner(pages, &seen)}
		got, err := cli.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != listPageSize+2 {
			t.Fatalf("List returned %d entries, want %d", len(got), listPageSize+2)
		}
		if got[0].Name != "gt-r-000" || got[listPageSize].Name != "gt-r-alpha" {
			t.Errorf("entries out of order across the page boundary: first %v, first of page two %v", got[0], got[listPageSize])
		}
		if got[listPageSize+1] != (Entry{Name: "gt-r-beta", Phase: "Provisioning"}) {
			t.Errorf("last entry = %v, want gt-r-beta in Provisioning", got[listPageSize+1])
		}
		if want := []string{"", "tok-2"}; !reflect.DeepEqual(seen, want) {
			t.Errorf("page tokens asked = %q, want %q", seen, want)
		}
	})

	t.Run("sends the page size and the json output with every call", func(t *testing.T) {
		t.Parallel()
		cli, f := newFakeCLI()
		f.Reply = map[string]string{"list": listReply("")}
		if _, err := cli.List(context.Background()); err != nil {
			t.Fatal(err)
		}
		wantCall(t, f, "openshell", "-g", "openshell", "sandbox", "list", "--page-size", "100", "--output", "json")
	})

	t.Run("a failed command is an error, not an empty list", func(t *testing.T) {
		t.Parallel()
		cli, f := newFakeCLI()
		f.Err = map[string]error{"list": errors.New("exit status 1")}
		f.Stderr = map[string]string{"list": "gateway unreachable"}
		got, err := cli.List(context.Background())
		if err == nil {
			t.Fatalf("List = %v, nil error; a failed listing must not read as empty", got)
		}
		if got != nil {
			t.Errorf("List returned rows %v with an error", got)
		}
		if !strings.Contains(err.Error(), "gateway unreachable") {
			t.Errorf("error %q does not carry the command's stderr", err)
		}
	})

	t.Run("no sandboxes is an empty list", func(t *testing.T) {
		t.Parallel()
		cli := &CLI{Gateway: "openshell", Run: (&Fake{Reply: map[string]string{"list": listReply("")}}).Runner()}
		got, err := cli.List(context.Background())
		if err != nil || len(got) != 0 {
			t.Fatalf("List = %v, %v; want no rows and no error", got, err)
		}
	})

	refusals := []struct {
		name  string
		reply string
		// msg, when set, is the part of the error a refusal must name.
		msg string
	}{
		{"full page with no continuation token", fullPage(0, ""), "no continuation token"},
		{"output that is the table form, not JSON", "NAME          CREATED              PHASE\ngt-r-alpha    2026-11-12 09:14:02  Ready\n", ""},
		{"empty output", "", ""},
		{"page with no sandboxes field", `{"next_page_token":""}`, "no sandboxes field"},
		{"page with null sandboxes", `{"sandboxes":null}`, "no sandboxes field"},
		{"row with no phase", `{"sandboxes":[{"name":"gt-r-alpha"}]}`, "no name or phase"},
		{"row with no name", `{"sandboxes":[{"phase":"Ready"}]}`, "no name or phase"},
	}
	for _, tc := range refusals {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			t.Parallel()
			cli := &CLI{Gateway: "openshell", Run: pagedRunner(map[string]string{"": tc.reply}, new([]string))}
			got, err := cli.List(context.Background())
			if err == nil {
				t.Fatalf("List = %v, nil error; an unreadable listing must be an error", got)
			}
			if tc.msg != "" && !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error %q does not name %q", err, tc.msg)
			}
		})
	}

	t.Run("a listing that never ends is an error at the page bound", func(t *testing.T) {
		t.Parallel()
		calls := 0
		run := func(_ context.Context, _ []string, _ []byte, _ []string) ([]byte, []byte, error) {
			calls++
			return []byte(fullPage(0, fmt.Sprintf("tok-%d", calls))), nil, nil
		}
		cli := &CLI{Gateway: "openshell", Run: run}
		if _, err := cli.List(context.Background()); err == nil || !strings.Contains(err.Error(), "more than") {
			t.Fatalf("List error = %v, want the page bound refused", err)
		}
		if calls != listMaxPages {
			t.Errorf("made %d calls, want %d (the bound stops the loop)", calls, listMaxPages)
		}
	})

	t.Run("a repeated continuation token is an error", func(t *testing.T) {
		t.Parallel()
		var seen []string
		pages := map[string]string{"": fullPage(0, "tok-1"), "tok-1": fullPage(100, "tok-1")}
		cli := &CLI{Gateway: "openshell", Run: pagedRunner(pages, &seen)}
		if _, err := cli.List(context.Background()); err == nil {
			t.Fatal("List followed a token that came back unchanged; it must stop")
		}
		if len(seen) != 2 {
			t.Errorf("made %d calls, want 2 (the repeat stops the loop)", len(seen))
		}
	})
}
