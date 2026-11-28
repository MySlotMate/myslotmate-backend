package service

import (
	"strings"
	"testing"
	"time"

	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

func TestActivityItems(t *testing.T) {
	b, e := uuid.New(), uuid.New()
	at := time.Now()
	row := func(id int64, action, details string) repository.ActivityRow {
		return repository.ActivityRow{ID: id, Source: "boost", BoostID: &b, EventID: &e, EventTitle: "Jam", ActorKind: "admin",
			Action: action, Details: []byte(details), CreatedAt: at.Add(time.Duration(id) * time.Minute)}
	}
	// Newest first, as the repository returns them.
	rows := []repository.ActivityRow{
		row(9, "ended", `{"reason":"host_stopped"}`),
		row(8, "payment_paid", `{"total_cents":177000,"orphan":true}`),
		row(7, "payment_paid", `{"total_cents":177000}`),
		row(6, "spend_logged", `{}`),
		row(5, "proposed", `{}`),
		row(4, "declined", `{"reason":"Not enough seats."}`),
		row(3, "proposed", `{}`),
		{ID: 2, Source: "premium", ActorKind: "admin", Action: "grant", Details: []byte(`{}`), CreatedAt: at},
	}
	items := activityItems(rows)
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	got := strings.Join(titles, " | ")
	want := "Boost payment received | Boost proposal revised | Boost request declined | Boost proposal ready | You're a Premium host"
	if got != want {
		t.Fatalf("titles:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(items[0].Body, "₹1770") || items[0].Href != "/host-dashboard/experiences/"+e.String()+"?boost=1" {
		t.Fatalf("payment item = %+v", items[0])
	}
	if !strings.Contains(items[2].Body, "Not enough seats.") || items[4].Href != "/host-dashboard/premium" {
		t.Fatalf("decline / premium items = %+v / %+v", items[2], items[4])
	}
}
