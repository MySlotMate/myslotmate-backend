// Package bookingimport owns the .xlsx contract for bulk booking upload: the
// column headers, the parser that validates a host's file against them, and the
// generator that produces the downloadable template.
//
// Template and validator are deliberately in the same file and built from the
// same Columns slice — a hand-maintained template (a static file shipped by the
// frontend, say) drifts from the validator the first time a column is renamed,
// and every host who downloaded the old one starts getting header errors.
package bookingimport

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Column is one expected spreadsheet column.
type Column struct {
	Header   string
	Required bool
	Help     string
	// Aliases are alternative labels accepted for this column. Hosts routinely
	// build their guest list before downloading the template, or rename a header
	// to something they find clearer; rejecting "Phone Number" outright would be
	// pedantry, not validation. The canonical Header always wins when both are
	// present.
	Aliases []string
}

// Columns is the single source of truth for the sheet layout, in order.
//
// Attendee-detail fields are intentionally absent: events with
// requires_attendee_details are rejected before a job is created (they need
// per-guest answers the on-spot modal collects one at a time), so a fixed
// three-column template is always sufficient for an importable event.
var Columns = []Column{
	{
		Header: "Name", Required: true, Help: "Guest's full name",
		Aliases: []string{"guest name", "full name", "guest", "attendee name", "attendee"},
	},
	{
		Header: "Phone", Required: true, Help: "10-digit Indian mobile number",
		Aliases: []string{"phone number", "mobile", "mobile number", "contact", "contact number", "phone no", "mobile no"},
	},
	{
		Header: "Quantity", Required: false, Help: "Number of seats (defaults to 1)",
		Aliases: []string{"qty", "seats", "tickets", "no of seats", "number of seats"},
	},
}

// SheetName is the worksheet the parser reads and the template writes. Hosts
// re-save these files in Excel, Numbers and Google Sheets, so the parser falls
// back to the first sheet rather than insisting on this name.
const SheetName = "Bookings"

// MaxRows caps a single import. The rows are processed sequentially against a
// live capacity check, so an unbounded file would hold a pool worker for a very
// long time.
const MaxRows = 1000

// ParsedRow is one validated data row, ready to be persisted as pending.
type ParsedRow struct {
	// RowNumber counts only non-blank guest rows, so it is the Nth guest in the
	// file rather than a literal spreadsheet line number (blank rows are skipped
	// and don't consume one). The failure report shows name and phone alongside
	// it, which is what hosts actually search on.
	RowNumber int
	Name      string
	Phone     string // normalized to +91XXXXXXXXXX
	Quantity  int
	// Err is set when the row is structurally unusable (missing name, unparseable
	// phone). Such rows are still recorded, as failed, so the host gets a complete
	// account of their file rather than silently losing lines.
	Err string
}

// HeaderError describes a header mismatch precisely enough for the host to fix
// their file without guessing.
type HeaderError struct {
	Missing  []string `json:"missing"`
	Expected []string `json:"expected"`
	Found    []string `json:"found"`
}

func (e *HeaderError) Error() string {
	return fmt.Sprintf("spreadsheet headers are wrong — missing: %s; expected: %s; found: %s",
		strings.Join(e.Missing, ", "),
		strings.Join(e.Expected, ", "),
		strings.Join(e.Found, ", "))
}

// ExpectedHeaders returns the header labels in order.
func ExpectedHeaders() []string {
	out := make([]string, 0, len(Columns))
	for _, c := range Columns {
		out = append(out, c.Header)
	}
	return out
}

