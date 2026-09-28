package style

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestStyleVariables(t *testing.T) {
	t.Parallel()
	// Test that all style variables render non-empty output
	tests := []struct {
		name   string
		render func(...string) string
	}{
		{"Success", Success.Render},
		{"Warning", Warning.Render},
		{"Error", Error.Render},
		{"Info", Info.Render},
		{"Dim", Dim.Render},
		{"Bold", Bold.Render},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.render == nil {
				t.Fatalf("Style variable %s should not be nil", tt.name)
			}
			result := tt.render("test message")
			if !strings.Contains(stripAnsi(result), "test message") {
				t.Errorf("Style %s.Render() = %q, want it to contain the text", tt.name, result)
			}
		})
	}
}

func TestPrefixVariables(t *testing.T) {
	t.Parallel()
	// Test that all prefix variables are non-empty
	tests := []struct {
		name   string
		prefix string
	}{
		{"SuccessPrefix", SuccessPrefix},
		{"WarningPrefix", WarningPrefix},
		{"ErrorPrefix", ErrorPrefix},
		{"ArrowPrefix", ArrowPrefix},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prefix == "" {
				t.Errorf("Prefix variable %s should not be empty", tt.name)
			}
		})
	}
}

func TestPrintWarning(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	fprintWarning(&buf, "test warning: %s", "value")
	out := stripAnsi(buf.String())
	if !strings.Contains(out, "Warning:") || !strings.HasSuffix(out, " test warning: value\n") {
		t.Errorf("fprintWarning output = %q, want a Warning: label and the formatted message on one line", out)
	}
}

func TestPrintWarning_NoFormatArgs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	fprintWarning(&buf, "simple warning")
	if !strings.HasSuffix(stripAnsi(buf.String()), " simple warning\n") {
		t.Errorf("fprintWarning output = %q, want it to end with the message", buf.String())
	}
}

func TestMultiplePrintWarning(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		fprintWarning(&buf, "warning %d", i)
	}
	if n := strings.Count(buf.String(), "\n"); n != 3 {
		t.Errorf("Expected 3 lines of output, got %d: %q", n, buf.String())
	}
}

func TestStripAnsi(t *testing.T) {
	t.Parallel()
	if got := stripAnsi("\x1b[1;32mok\x1b[0m plain"); got != "ok plain" {
		t.Errorf("stripAnsi = %q, want %q", got, "ok plain")
	}
}

func TestTableRender(t *testing.T) {
	t.Parallel()
	tbl := NewTable(
		Column{Name: "ID", Width: 4},
		Column{Name: "N", Width: 3, Align: AlignRight},
		Column{Name: "C", Width: 5, Align: AlignCenter},
	).SetIndent("> ")
	tbl.AddRow("a", "1", "x").AddRow("toolong", "22") // short row is padded

	lines := strings.Split(strings.TrimSuffix(stripAnsi(tbl.Render()), "\n"), "\n")
	want := []string{
		"> ID     N   C  ",
		"> " + strings.Repeat("─", 4+1+3+1+5),
		"> a      1   x  ",
		"> t...  22      ",
	}
	if len(lines) != len(want) {
		t.Fatalf("Render() lines = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestTableRenderNoSeparatorAndColumnStyle pins current behaviour, quirk
// included: Render applies Column.Style only when col.Style.Value() != "",
// which holds only for a style built with SetString. A style carrying
// colours alone is never applied. No caller sets Column.Style today; if the
// check is changed to apply any non-zero style, update this test with it.
func TestTableRenderNoSeparatorAndColumnStyle(t *testing.T) {
	t.Parallel()
	st := lipgloss.NewStyle().SetString("") // Value() == "": style not applied
	tbl := NewTable(Column{Name: "A", Width: 2, Style: st}).SetHeaderSeparator(false)
	tbl.AddRow("x")
	if got := stripAnsi(tbl.Render()); got != "  A \n  x \n" {
		t.Errorf("Render() = %q, want header and row with no separator", got)
	}

	styled := NewTable(Column{Name: "A", Width: 6, Style: lipgloss.NewStyle().SetString("pre")})
	styled.AddRow("x")
	if got := stripAnsi(styled.Render()); !strings.Contains(got, "pre x") {
		t.Errorf("Render() with a column style = %q, want the style applied to the cell", got)
	}
}

func TestTableRenderNoColumns(t *testing.T) {
	t.Parallel()
	if got := NewTable().Render(); got != "" {
		t.Errorf("Render() with no columns = %q, want empty", got)
	}
}

func ExamplePrintWarning() {
	// PrintWarning writes to stderr (not stdout) to avoid contaminating
	// structured output like JSON. This example demonstrates usage;
	// output appears on stderr, not in the example output below.
	PrintWarning("This is a warning message")
	PrintWarning("Warning with value: %d", 42)
}
