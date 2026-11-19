package controller

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"log"
	"strings"
	"time"

	"myslotmate-backend/internal/lib/notification"

	"github.com/google/uuid"
)

// GalleryNotifier tells an event's past guests that the host shared photos
// from it. At most one message per event per day (events.gallery_notified_at).
//
// ponytail: email only — Go has no push/in-app channel yet and WhatsApp needs a
// Meta-approved template. Swap the send call when one exists.
type GalleryNotifier struct {
	db      *sql.DB
	notif   notification.NotificationService
	siteURL string
}

func NewGalleryNotifier(db *sql.DB, notif notification.NotificationService, siteURL string) *GalleryNotifier {
	return &GalleryNotifier{db: db, notif: notif, siteURL: strings.TrimRight(siteURL, "/")}
}

// PhotosAdded runs in the background; failures are logged, never surfaced.
func (g *GalleryNotifier) PhotosAdded(eventID uuid.UUID, added int) {
	if g == nil || g.notif == nil || added <= 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := g.send(ctx, eventID, added); err != nil {
			log.Printf("[GALLERY_NOTIFY] event %s: %v", eventID, err)
		}
	}()
}

func (g *GalleryNotifier) send(ctx context.Context, eventID uuid.UUID, added int) error {
	// Claim today's slot atomically; no row back means already sent in the last day.
	var title, hostName string
	err := g.db.QueryRowContext(ctx, `UPDATE events e SET gallery_notified_at = now()
		FROM hosts h
		WHERE e.id = $1 AND h.id = e.host_id
		  AND (e.gallery_notified_at IS NULL OR e.gallery_notified_at < now() - interval '1 day')
		RETURNING e.title, trim(COALESCE(h.first_name, '') || ' ' || COALESCE(h.last_name, ''))`,
		eventID).Scan(&title, &hostName)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	rows, err := g.db.QueryContext(ctx, `SELECT DISTINCT u.email, u.name
		FROM bookings b JOIN users u ON u.id = b.user_id
		WHERE b.event_id = $1 AND b.status = 'confirmed' AND b.occurrence_date < now()
		  AND COALESCE(u.email, '') <> ''`, eventID)
	if err != nil {
		return err
	}
	defer rows.Close()

	photos := "photos"
	if added == 1 {
		photos = "photo"
	}
	subject := fmt.Sprintf("%s shared %d %s from %s", hostName, added, photos, title)
	link := g.siteURL + "/activities"
	for rows.Next() {
		var email, name string
		if err := rows.Scan(&email, &name); err != nil {
			return err
		}
		body := fmt.Sprintf(`<p>Hi %s,</p><p>%s shared %d %s from <b>%s</b>.</p><p><a href="%s">See them in My journey</a></p>`,
			html.EscapeString(name), html.EscapeString(hostName), added, photos, html.EscapeString(title), link)
		if err := g.notif.SendCustomEmail(ctx, email, subject, body); err != nil {
			log.Printf("[GALLERY_NOTIFY] email to guest of %s: %v", eventID, err)
		}
	}
	return rows.Err()
}
