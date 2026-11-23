package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Login OTP sessions are bound to the phone number they were sent to.
//
// Message Central's verificationId only proves "someone read the code sent to
// SOME number". Before this, /auth/otp/login/verify trusted the phone from the
// request body, so a caller could verify an OTP sent to their own phone and
// log in as any other phone number (account + wallet takeover).
//
// The session_id handed to clients is now "<verificationId>.<hmac>", where the
// HMAC covers the verificationId and the normalized phone. Clients treat
// session_id as opaque, so the web and mobile apps need no change.

// loginPhone strictly normalizes a login phone to "+91XXXXXXXXXX": spaces,
// dashes and brackets are ignored; what remains must be exactly 10 digits,
// optionally prefixed by 91. Anything else is rejected rather than truncated,
// so the number the SMS goes to, the number sealed into the session and the
// number logged in as are always the same one.
func loginPhone(raw string) (string, bool) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0, r == ' ', r == '-', r == '(', r == ')':
		default:
			return "", false
		}
	}
	d := b.String()
	if len(d) == 12 && strings.HasPrefix(d, "91") {
		d = d[2:]
	}
	if len(d) != 10 {
		return "", false
	}
	return "+91" + d, true
}

func otpSessionMAC(secret, verificationID, phone string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(verificationID + "|" + phone))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// sealOTPSession returns the client-facing session id for an OTP sent to
// phone, which must already be normalized by loginPhone.
func sealOTPSession(secret, verificationID, phone string) string {
	return verificationID + "." + otpSessionMAC(secret, verificationID, phone)
}

// openOTPSession returns the provider verificationId when sessionID was issued
// for this phone, and ok=false otherwise (tampered, or a different phone).
func openOTPSession(secret, sessionID, phone string) (verificationID string, ok bool) {
	phone, valid := loginPhone(phone)
	i := strings.LastIndex(sessionID, ".")
	if i <= 0 || !valid {
		return "", false
	}
	verificationID, mac := sessionID[:i], sessionID[i+1:]
	want := otpSessionMAC(secret, verificationID, phone)
	return verificationID, hmac.Equal([]byte(mac), []byte(want))
}
