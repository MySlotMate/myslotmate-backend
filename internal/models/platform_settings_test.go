package models

import "testing"

// The guest fee rides on top of the ticket total, once per paid booking, and
// the booking row must still satisfy amount = service_fee + net_earning for
// the cancel reversal. Node's tests/booking-fee.test.ts holds the same cases.
func TestBookingFeeSplit(t *testing.T) {
	flat := func(c int64) BookingFeeConfig { return BookingFeeConfig{Type: BookingFeeFlat, AmountCents: c} }
	pct := func(p int) BookingFeeConfig { return BookingFeeConfig{Type: BookingFeePercentage, Percentage: p} }

	cases := []struct {
		name             string
		tickets          int64
		commissionPct    int
		cfg              BookingFeeConfig
		wantFee, wantAmt int64
	}{
		{"off", 50000, 0, BookingFeeConfig{}, 0, 50000},
		{"flat", 50000, 10, flat(2000), 2000, 52000},
		{"flat not scaled", 999, 30, flat(1500), 1500, 2499},
		{"untyped is flat", 50000, 0, BookingFeeConfig{AmountCents: 2000}, 2000, 52000},
		{"percentage", 50000, 10, pct(5), 2500, 52500},
		{"percentage floors", 999, 30, pct(3), 29, 1028},
		{"percentage ignores amount", 50000, 0, BookingFeeConfig{Type: BookingFeePercentage, Percentage: 2, AmountCents: 9999}, 1000, 51000},
		{"free stays free", 0, 30, flat(2000), 0, 0},
		{"free stays free pct", 0, 30, pct(5), 0, 0},
		{"negative never discounts", 50000, 0, flat(-500), 0, 50000},
	}
	for _, c := range cases {
		commission := c.tickets * int64(c.commissionPct) / 100
		hostEarning := c.tickets - commission
		fee := c.cfg.FeeCents(c.tickets)
		amount := c.tickets + fee
		serviceFee := commission + fee

		if fee != c.wantFee || amount != c.wantAmt {
			t.Errorf("%s: fee=%d amount=%d", c.name, fee, amount)
		}
		if amount != serviceFee+hostEarning {
			t.Errorf("%s: amount %d != service_fee %d + net_earning %d", c.name, amount, serviceFee, hostEarning)
		}
	}
}

func TestBookingFeeValid(t *testing.T) {
	ok := []BookingFeeConfig{
		{}, {Type: BookingFeeFlat, AmountCents: MaxBookingFeeCents}, {Type: BookingFeePercentage, Percentage: 100},
	}
	bad := []BookingFeeConfig{
		{Type: "fixed"}, {AmountCents: MaxBookingFeeCents + 1}, {AmountCents: -1},
		{Type: BookingFeePercentage, Percentage: 101}, {Type: BookingFeePercentage, Percentage: -1},
	}
	for _, c := range ok {
		if !c.Valid() {
			t.Errorf("%+v should be valid", c)
		}
	}
	for _, c := range bad {
		if c.Valid() {
			t.Errorf("%+v should be invalid", c)
		}
	}
}
