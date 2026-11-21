package service

import (
	"testing"
	"time"
)

// Same cases as Myslotmate-backend-node/tests/cancellation-policy.test.ts —
// both backends must agree on whether a guest cancel is refunded.
func TestRefundDueOnCancel(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	in := func(h float64) time.Time { return now.Add(time.Duration(h * float64(time.Hour))) }
	p := func(s string) *string { return &s }

	cases := []struct {
		name   string
		policy *string
		at     time.Time
		want   bool
	}{
		{"flexible at 24h", p("flexible"), in(24), true},
		{"flexible at 23.9h", p("flexible"), in(23.9), false},
		{"moderate at 120h", p("moderate"), in(120), true},
		{"moderate at 119h", p("moderate"), in(119), false},
		{"no_refund", p("no_refund"), in(24 * 60), false},
		{"strict", p("strict"), in(24 * 60), false},
		{"unset", nil, in(1), true},
		{"unknown", p("lenient"), in(1), true},
	}
	for _, c := range cases {
		if got := RefundDueOnCancel(c.policy, c.at, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
