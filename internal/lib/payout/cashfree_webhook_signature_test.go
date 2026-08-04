package payout

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// signCashfreePayoutBody reproduces Cashfree's Payouts body-signature scheme:
// strip `signature`, sort remaining keys ascending, concatenate their string
// values, HMAC-SHA256 with the client secret, base64-encode.
func signCashfreePayoutBody(t *testing.T, fields map[string]string, secret string) []byte {
	t.Helper()
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(fields[k])
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(sb.String()))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	out := make(map[string]string, len(fields)+1)
	for k, v := range fields {
		out[k] = v
	}
	out["signature"] = sig
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newTestCashfree(clientSecret string) *CashfreeProvider {
	return &CashfreeProvider{cfg: CashfreeConfig{ClientSecret: clientSecret}}
}

func TestValidateWebhookSignature_PayoutBody_LowBalanceAlert(t *testing.T) {
	const secret = "test_client_secret_123"
	fields := map[string]string{
		"event":          "LOW_BALANCE_ALERT",
		"currentBalance": "100.00",
		"alertTime":      "2026-08-03 21:51:18",
	}
	body := signCashfreePayoutBody(t, fields, secret)

	p := newTestCashfree(secret)
	// Header args are empty for Payouts — verification must come from the body.
	if !p.ValidateWebhookSignature(body, "", "") {
		t.Fatalf("expected valid body signature to pass")
	}
}

func TestValidateWebhookSignature_PayoutBody_TransferSuccess(t *testing.T) {
	const secret = "another_secret"
	fields := map[string]string{
		"event":       "TRANSFER_SUCCESS",
		"transferId":  "a38a763d-891e-40fc-b3a2-e642ba51943a",
		"referenceId": "2373321120",
		"eventTime":   "2026-08-03 21:00:00",
		"reason":      "",
	}
	body := signCashfreePayoutBody(t, fields, secret)

	p := newTestCashfree(secret)
	if !p.ValidateWebhookSignature(body, "", "") {
		t.Fatalf("expected valid transfer webhook signature to pass")
	}
}

func TestValidateWebhookSignature_PayoutBody_TamperedRejected(t *testing.T) {
	const secret = "s3cr3t"
	fields := map[string]string{
		"event":          "LOW_BALANCE_ALERT",
		"currentBalance": "100.00",
		"alertTime":      "2026-08-03 21:51:18",
	}
	body := signCashfreePayoutBody(t, fields, secret)

	// Tamper with a signed field after signing.
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	m["currentBalance"] = "999999.00"
	tampered, _ := json.Marshal(m)

	p := newTestCashfree(secret)
	if p.ValidateWebhookSignature(tampered, "", "") {
		t.Fatalf("expected tampered payload to be rejected")
	}
}

func TestValidateWebhookSignature_PayoutBody_WrongSecretRejected(t *testing.T) {
	fields := map[string]string{"event": "LOW_BALANCE_ALERT", "currentBalance": "1.00", "alertTime": "t"}
	body := signCashfreePayoutBody(t, fields, "real_secret")

	p := newTestCashfree("wrong_secret")
	if p.ValidateWebhookSignature(body, "", "") {
		t.Fatalf("expected signature under a different secret to be rejected")
	}
}

// signCashfreePayoutForm builds an x-www-form-urlencoded transfer webhook body
// (as Cashfree Payouts actually sends), signed the same way: ksort field values,
// HMAC-SHA256 with client secret, base64, then URL-encoded into the query string.
func signCashfreePayoutForm(t *testing.T, fields map[string]string, secret string) []byte {
	t.Helper()
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(fields[k])
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(sb.String()))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	q := url.Values{}
	for k, v := range fields {
		q.Set(k, v)
	}
	q.Set("signature", sig)
	return []byte(q.Encode())
}

func TestValidateWebhookSignature_PayoutForm_TransferFailed(t *testing.T) {
	const secret = "form_secret_xyz"
	// Mirrors the real payload: event=TRANSFER_FAILED&transferId=..&referenceId=..&reason=..
	fields := map[string]string{
		"event":       "TRANSFER_FAILED",
		"transferId":  "227e1a1f-e258-4551-819c-d9e6087aaba6",
		"referenceId": "2374845107",
		"reason":      "SOURCE_BANK_DECLINED",
	}
	body := signCashfreePayoutForm(t, fields, secret)

	p := newTestCashfree(secret)
	if !p.ValidateWebhookSignature(body, "", "") {
		t.Fatalf("expected valid form-encoded transfer webhook signature to pass")
	}
}

func TestValidateWebhookSignature_PayoutForm_TamperedRejected(t *testing.T) {
	const secret = "form_secret_xyz"
	fields := map[string]string{
		"event":      "TRANSFER_SUCCESS",
		"transferId": "227e1a1f-e258-4551-819c-d9e6087aaba6",
		"referenceId": "2374845107",
	}
	body := signCashfreePayoutForm(t, fields, secret)
	// Flip TRANSFER_SUCCESS → TRANSFER_FAILED after signing.
	tampered := []byte(strings.Replace(string(body), "TRANSFER_SUCCESS", "TRANSFER_FAILED", 1))

	p := newTestCashfree(secret)
	if p.ValidateWebhookSignature(tampered, "", "") {
		t.Fatalf("expected tampered form-encoded payload to be rejected")
	}
}

func TestValidateWebhookSignature_NoSecretRejects(t *testing.T) {
	fields := map[string]string{"event": "LOW_BALANCE_ALERT", "currentBalance": "1.00", "alertTime": "t"}
	body := signCashfreePayoutBody(t, fields, "real_secret")

	p := newTestCashfree("") // no configured secret
	if p.ValidateWebhookSignature(body, "sig", "ts") {
		t.Fatalf("expected rejection when no signing key is configured")
	}
}
