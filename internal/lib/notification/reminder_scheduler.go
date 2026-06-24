package notification

import (
	"context"
	"fmt"
	"time"

	"myslotmate-backend/internal/repository"
)

// ReminderScheduler handles scheduling of event reminder notifications
type ReminderScheduler struct {
	bookingRepo  repository.BookingRepository
	eventRepo    repository.EventRepository
	userRepo     repository.UserRepository
	notifService NotificationService
	ticker       *time.Ticker
	stopChan     chan struct{}
	reminderMins int // Send reminder N minutes before event
}

// NewReminderScheduler creates a new reminder scheduler
// reminderMins: send reminder X minutes before event (typically 60-120 minutes)
func NewReminderScheduler(
	bookingRepo repository.BookingRepository,
	eventRepo repository.EventRepository,
	userRepo repository.UserRepository,
	notifService NotificationService,
	reminderMins int,
) *ReminderScheduler {
	return &ReminderScheduler{
		bookingRepo:  bookingRepo,
		eventRepo:    eventRepo,
		userRepo:     userRepo,
		notifService: notifService,
		reminderMins: reminderMins,
		stopChan:     make(chan struct{}),
	}
}

// Start begins the reminder scheduler, running every checkInterval
func (rs *ReminderScheduler) Start(checkInterval time.Duration) {
	rs.ticker = time.NewTicker(checkInterval)

	go func() {
		for {
			select {
			case <-rs.ticker.C:
				// Run the reminder check
				if err := rs.processReminders(); err != nil {
					fmt.Printf("[REMINDER] Error processing reminders: %v\n", err)
				}
			case <-rs.stopChan:
				fmt.Println("[REMINDER] Scheduler stopped")
				return
			}
		}
	}()

	fmt.Printf("[REMINDER] Scheduler started with %d minute offset\n", rs.reminderMins)
}

// Stop stops the reminder scheduler
func (rs *ReminderScheduler) Stop() {
	if rs.ticker != nil {
		rs.ticker.Stop()
	}
	close(rs.stopChan)
}

// processReminders checks for bookings that need reminder notifications
func (rs *ReminderScheduler) processReminders() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Get all pending reminder notifications (not yet sent)
	bookings, err := rs.bookingRepo.ListPendingReminderNotifications(ctx, 100) // Get up to 100 at a time
	if err != nil {
		return fmt.Errorf("failed to list pending reminders: %w", err)
	}

	if len(bookings) == 0 {
		return nil
	}

	now := time.Now()

	for _, booking := range bookings {
		// Fetch event details
		evt, err := rs.eventRepo.GetByID(ctx, booking.EventID)
		if err != nil || evt == nil {
			continue
		}

		// Calculate if it's time to send the reminder
		// Reminder window: [eventTime - reminderMins] to [eventTime]
		reminderTime := evt.Time.Add(-time.Duration(rs.reminderMins) * time.Minute)

		// If current time is before reminder time, skip
		if now.Before(reminderTime) {
			continue
		}

		// If the event has already started, skip both channels
		if now.After(evt.Time) {
			continue
		}

		// If both channel reminders have already been sent, nothing to do.
		// Each channel is gated independently below so that a failure on one
		// (e.g. user has no email) never causes the other to be re-sent.
		if booking.ReminderNotificationSentWhatsapp && booking.ReminderNotificationSentEmail {
			continue
		}

		// Fetch user details
		user, err := rs.userRepo.GetByID(ctx, booking.UserID)
		if err != nil || user == nil {
			continue
		}

		// Send the reminder notification
		fmt.Printf("[REMINDER] Sending reminder for booking %s (event %s at %s)\n",
			booking.ID, evt.Title, evt.Time.Format("2006-01-02 15:04:05"))

		// Send WhatsApp reminder only if not already sent. The send call marks
		// the booking's WhatsApp flag on success, so it won't be re-sent next tick.
		if !booking.ReminderNotificationSentWhatsapp {
			if err := rs.notifService.SendEventReminderWhatsapp(ctx, booking, user, evt); err != nil {
				fmt.Printf("[REMINDER] Error sending WhatsApp reminder for booking %s: %v\n",
					booking.ID, err)
				// Non-critical failure — independent of email below
			}
		}

		// Send Email reminder only if not already sent (independent of WhatsApp result)
		if !booking.ReminderNotificationSentEmail {
			if err := rs.notifService.SendEventReminderEmail(ctx, booking, user, evt); err != nil {
				fmt.Printf("[REMINDER] Error sending Email reminder for booking %s: %v\n",
					booking.ID, err)
				// Non-critical failure — continue to next booking
			}
		}
	}

	return nil
}

// SendReminderForBooking manually triggers a reminder for a specific booking
func (rs *ReminderScheduler) SendReminderForBooking(ctx context.Context, bookingID string) error {
	// This can be used for testing or manual triggers
	fmt.Printf("[REMINDER] Manual reminder trigger for booking %s\n", bookingID)
	// Implementation depends on how you want to structure the ID conversion
	return nil
}
