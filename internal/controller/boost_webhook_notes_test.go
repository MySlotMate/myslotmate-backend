package controller

import (
	"encoding/json"
	"testing"
)

// Razorpay sends notes as an object, or as [] when a payment has none; the
// wallet webhook must not break on either.
func TestPaymentWebhookNotes(t *testing.T) {
	for _, body := range []string{
		`{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_1","order_id":"order_1","amount":826000,"notes":{"boost_payment_id":"abc"}}}}}`,
		`{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_1","order_id":"order_1","amount":100,"notes":[]}}}}`,
	} {
		var e RazorpayPaymentWebhookEvent
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		n := paymentNotes(e.Payload.Payment.Entity.Notes)
		if e.Payload.Payment.Entity.Amount == 826000 && n["boost_payment_id"] != "abc" {
			t.Fatalf("notes = %v", n)
		}
	}
}
