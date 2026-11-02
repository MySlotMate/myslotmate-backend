package models

import "testing"

// The flat guest fee rides on top of the ticket total, once per paid booking,
// and the booking row must still satisfy amount = service_fee + net_earning
// for the cancel reversal.
func TestBookingFeeSplit(t *testing.T) {
	cases := []struct {
		tickets          int64
		commissionPct    int
		feeCents         int64
		wantFee, wantAmt int64
	}{
		{tickets: 50000, commissionPct: 0, feeCents: 0, wantFee: 0, wantAmt: 50000}, // off: unchanged
		{tickets: 50000, commissionPct: 10, feeCents: 2000, wantFee: 2000, wantAmt: 52000},
		{tickets: 999, commissionPct: 30, feeCents: 1500, wantFee: 1500, wantAmt: 2499}, // flat, not scaled
		{tickets: 0, commissionPct: 30, feeCents: 2000, wantFee: 0, wantAmt: 0},         // free stays free
		{tickets: 50000, commissionPct: 0, feeCents: -500, wantFee: 0, wantAmt: 50000},  // bad data never discounts
	}
	for _, c := range cases {
		commission := c.tickets * int64(c.commissionPct) / 100
		hostEarning := c.tickets - commission
		fee := BookingFeeCents(c.tickets, c.feeCents)
		amount := c.tickets + fee
		serviceFee := commission + fee

		if fee != c.wantFee || amount != c.wantAmt {
			t.Errorf("%+v: fee=%d amount=%d", c, fee, amount)
		}
		if amount != serviceFee+hostEarning {
			t.Errorf("%+v: amount %d != service_fee %d + net_earning %d", c, amount, serviceFee, hostEarning)
		}
	}
}
