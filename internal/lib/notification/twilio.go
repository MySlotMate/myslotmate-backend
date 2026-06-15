package notification

import (
	"context"
	"fmt"
	"myslotmate-backend/internal/config"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"strings"

	"github.com/twilio/twilio-go"
	twilioapiv2010 "github.com/twilio/twilio-go/rest/api/v2010"
)

// TwilioNotificationService handles SMS and WhatsApp notifications via Twilio
type TwilioNotificationService struct {
	cfg         *config.TwilioConfig
	client      *twilio.RestClient
	bookingRepo repository.BookingRepository
	eventRepo   repository.EventRepository
	userRepo    repository.UserRepository
	emailSvc    *EmailService
	kapsoClient *KapsoClient
	kapsoCfg    *config.KapsoConfig
}

// formatPhoneNumber ensures phone number has country code (+91 for India)
// Input: "9876543210" or "09876543210"
// Output: "+919876543210"
func formatPhoneNumber(phoneNumber string) string {
	// Remove any spaces or special characters except digits and +
	phoneNumber = strings.TrimSpace(phoneNumber)

	// If already has +91, return as is
	if strings.HasPrefix(phoneNumber, "+91") {
		return phoneNumber
	}

	// If starts with 0, remove it (common Indian format)
	if strings.HasPrefix(phoneNumber, "0") {
		phoneNumber = phoneNumber[1:]
	}

	// If starts with +, just ensure it's formatted right (shouldn't happen for India but handle it)
	if strings.HasPrefix(phoneNumber, "+") {
		return phoneNumber
	}

	// Add +91 prefix for India
	return "+91" + phoneNumber
}

// NotificationService interface defines methods for sending notifications
type NotificationService interface {
	SendBookingConfirmationWhatsapp(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event) error
	SendBookingConfirmationWhatsappWithPDF(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event, fileName string, pdfBytes []byte) error
	SendBookingConfirmationEmail(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event) error
	SendEventReminderWhatsapp(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event) error
	SendEventReminderEmail(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event) error
	SendSMS(ctx context.Context, to string, body string) error
	GetKapsoClient() *KapsoClient
	SendCustomEmail(ctx context.Context, to string, subject string, htmlBody string) error
}

// NewTwilioNotificationService creates a new Twilio notification service
func NewTwilioNotificationService(
	cfg *config.TwilioConfig,
	emailCfg *config.SMTPConfig,
	kapsoCfg *config.KapsoConfig,
	bookingRepo repository.BookingRepository,
	eventRepo repository.EventRepository,
	userRepo repository.UserRepository,
) *TwilioNotificationService {
	client := twilio.NewRestClientWithParams(twilio.ClientParams{
		Username: cfg.AccountSID,
		Password: cfg.AuthToken,
	})

	var kapsoClient *KapsoClient
	if kapsoCfg != nil && kapsoCfg.APIKey != "" && kapsoCfg.PhoneNumberID != "" {
		kapsoClient = NewKapsoClient(kapsoCfg.APIKey, kapsoCfg.PhoneNumberID)
	}

	return &TwilioNotificationService{
		cfg:         cfg,
		client:      client,
		bookingRepo: bookingRepo,
		eventRepo:   eventRepo,
		userRepo:    userRepo,
		emailSvc:    NewEmailService(emailCfg),
		kapsoClient: kapsoClient,
		kapsoCfg:    kapsoCfg,
	}
}

// SendBookingConfirmationWhatsapp sends booking confirmation via WhatsApp
// Message includes event name, time, and booking details
func (s *TwilioNotificationService) SendBookingConfirmationWhatsapp(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event) error {
	if user.PhnNumber == "" {
		return fmt.Errorf("user phone not available")
	}

	// Format message with booking details
	message := fmt.Sprintf(
		"🎉 Booking Confirmed!\n\nEvent: %s\nTime: %s\nTickets: %d\n\nThank you for booking with MySlotMate!",
		event.Title,
		event.Time.Format("Jan 2, 2006 3:04 PM"),
		booking.Quantity,
	)

	if s.kapsoClient != nil {
		if err := s.kapsoClient.SendTextMessage(ctx, user.PhnNumber, message); err != nil {
			return fmt.Errorf("failed to send WhatsApp message via Kapso: %w", err)
		}
	} else {
		if s.cfg.WhatsappNumber == "" {
			return fmt.Errorf("WhatsApp number not configured")
		}
		// Send WhatsApp message via Twilio with formatted phone number
		params := &twilioapiv2010.CreateMessageParams{}
		params.SetFrom("whatsapp:" + formatPhoneNumber(s.cfg.WhatsappNumber))
		params.SetTo("whatsapp:" + formatPhoneNumber(user.PhnNumber))
		params.SetBody(message)

		_, err := s.client.Api.CreateMessage(params)
		if err != nil {
			return fmt.Errorf("failed to send WhatsApp message via Twilio: %w", err)
		}
	}

	// Mark notification as sent in database
	if err := s.bookingRepo.MarkWhatsappNotificationSent(ctx, booking.ID); err != nil {
		return fmt.Errorf("failed to mark WhatsApp notification as sent: %w", err)
	}

	return nil
}

