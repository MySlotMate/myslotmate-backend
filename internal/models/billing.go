package models

import (
	"time"

	"github.com/google/uuid"
)

// Billing core: GST tax invoices for the guest booking fee, and credit notes
// when it is refunded. Amounts are integer paise.

const (
	InvoiceKindInvoice    = "invoice"
	InvoiceKindCreditNote = "credit_note"

	InvoiceSourceBooking = "booking"
	InvoiceSourcePass    = "pass"
)

// BillingSettings is MySlotMate's own details as the supplier (one row).
type BillingSettings struct {
	Enabled          bool      `json:"enabled"`
	LegalName        string    `json:"legal_name"`
	TradeName        string    `json:"trade_name"`
	GSTIN            string    `json:"gstin"`
	PAN              string    `json:"pan"`
	Address          string    `json:"address"`
	StateCode        string    `json:"state_code"` // from the GSTIN
	Email            string    `json:"email"`
	SACCode          string    `json:"sac_code"`
	GSTRateBps       int       `json:"gst_rate_bps"`
	InvoicePrefix    string    `json:"invoice_prefix"`
	CreditNotePrefix string    `json:"credit_note_prefix"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Missing lists the required fields that are still blank.
func (s *BillingSettings) Missing() []string {
	var out []string
	for _, f := range []struct{ name, v string }{
		{"legal_name", s.LegalName}, {"gstin", s.GSTIN}, {"address", s.Address},
		{"state_code", s.StateCode}, {"sac_code", s.SACCode},
	} {
		if f.v == "" {
			out = append(out, f.name)
		}
	}
	return out
}

// Issuing reports whether new sales get an invoice now.
func (s *BillingSettings) Issuing() bool {
	return s != nil && s.Enabled && len(s.Missing()) == 0
}

// InvoiceSupplier is the supplier block copied onto each invoice.
type InvoiceSupplier struct {
	LegalName string `json:"legal_name"`
	TradeName string `json:"trade_name,omitempty"`
	GSTIN     string `json:"gstin"`
	PAN       string `json:"pan,omitempty"`
	Address   string `json:"address"`
	StateCode string `json:"state_code"`
	Email     string `json:"email,omitempty"`
}

type Invoice struct {
	ID                uuid.UUID       `json:"id"`
	Kind              string          `json:"kind"`
	Number            string          `json:"number"`
	FY                string          `json:"fy"`
	IssuedAt          time.Time       `json:"issued_at"`
	SourceType        string          `json:"source_type"`
	SourceID          uuid.UUID       `json:"source_id"`
	EventID           *uuid.UUID      `json:"event_id,omitempty"`
	EventTitle        string          `json:"event_title"`
	OriginalInvoiceID *uuid.UUID      `json:"original_invoice_id,omitempty"`
	OriginalNumber    string          `json:"original_number,omitempty"` // filled on read
	UserID            uuid.UUID       `json:"user_id"`
	BuyerName         string          `json:"buyer_name"`
	BuyerEmail        string          `json:"buyer_email"`
	BuyerPhone        string          `json:"buyer_phone"`
	PlaceOfSupply     string          `json:"place_of_supply"`
	Supplier          InvoiceSupplier `json:"supplier"`
	TaxableCents      int64           `json:"taxable_cents"`
	CGSTCents         int64           `json:"cgst_cents"`
	SGSTCents         int64           `json:"sgst_cents"`
	IGSTCents         int64           `json:"igst_cents"`
	TotalCents        int64           `json:"total_cents"`
	PDFKey            *string         `json:"-"`
	CreatedAt         time.Time       `json:"created_at"`
	Lines             []InvoiceLine   `json:"lines,omitempty"`
}

type InvoiceLine struct {
	ID           uuid.UUID `json:"id"`
	InvoiceID    uuid.UUID `json:"invoice_id"`
	Position     int       `json:"position"`
	Description  string    `json:"description"`
	SACCode      string    `json:"sac_code"`
	Quantity     int       `json:"quantity"`
	TaxableCents int64     `json:"taxable_cents"`
	GSTRateBps   int       `json:"gst_rate_bps"`
	CGSTCents    int64     `json:"cgst_cents"`
	SGSTCents    int64     `json:"sgst_cents"`
	IGSTCents    int64     `json:"igst_cents"`
	TotalCents   int64     `json:"total_cents"`
}

// InvoiceFilter narrows the admin invoice list.
type InvoiceFilter struct {
	Query  string // number, buyer name, email or phone (substring)
	Kind   string
	From   *time.Time
	To     *time.Time
	Limit  int
	Offset int
}

// BillingDocsFolder is the private S3 folder for archived invoice PDFs.
const BillingDocsFolder = "billing/invoices/"
