package notification

import (
	"database/sql"
	"myslotmate-backend/internal/config"
	"myslotmate-backend/internal/repository"
)

// InitializeNotificationService creates and returns a fully configured notification service
func InitializeNotificationService(
	twilioConfig *config.TwilioConfig,
	smtpConfig *config.SMTPConfig,
	kapsoConfig *config.KapsoConfig,
	db *sql.DB,
) (NotificationService, error) {
	// Create repositories
	bookingRepo := repository.NewBookingRepository(db)
	eventRepo := repository.NewEventRepository(db)
	userRepo := repository.NewUserRepository(db)

	// Create and return the Twilio notification service
	return NewTwilioNotificationService(
		twilioConfig,
		smtpConfig,
		kapsoConfig,
		bookingRepo,
		eventRepo,
		userRepo,
	), nil
}

// InitializeReminderScheduler creates and returns a configured reminder scheduler
// checkInterval: how often to check for pending reminders (typically 1-5 minutes)
// reminderMinsBefore: how many minutes before event to send reminder (typically 60-120)
func InitializeReminderScheduler(
	db *sql.DB,
	notifService NotificationService,
	checkInterval int, // in minutes
	reminderMinsBefore int, // in minutes
) *ReminderScheduler {
	// Create repositories
	bookingRepo := repository.NewBookingRepository(db)
	eventRepo := repository.NewEventRepository(db)
	userRepo := repository.NewUserRepository(db)

	// Create scheduler
	scheduler := NewReminderScheduler(
		bookingRepo,
		eventRepo,
		userRepo,
		notifService,
		reminderMinsBefore,
	)

	return scheduler
}