// SendBookingConfirmationEmail sends booking confirmation via email
func (s *TwilioNotificationService) SendBookingConfirmationEmail(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event) error {
	if user.Email == "" {
		return fmt.Errorf("user email not available")
	}

	// Send email
	err := s.emailSvc.SendBookingConfirmationEmail(
		user.Email,
		user.Name,
		event.Title,
		event.Time.Format("Jan 2, 2006 3:04 PM"),
		fmt.Sprintf("%d", booking.Quantity),
	)
	if err != nil {
		return fmt.Errorf("failed to send confirmation email: %w", err)
	}

	// Mark notification as sent in database
	if err := s.bookingRepo.MarkEmailNotificationSent(ctx, booking.ID); err != nil {
		return fmt.Errorf("failed to mark email notification as sent: %w", err)
	}

	return nil
}

// SendEventReminderWhatsapp sends event reminder via WhatsApp
// Called 1-2 hours before event start
func (s *TwilioNotificationService) SendEventReminderWhatsapp(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event) error {
	if user.PhnNumber == "" {
		return fmt.Errorf("user phone not available")
	}

	eventTimeStr := event.Time.Format("Jan 2, 2006 3:04 PM")
	if !booking.OccurrenceDate.IsZero() {
		eventTimeStr = booking.OccurrenceDate.Format("Jan 2, 2006 3:04 PM")
	}

	if s.kapsoClient != nil {
		templateName := "event_reminder"
		if s.kapsoCfg != nil && s.kapsoCfg.ReminderTemplateName != "" {
			templateName = s.kapsoCfg.ReminderTemplateName
		}
		templateLang := "en_US"
		if s.kapsoCfg != nil && s.kapsoCfg.ReminderTemplateLang != "" {
			templateLang = s.kapsoCfg.ReminderTemplateLang
		}

		// Map parameters sequentially for the template: userName, eventTitle, eventTime
		components := []TemplateComponent{
			{
				Type: "body",
				Parameters: []TemplateParameter{
					{
						Type: "text",
						Text: user.Name,
					},
					{
						Type: "text",
						Text: event.Title,
					},
					{
						Type: "text",
						Text: eventTimeStr,
					},
				},
			},
		}

		if err := s.kapsoClient.SendTemplateMessage(ctx, user.PhnNumber, templateName, templateLang, components); err != nil {
			return fmt.Errorf("failed to send reminder WhatsApp via Kapso: %w", err)
		}
	} else {
		if s.cfg.WhatsappNumber == "" {
			return fmt.Errorf("WhatsApp number not configured")
		}

		message := fmt.Sprintf(
			"⏰ Event Starting Soon!\n\nEvent: %s\nTime: %s\n\nYour booking is confirmed. See you soon!",
			event.Title,
			eventTimeStr,
		)

		// Send WhatsApp reminder via Twilio with formatted phone number
		params := &twilioapiv2010.CreateMessageParams{}
		params.SetFrom("whatsapp:" + formatPhoneNumber(s.cfg.WhatsappNumber))
		params.SetTo("whatsapp:" + formatPhoneNumber(user.PhnNumber))
		params.SetBody(message)

		_, err := s.client.Api.CreateMessage(params)
		if err != nil {
			return fmt.Errorf("failed to send reminder WhatsApp via Twilio: %w", err)
		}
	}

	// Mark reminder notification as sent in database
	if err := s.bookingRepo.MarkWhatsappReminderNotificationSent(ctx, booking.ID); err != nil {
		return fmt.Errorf("failed to mark reminder WhatsApp as sent: %w", err)
	}

	return nil
}

