package notification

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestBuildMultipartEmailRoundTrip(t *testing.T) {
	pdf := bytes.Repeat([]byte("%PDF-1.4 fake body "), 40) // longer than one 76-char line
	raw, err := buildMultipartEmail("MySlotMate <noreply@example.com>", "guest@example.com",
		"Booking Confirmed - MySlotMate", "<p>Hi</p>",
		[]Attachment{{Filename: "MySlotMate-F-2627-0000001.pdf", ContentType: "application/pdf", Data: pdf}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("Subject"); got != "Booking Confirmed - MySlotMate" {
		t.Fatalf("subject %q", got)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("content type %q: %v", mt, err)
	}
	r := multipart.NewReader(msg.Body, params["boundary"])

	html, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(html); string(b) != "<p>Hi</p>" || !strings.HasPrefix(html.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("html part %q", b)
	}

	att, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if att.FileName() != "MySlotMate-F-2627-0000001.pdf" {
		t.Fatalf("filename %q", att.FileName())
	}
	enc, _ := io.ReadAll(att)
	for _, line := range strings.Split(strings.TrimSpace(string(enc)), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("base64 line of %d chars", len(line))
		}
	}
	got, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(enc), "\r\n", ""))
	if err != nil || !bytes.Equal(got, pdf) {
		t.Fatalf("attachment did not round-trip: %v", err)
	}
	if _, err := r.NextPart(); err != io.EOF {
		t.Fatalf("expected two parts, got more: %v", err)
	}
}
