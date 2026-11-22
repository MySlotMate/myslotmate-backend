package models

import (
	"encoding/json"
	"strings"
)

// PrivateDocsFolder is the upload folder for ID, selfie, certificate and
// degree files. The bucket policy keeps it out of public read; clients get
// short-lived signed links instead.
const PrivateDocsFolder = "hosts/government-ids/"

// SignPrivateURL, when set, turns a stored private-document URL into a
// short-lived signed link. cmd/api wires it to S3 presigning when uploads are
// configured. Host JSON (admin and the host's own views; public DTOs never
// carry these fields) uses it for the four private document URLs.
var SignPrivateURL func(string) string

// MarshalJSON signs the private document URLs on the way out. The stored
// values are untouched.
func (h Host) MarshalJSON() ([]byte, error) {
	type plain Host
	p := plain(h)
	if SignPrivateURL != nil {
		sign := func(u *string) *string {
			if u == nil || *u == "" {
				return u
			}
			s := SignPrivateURL(*u)
			return &s
		}
		p.GovernmentIDURL = sign(p.GovernmentIDURL)
		p.SelfieURL = sign(p.SelfieURL)
		p.CertificateURL = sign(p.CertificateURL)
		p.DegreeURL = sign(p.DegreeURL)
	}
	return json.Marshal(p)
}

// UnsignURL drops a presign query string, so a signed link a client sends
// back (e.g. the admin edit form) is stored as the plain object URL.
func UnsignURL(u *string) *string {
	if u == nil {
		return nil
	}
	if i := strings.IndexByte(*u, '?'); i >= 0 && strings.Contains((*u)[i:], "X-Amz-Signature=") {
		s := (*u)[:i]
		return &s
	}
	return u
}
