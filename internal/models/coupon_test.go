package models

import "testing"

func TestCouponDiscountCents(t *testing.T) {
	pct := func(n int) *int { return &n }
	cases := []struct {
		name  string
		c     *Coupon
		total int64
		want  int64
	}{
		{"nil coupon", nil, 1000, 0},
		{"access code", &Coupon{}, 1000, 0},
		{"free code is not a discount", &Coupon{GrantsFree: true}, 1000, 0},
		{"20% of 1000", &Coupon{DiscountPercent: pct(20)}, 1000, 200},
		{"floored: 15% of 999", &Coupon{DiscountPercent: pct(15)}, 999, 149},
		{"99% of 1", &Coupon{DiscountPercent: pct(99)}, 1, 0},
		{"zero total", &Coupon{DiscountPercent: pct(50)}, 0, 0},
	}
	for _, tc := range cases {
		got := tc.c.DiscountCents(tc.total)
		if got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
		if got < 0 || got >= tc.total && tc.total > 0 {
			t.Errorf("%s: discount %d must stay below total %d", tc.name, got, tc.total)
		}
	}
}
