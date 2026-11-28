package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// The meeting link is for booked guests only (GET /events/{id}/join), so it
// must never travel with an event, whatever endpoint serialises it.
func TestEventJSONOmitsMeetingLink(t *testing.T) {
	link := "https://meet.example.com/secret-room"
	b, err := json.Marshal(&Event{IsOnline: true, MeetingLink: &link})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-room") || strings.Contains(string(b), "meeting_link") {
		t.Fatalf("event JSON leaks the meeting link: %s", b)
	}
}
