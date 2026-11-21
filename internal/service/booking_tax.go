package service

import "myslotmate-backend/internal/models"

// Per-booking statutory tax lines (Marketing Boost v2 PRD, "Payout and tax
// spec" §4). Pure: the booking transaction gathers the inputs, this computes,
// the caller stamps the result on the booking row. The Node backend mirrors it
// in src/lib/tax/booking-tax.ts against the same golden vectors.

// TaxInput is everything the tax lines depend on, in paise.
type TaxInput struct {
	TicketCents        int64 // unit price × qty after coupon discount, excl. the guest fee
	NetEarningCents    int64 // ticket − platform commission − boost commission
	ServiceFeeCents    int64 // what the platform keeps from the guest side (GST-inclusive)
	FYGrossBeforeCents int64 // host's confirmed ticket_cents this FY, EXCLUDING this booking
	Profile            *models.TaxProfile
	EventStateCode     *string
	Settings           models.TaxSettings
}

// TaxLines is what gets stamped on the booking.
type TaxLines struct {
	PlatformFeeGSTCents int64 // GST inside the platform's fee; internal only
	TCSCents            int64
	TCSInterstate       bool
	TDSCents            int64
	HostPayoutCents     int64 // what the host receives for this booking
}

// roundBps is amount × bps / 10000, rounded half-up to the paisa (amount ≥ 0).
func roundBps(amountCents, bps int64) int64 {
	return (amountCents*bps + 5000) / 10000
}

// gstInside is the 18% GST contained in a GST-inclusive amount (18/118), half-up.
func gstInside(amountCents int64) int64 {
	return (amountCents*18 + 59) / 118
}

// ComputeBookingTax returns the tax lines for one booking.
//
//   - TCS: TCSBps of the ticket value, only when the host has a GSTIN on file.
//     Interstate when the venue's state differs from the GSTIN's; an unknown
//     venue state counts as intra-state.
//   - TDS: TDSBps of the ticket value. Always for companies, firms, LLPs and
//     other non-individuals. For individuals and HUFs (and hosts with no
//     profile yet) only once this FY's gross passes the threshold; on the
//     crossing booking it is charged on the whole year-to-date gross (catch-up).
func ComputeBookingTax(in TaxInput) TaxLines {
	out := TaxLines{PlatformFeeGSTCents: gstInside(in.ServiceFeeCents)}
	if !in.Settings.Enabled {
		out.HostPayoutCents = in.NetEarningCents
		return out
	}

	if in.Profile != nil && in.Profile.GSTIN != nil {
		out.TCSCents = roundBps(in.TicketCents, in.Settings.TCSBps)
		out.TCSInterstate = in.EventStateCode != nil && in.Profile.GSTStateCode != nil &&
			*in.EventStateCode != *in.Profile.GSTStateCode
	}

	individual := in.Profile == nil ||
		in.Profile.EntityType == models.EntityIndividual || in.Profile.EntityType == models.EntityHUF
	threshold := in.Settings.TDSThresholdCents
	after := in.FYGrossBeforeCents + in.TicketCents
	switch {
	case !individual:
		out.TDSCents = roundBps(in.TicketCents, in.Settings.TDSBps)
	case after <= threshold:
		// still within the exemption
	case in.FYGrossBeforeCents <= threshold:
		out.TDSCents = roundBps(after, in.Settings.TDSBps) // crossing: catch-up on full YTD
	default:
		out.TDSCents = roundBps(in.TicketCents, in.Settings.TDSBps)
	}

	out.HostPayoutCents = in.NetEarningCents - out.TCSCents - out.TDSCents
	return out
}
