//go:build !integration

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// listTable is a `sandbox list` page in the shape the NAME header and the
// PHASE column give it. The CREATED cell holds spaces, so PHASE is only found
// by its column.
const listTable = `NAME          CREATED              PHASE
gt-r-alpha    2026-11-12 09:14:02  Ready
gt-r-beta     2026-11-12 09:20:44  Provisioning
`

// fullPage is a listPageSize page of Ready rows named by index, followed by the
// continuation token.
func fullPage(from int, token string) string {
	var b strings.Builder
	b.WriteString("NAME          CREATED              PHASE\n")
	for i := 0; i < listPageSize; i++ {
		fmt.Fprintf(&b, "gt-r-%03d     2026-11-12 09:14:02  Ready\n", from+i)
	}
	if token != "" {
		b.WriteString(listTokenPrefix + " " + token + "\n")
	}
	return b.String()
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

	t.Run("parses names and phases from a captured table", func(t *testing.T) {
		t.Parallel()
		cli := &CLI{Gateway: "openshell", Run: pagedRunner(map[string]string{"": listTable}, new([]string))}
		got, err := cli.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := []Entry{{Name: "gt-r-alpha", Phase: "Ready"}, {Name: "gt-r-beta", Phase: "Provisioning"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("List = %v, want %v", got, want)
		}
	})

	t.Run("follows the paging token across two pages", func(t *testing.T) {
		t.Parallel()
		var seen []string
		pages := map[string]string{
			"":      fullPage(0, "tok-2"),
			"tok-2": listTable,
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
		if want := []string{"", "tok-2"}; !reflect.DeepEqual(seen, want) {
			t.Errorf("page tokens asked = %q, want %q", seen, want)
		}
	})

	t.Run("sends the page size with every call", func(t *testing.T) {
		t.Parallel()
		cli, f := newFakeCLI()
		f.Reply = map[string]string{"list": listTable}
		if _, err := cli.List(context.Background()); err != nil {
			t.Fatal(err)
		}
		wantCall(t, f, "openshell", "-g", "openshell", "sandbox", "list", "--page-size", "100")
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
		cli := &CLI{Gateway: "openshell", Run: (&Fake{Reply: map[string]string{"list": "No sandboxes found.\n"}}).Runner()}
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
		{"output with no NAME header", "gt-r-alpha  2026-11-12  Ready\n", ""},
		{"header with no PHASE column", "NAME  CREATED\ngt-r-alpha  2026-11-12\n", ""},
		{"row with no phase cell", "NAME          CREATED              PHASE\ngt-r-alpha    2026-11-12 09:14:02\n", ""},
		{"empty output", "", ""},
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
