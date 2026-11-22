package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHostJSON_SignsOnlyPrivateDocuments(t *testing.T) {
	SignPrivateURL = func(u string) string { return u + "?signed" }
	defer func() { SignPrivateURL = nil }()

	id, selfie, cert, deg, photo := "https://b/hosts/government-ids/id.png", "https://b/s.png", "https://b/c.pdf", "https://b/d.pdf", "https://b/general/p.png"
	h := Host{GovernmentIDURL: &id}
	h.SelfieURL, h.CertificateURL, h.DegreeURL = &selfie, &cert, &deg
	h.PortfolioPhotoURLs = []string{photo}
	b, err := json.Marshal(&h)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	for _, k := range []string{"government_id_url", "selfie_url", "certificate_url", "degree_url"} {
		if s, _ := out[k].(string); !strings.HasSuffix(s, "?signed") {
			t.Errorf("%s not signed: %v", k, out[k])
		}
	}
	if p := out["portfolio_photo_urls"].([]any)[0]; p != photo {
		t.Errorf("public photo was rewritten: %v", p)
	}
	if out["host_type"] == nil {
		t.Error("embedded answers lost from JSON")
	}
	if *h.GovernmentIDURL != id {
		t.Error("marshalling mutated the host")
	}
}

func TestHostJSON_NoSignerLeavesURLs(t *testing.T) {
	id := "https://b/hosts/government-ids/id.png"
	b, _ := json.Marshal(Host{GovernmentIDURL: &id})
	if !strings.Contains(string(b), `"government_id_url":"`+id+`"`) {
		t.Fatalf("got %s", b)
	}
}

func TestUnsignURL(t *testing.T) {
	signed := "https://b.s3.ap-south-1.amazonaws.com/hosts/government-ids/id.png?X-Amz-Algorithm=AWS4&X-Amz-Signature=abc"
	if got := *UnsignURL(&signed); got != "https://b.s3.ap-south-1.amazonaws.com/hosts/government-ids/id.png" {
		t.Fatalf("got %s", got)
	}
	plain := "https://x/a.png?v=2"
	if got := *UnsignURL(&plain); got != plain {
		t.Fatalf("non-signed query stripped: %s", got)
	}
	if UnsignURL(nil) != nil {
		t.Fatal("nil")
	}
}