// Parse reads an uploaded .xlsx and returns its data rows.
//
// Header validation is strict about presence and loose about everything else:
// required columns must all be there (case- and space-insensitive), extra columns
// are ignored, and column ORDER does not matter — headers are matched by name to
// their index. Hosts reorder columns constantly and there is no reason to punish
// it, but a missing Phone column must be a hard, up-front failure rather than
// 300 identical per-row errors.
func Parse(r io.Reader) ([]ParsedRow, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, fmt.Errorf("could not read the file — please upload a valid .xlsx spreadsheet")
	}
	defer f.Close()

	sheet := SheetName
	if idx, err := f.GetSheetIndex(SheetName); err != nil || idx == -1 {
		sheets := f.GetSheetList()
		if len(sheets) == 0 {
			return nil, fmt.Errorf("the spreadsheet has no sheets")
		}
		sheet = sheets[0]
	}

	rows, err := f.GetRows(sheet)
	if err != nil {
		return nil, fmt.Errorf("could not read the sheet: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("the spreadsheet is empty")
	}

	index, herr := mapHeaders(rows[0])
	if herr != nil {
		return nil, herr
	}

	body := rows[1:]
	if len(body) > MaxRows {
		return nil, fmt.Errorf("too many rows — this file has %d, the limit is %d per upload", len(body), MaxRows)
	}

	out := make([]ParsedRow, 0, len(body))
	rowNum := 0
	// Tracks the first row each phone number appeared on, so a guest listed twice
	// in the same file is caught here rather than at booking time.
	//
	// The booking path would in fact reject the second attempt anyway (the guest
	// resolves to the same account, which already holds a booking for the slot),
	// but it would report the unhelpful "this guest already has a booking for
	// this slot" — true, yet it hides the fact that the host's own sheet is
	// where the duplicate came from. Naming the earlier row is what makes it
	// fixable. Matching is on the NORMALIZED phone, so "+91 98765 43210" and
	// "9876543210" are correctly seen as the same guest.
	firstSeen := make(map[string]int, len(body))
	for _, raw := range body {
		name := strings.TrimSpace(cell(raw, index["Name"]))
		phoneRaw := strings.TrimSpace(cell(raw, index["Phone"]))
		qtyRaw := strings.TrimSpace(cell(raw, index["Quantity"]))

		// Skip blank lines — trailing empty rows are extremely common in files
		// that have been opened and re-saved, and counting them as failures would
		// make every import look broken.
		if name == "" && phoneRaw == "" && qtyRaw == "" {
			continue
		}
		rowNum++

		pr := ParsedRow{RowNumber: rowNum, Name: name, Quantity: 1}

		phone, perr := NormalizePhone(phoneRaw)
		pr.Phone = phone
		if qtyRaw != "" {
			// Excel writes whole numbers from a numeric cell as "2" but a cell that
			// was ever formatted as a decimal comes back "2.0".
			q, err := strconv.Atoi(strings.TrimSuffix(qtyRaw, ".0"))
			if err != nil || q <= 0 {
				pr.Err = fmt.Sprintf("invalid quantity %q — must be a whole number of 1 or more", qtyRaw)
			} else {
				pr.Quantity = q
			}
		}
		switch {
		case name == "":
			pr.Err = "name is required"
		case perr != nil:
			pr.Err = perr.Error()
		default:
			// Only meaningful once the phone parsed — an unreadable number can't be
			// compared against anything. The first occurrence is kept and booked;
			// every later one fails pointing back at it.
			if prev, dup := firstSeen[phone]; dup {
				pr.Err = fmt.Sprintf("duplicate phone number — same guest as row %d", prev)
			} else {
				firstSeen[phone] = rowNum
			}
		}
		out = append(out, pr)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("the spreadsheet has headers but no guest rows")
	}
	return out, nil
}

// mapHeaders resolves each expected column to its position in the header row.
func mapHeaders(header []string) (map[string]int, *HeaderError) {
	seen := make(map[string]int, len(header))
	found := make([]string, 0, len(header))
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		found = append(found, h)
		key := normalizeHeader(h)
		if _, dup := seen[key]; !dup {
			seen[key] = i
		}
	}

	index := make(map[string]int, len(Columns))
	var missing []string
	for _, c := range Columns {
		// Exact header first, then aliases in order — so a sheet carrying both
		// "Phone" and "Phone Number" reads the canonical one rather than whichever
		// happens to come first.
		i, ok := seen[normalizeHeader(c.Header)]
		if !ok {
			for _, alias := range c.Aliases {
				if i, ok = seen[normalizeHeader(alias)]; ok {
					break
				}
			}
		}
		if !ok {
			if c.Required {
				missing = append(missing, c.Header)
			}
			index[c.Header] = -1
			continue
		}
		index[c.Header] = i
	}
	if len(missing) > 0 {
		return nil, &HeaderError{Missing: missing, Expected: ExpectedHeaders(), Found: found}
	}
	return index, nil
}

