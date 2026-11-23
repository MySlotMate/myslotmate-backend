package service

import "testing"

// Regression: an OTP session issued for one phone must not verify a login for
// another phone (account takeover via /auth/otp/login/verify).
func TestOTPSessionBoundToPhone(t *testing.T) {
	const secret = "test-secret"
	p, ok := loginPhone("+91 98765 43210")
	if !ok || p != "+919876543210" {
		t.Fatalf("loginPhone: got %q ok=%v", p, ok)
	}
	sid := sealOTPSession(secret, "123456789", p)

	if vid, ok := openOTPSession(secret, sid, "+919876543210"); !ok || vid != "123456789" {
		t.Fatalf("same phone (other format) rejected: vid=%q ok=%v", vid, ok)
	}
	if _, ok := openOTPSession(secret, sid, "+919999999999"); ok {
		t.Fatal("session accepted for a different phone")
	}
	if _, ok := openOTPSession("other-secret", sid, "+919876543210"); ok {
		t.Fatal("session accepted under a different secret")
	}
	// Over-long / odd numbers are rejected, never truncated to a valid tail.
	for _, raw := range []string{"+91987654321099", "919876543", "+1 9876543210", "98765x43210", "0919876543210"} {
		if _, ok := loginPhone(raw); ok {
			t.Errorf("loginPhone accepted %q", raw)
		}
		if _, ok := openOTPSession(secret, sid, raw); ok {
			t.Errorf("session opened for malformed phone %q", raw)
		}
	}
	for _, bad := range []string{"123456789", "", ".", "123456789.", "x.y"} {
		if _, ok := openOTPSession(secret, bad, "+919876543210"); ok {
			t.Fatalf("malformed session %q accepted", bad)
		}
	}
}
