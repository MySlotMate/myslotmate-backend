package service

import "time"

// When a guest-initiated cancel earns a refund. Mirrors
// Myslotmate-backend-node/src/modules/booking/cancellation-policy.ts exactly —
// both backends cancel the same bookings, so they must agree.
//
// A host cancelling (one booking or the whole experience) always refunds; this
// is for guest cancels only.
//
// ponytail: `strict` promises 50% a week out, but a partial refund means
// splitting the reversal between guest and host, which neither backend does.
// It is treated as non-refundable until partial reversal exists.

// fullRefundHours: hours before the session a full refund is still due.
// A negative value means never refunded.
var fullRefundHours = map[string]float64{
	"flexible":  24,
	"moderate":  5 * 24,
	"strict":    -1,
	"no_refund": -1,
}

// RefundDueOnCancel reports whether a guest cancelling now is refunded in full.
// An unset or unknown policy keeps the old behaviour (refund), so bad data
// never takes a guest's money.
func RefundDueOnCancel(policy *string, occurrence time.Time, now time.Time) bool {
	if policy == nil {
		return true
	}
	hours, ok := fullRefundHours[*policy]
	if !ok {
		return true
	}
	if hours < 0 {
		return false
	}
	return occurrence.Sub(now) >= time.Duration(hours*float64(time.Hour))
}