// SendSMS sends a plain text SMS via Twilio
func (s *TwilioNotificationService) SendSMS(ctx context.Context, to string, body string) error {
	if s.cfg.PhoneNumber == "" {
		return fmt.Errorf("Twilio phone number not configured")
	}

	params := &twilioapiv2010.CreateMessageParams{}
	params.SetFrom(s.cfg.PhoneNumber)
	params.SetTo(formatPhoneNumber(to))
	params.SetBody(body)

	_, err := s.client.Api.CreateMessage(params)
	if err != nil {
		return fmt.Errorf("failed to send SMS: %w", err)
	}

	return nil
}

// SendEventReminderEmail sends event reminder via email
// Called 1-2 hours before event start
func (s *TwilioNotificationService) SendEventReminderEmail(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event) error {
	if user.Email == "" {
		return fmt.Errorf("user email not available")
	}

	// Send email reminder
	err := s.emailSvc.SendEventReminderEmail(
		user.Email,
		user.Name,
		event.Title,
		event.Time.Format("Jan 2, 2006 3:04 PM"),
	)
	if err != nil {
		return fmt.Errorf("failed to send reminder email: %w", err)
	}

	// Mark reminder notification as sent in database
	if err := s.bookingRepo.MarkEmailReminderNotificationSent(ctx, booking.ID); err != nil {
		return fmt.Errorf("failed to mark reminder email as sent: %w", err)
	}

	return nil
}

// SendReminderNotifications processes pending reminder notifications (called by scheduler)
func (s *TwilioNotificationService) SendReminderNotifications(ctx context.Context, limit int) error {
	// Get pending reminders
	bookings, err := s.bookingRepo.ListPendingReminderNotifications(ctx, limit)
	if err != nil {
		return fmt.Errorf("failed to fetch pending reminders: %w", err)
	}

	for _, booking := range bookings {
		// Fetch related data
		user, err := s.userRepo.GetByID(ctx, booking.UserID)
		if err != nil || user == nil {
			continue
		}

		event, err := s.eventRepo.GetByID(ctx, booking.EventID)
		if err != nil || event == nil {
			continue
		}

		// Send both WhatsApp and Email reminders
		_ = s.SendEventReminderWhatsapp(ctx, booking, user, event)
		_ = s.SendEventReminderEmail(ctx, booking, user, event)
	}

	return nil
}

// SendBookingConfirmationWhatsappWithPDF uploads the booking ticket PDF to Kapso and sends it to the user.
// If Kapso is not configured, it falls back to sending a text confirmation.
func (s *TwilioNotificationService) SendBookingConfirmationWhatsappWithPDF(ctx context.Context, booking *models.Booking, user *models.User, event *models.Event, fileName string, pdfBytes []byte) error {
	if s.kapsoClient == nil {
		return s.SendBookingConfirmationWhatsapp(ctx, booking, user, event)
	}

	if user.PhnNumber == "" {
		return fmt.Errorf("user phone not available")
	}

	mediaID, err := s.kapsoClient.UploadMedia(ctx, fileName, pdfBytes)
	if err != nil {
		return fmt.Errorf("failed to upload ticket PDF to Kapso: %w", err)
	}

	templateName := "ticket_confirmation"
	if s.kapsoCfg != nil && s.kapsoCfg.TicketTemplateName != "" {
		templateName = s.kapsoCfg.TicketTemplateName
	}
	templateLang := "en_US"
	if s.kapsoCfg != nil && s.kapsoCfg.TicketTemplateLang != "" {
		templateLang = s.kapsoCfg.TicketTemplateLang
	}

	err = s.kapsoClient.SendTicketTemplateMessage(
		ctx,
		user.PhnNumber,
		mediaID,
		fileName,
		templateName,
		templateLang,
		user.Name,
		event.Title,
	)
	if err != nil {
		return fmt.Errorf("failed to send ticket WhatsApp via Kapso: %w", err)
	}

	if err := s.bookingRepo.MarkWhatsappNotificationSent(ctx, booking.ID); err != nil {
		return fmt.Errorf("failed to mark WhatsApp notification as sent: %w", err)
	}

	return nil
}

// GetKapsoClient returns the initialized KapsoClient or nil if not configured.
func (s *TwilioNotificationService) GetKapsoClient() *KapsoClient {
	return s.kapsoClient
}

// SendCustomEmail sends a custom styled email to the user
func (s *TwilioNotificationService) SendCustomEmail(ctx context.Context, to string, subject string, htmlBody string) error {
	if s.emailSvc == nil {
		return fmt.Errorf("email service not initialized")
	}
	return s.emailSvc.SendEmail(to, subject, htmlBody)
}