// normalizeHeader makes header matching forgiving of the cosmetic differences
// spreadsheet editors introduce: case, surrounding space, and the space/underscore
// split in labels like "Phone Number".
func normalizeHeader(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == ' ' || r == '_' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func cell(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return row[idx]
}

// NormalizePhone turns whatever Excel produced into the +91XXXXXXXXXX form the
// rest of the system uses.
//
// This is the single most likely source of real-world import failures. A phone
// column formatted as a number loses its leading zero and, past 11 digits, comes
// back as "9.19876e+11"; hosts also paste "+91 98765-43210", "0 9876543210" and
// "91-9876543210". Everything non-digit is therefore dropped and the last 10
// digits are taken — the same rule userRepository.GetByPhone already applies when
// matching an existing account, so an imported guest reliably lands on their
// existing user row instead of a duplicate.
func NormalizePhone(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("phone number is required")
	}

	// Recover a scientific-notation cell before digit-stripping, which would
	// otherwise turn "9.19876e+11" into the nonsense "91987611".
	if f, err := strconv.ParseFloat(s, 64); err == nil && strings.ContainsAny(s, "eE") {
		s = strconv.FormatFloat(f, 'f', 0, 64)
	}

	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if len(d) < 10 {
		return "", fmt.Errorf("invalid phone number %q — need at least 10 digits", raw)
	}
	last10 := d[len(d)-10:]
	// Indian mobile numbers start 6–9. This catches the classic mis-parse where a
	// mangled cell still yields ten digits (e.g. a date serial), which would
	// otherwise create a junk guest account.
	if last10[0] < '6' {
		return "", fmt.Errorf("invalid phone number %q — not a valid Indian mobile number", raw)
	}
	return "+91" + last10, nil
}

// BuildTemplate generates the .xlsx hosts download, from the same Columns above.
// It carries one greyed example row so the expected phone format is unambiguous.
func BuildTemplate() (*excelize.File, error) {
	f := excelize.NewFile()
	idx, err := f.NewSheet(SheetName)
	if err != nil {
		return nil, err
	}
	f.SetActiveSheet(idx)
	// NewFile seeds a "Sheet1"; drop it so the parser's first-sheet fallback can
	// never land on an empty one.
	if def := f.GetSheetName(0); def != SheetName {
		_ = f.DeleteSheet(def)
	}

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"4F46E5"}, Pattern: 1},
	})
	if err != nil {
		return nil, err
	}
	// Phone as TEXT (format 49) — without this Excel treats the column as numeric
	// and eats the guest's leading zero the moment the host types one.
	textStyle, err := f.NewStyle(&excelize.Style{NumFmt: 49})
	if err != nil {
		return nil, err
	}

	for i, c := range Columns {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetCellStr(SheetName, col+"1", c.Header)
		_ = f.SetCellStyle(SheetName, col+"1", col+"1", headerStyle)
		_ = f.SetColWidth(SheetName, col, col, 24)
		if c.Header == "Phone" {
			_ = f.SetColStyle(SheetName, col, textStyle)
		}
	}

	// One example row, so the phone format is shown rather than described.
	example := map[string]string{"Name": "Asha Menon", "Phone": "9876543210", "Quantity": "1"}
	for i, c := range Columns {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetCellStr(SheetName, col+"2", example[c.Header])
	}

	notes := []string{
		"Delete the example row before uploading.",
		"Name and Phone are required. Quantity is optional and defaults to 1.",
		"Phone must be a 10-digit Indian mobile number. +91 and spaces are fine.",
		"Each phone number can appear only once — list a guest bringing others as one row with a higher Quantity.",
		fmt.Sprintf("Up to %d guests per upload.", MaxRows),
	}
	for i, n := range notes {
		_ = f.SetCellStr(SheetName, fmt.Sprintf("A%d", 4+i), n)
	}
	return f, nil
}
