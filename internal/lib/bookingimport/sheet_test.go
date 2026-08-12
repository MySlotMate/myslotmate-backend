package bookingimport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// buildSheet writes a throwaway .xlsx with the given rows (row 0 = header).
func buildSheet(t *testing.T, rows [][]string) *bytes.Buffer {
	t.Helper()
	f := excelize.NewFile()
	idx, err := f.NewSheet(SheetName)
	if err != nil {
		t.Fatalf("NewSheet: %v", err)
	}
	f.SetActiveSheet(idx)
	_ = f.DeleteSheet("Sheet1")
	for r, row := range rows {
		for c, val := range row {
			col, _ := excelize.ColumnNumberToName(c + 1)
			if err := f.SetCellStr(SheetName, col+itoa(r+1), val); err != nil {
				t.Fatalf("SetCellStr: %v", err)
			}
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return &buf
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// The template must satisfy the validator that guards uploads. If this ever
// fails, every host who downloads a template gets a header error on upload.
func TestTemplateRoundTrips(t *testing.T) {
	f, err := BuildTemplate()
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}

	rows, err := Parse(&buf)
	if err != nil {
		t.Fatalf("template failed its own validator: %v", err)
	}
	// The example row plus the instruction notes; the example must be valid.
	if len(rows) == 0 {
		t.Fatal("expected the example row to parse")
	}
	if rows[0].Name != "Asha Menon" || rows[0].Phone != "+919876543210" {
		t.Fatalf("example row parsed wrong: %+v", rows[0])
	}
	if rows[0].Err != "" {
		t.Fatalf("example row should be valid, got error: %s", rows[0].Err)
	}
}

func TestParseHeaderValidation(t *testing.T) {
	// Missing the required Phone column.
	_, err := Parse(buildSheet(t, [][]string{
		{"Name", "Quantity"},
		{"Asha", "1"},
	}))
	if err == nil {
		t.Fatal("expected a header error when Phone is missing")
	}
	herr, ok := err.(*HeaderError)
	if !ok {
		t.Fatalf("expected *HeaderError, got %T: %v", err, err)
	}
	if len(herr.Missing) != 1 || herr.Missing[0] != "Phone" {
		t.Fatalf("expected Phone reported missing, got %v", herr.Missing)
	}
}

func TestParseAcceptsReorderedAndMessyHeaders(t *testing.T) {
	rows, err := Parse(buildSheet(t, [][]string{
		{"phone number", "  NAME  ", "Notes", "quantity"},
		{"9876543210", "Asha", "vip", "2"},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// Reordered columns, an alias ("phone number"), casing/padding noise and an
	// unknown extra column ("Notes") must all be tolerated.
	if rows[0].Name != "Asha" || rows[0].Phone != "+919876543210" || rows[0].Quantity != 2 {
		t.Fatalf("messy-header row parsed wrong: %+v", rows[0])
	}
}

// When a sheet carries both the canonical header and an alias, the canonical one
// must win — otherwise the importer silently reads the wrong column.
func TestCanonicalHeaderBeatsAlias(t *testing.T) {
	rows, err := Parse(buildSheet(t, [][]string{
		{"Name", "Mobile", "Phone"},
		{"Asha", "9111111111", "9876543210"},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rows[0].Phone != "+919876543210" {
		t.Fatalf("expected the canonical Phone column to win, got %q", rows[0].Phone)
	}
}

func TestParseRowOutcomes(t *testing.T) {
	rows, err := Parse(buildSheet(t, [][]string{
		{"Name", "Phone", "Quantity"},
		{"Asha Menon", "+91 98765-43210", "2"},
		{"", "9876543211", "1"},          // missing name
		{"Bad Phone", "12345", "1"},      // too short
		{"Low Digit", "1234567890", "1"}, // not an Indian mobile
		{"", "", ""},                     // blank line, skipped entirely
		{"Bad Qty", "9876543212", "abc"}, // unparseable quantity
		{"Default Qty", "9876543213", ""},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("expected 6 rows (blank line skipped), got %d", len(rows))
	}

	if rows[0].Phone != "+919876543210" || rows[0].Quantity != 2 || rows[0].Err != "" {
		t.Fatalf("row 1 wrong: %+v", rows[0])
	}
	if rows[1].Err == "" {
		t.Fatal("row 2 should fail: missing name")
	}
	if rows[2].Err == "" {
		t.Fatal("row 3 should fail: short phone")
	}
	if rows[3].Err == "" {
		t.Fatal("row 4 should fail: not an Indian mobile")
	}
	if rows[4].Err == "" {
		t.Fatal("row 5 should fail: bad quantity")
	}
	if rows[5].Quantity != 1 || rows[5].Err != "" {
		t.Fatalf("row 6 should default quantity to 1: %+v", rows[5])
	}
	// Row numbers must stay contiguous across the skipped blank line so the host
	// can map a failure back to a line in their file.
	for i, r := range rows {
		if r.RowNumber != i+1 {
			t.Fatalf("row %d has RowNumber %d", i, r.RowNumber)
		}
	}
}

// A guest listed twice in one file must be caught at parse time, naming the
// earlier row. Comparison is on the normalized number, so differently-formatted
// spellings of the same phone still collide.
func TestParseDetectsDuplicatePhones(t *testing.T) {
	rows, err := Parse(buildSheet(t, [][]string{
		{"Name", "Phone", "Quantity"},
		{"Asha Menon", "9876543210", "1"},
		{"Ravi Kumar", "9876543211", "1"},
		// Same guest as row 1, written differently.
		{"Asha M.", "+91 98765-43210", "2"},
		{"Priya S", "09876543210", "1"}, // also row 1
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}

	// First occurrence is kept.
	if rows[0].Err != "" {
		t.Fatalf("row 1 should be valid, got %q", rows[0].Err)
	}
	if rows[1].Err != "" {
		t.Fatalf("row 2 is a different number and should be valid, got %q", rows[1].Err)
	}
	// Later occurrences fail, pointing back at row 1.
	for _, i := range []int{2, 3} {
		if rows[i].Err == "" {
			t.Fatalf("row %d should be flagged as a duplicate", i+1)
		}
		if !strings.Contains(rows[i].Err, "row 1") {
			t.Fatalf("row %d should name row 1, got %q", i+1, rows[i].Err)
		}
	}
}

// A row whose phone can't be read must report the phone problem, not be
// swallowed by duplicate detection (every unparseable phone normalizes to "").
func TestParseDuplicateCheckIgnoresBadPhones(t *testing.T) {
	rows, err := Parse(buildSheet(t, [][]string{
		{"Name", "Phone", "Quantity"},
		{"Bad One", "12345", "1"},
		{"Bad Two", "67890", "1"},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, r := range rows {
		if strings.Contains(r.Err, "duplicate") {
			t.Fatalf("row %d reported as duplicate instead of a bad phone: %q", i+1, r.Err)
		}
		if r.Err == "" {
			t.Fatalf("row %d should fail on its phone", i+1)
		}
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"9876543210", "+919876543210", false},
		{"+91 98765 43210", "+919876543210", false},
		{"091-9876543210", "+919876543210", false},
		{"919876543210", "+919876543210", false},
		{"  9876543210  ", "+919876543210", false},
		// Excel turned a long number into scientific notation.
		{"9.19876543210e+11", "+919876543210", false},
		{"12345", "", true},
		{"", "", true},
		// Ten digits but not a mobile prefix — a mangled cell, not a phone.
		{"1234567890", "", true},
	}
	for _, c := range cases {
		got, err := NormalizePhone(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizePhone(%q) = %q, expected an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizePhone(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
