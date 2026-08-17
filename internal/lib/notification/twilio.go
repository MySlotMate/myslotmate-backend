package notification

import (
	"context"
	"fmt"
	"log"
	"myslotmate-backend/internal/config"
	"myslotmate-backend/internal/lib/timeutil"
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
	SendAdminHostPendingAlert(ctx context.Context, hostName string, hostCity string, hostPhone string, adminPhoneNumbers []string) error
	// RSVP join requests — see the implementations for the delivery strategy.
	SendJoinRequestReceivedWhatsapp(ctx context.Context, hostPhone, hostName, guestName, eventTitle string) error
	SendJoinRequestApprovedWhatsapp(ctx context.Context, guestPhone, guestName, eventTitle string) error
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
		timeutil.FormatEventTime(event.Time),
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
		timeutil.FormatEventTime(event.Time),
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

	eventTimeStr := timeutil.FormatEventTime(event.Time)
	if !booking.OccurrenceDate.IsZero() {
		eventTimeStr = timeutil.FormatEventTime(booking.OccurrenceDate)
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
		timeutil.FormatEventTime(event.Time),
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

// SendAdminHostPendingAlert sends a Kapso WhatsApp alert message to configured admin phone numbers when a host submits a pending application
func (s *TwilioNotificationService) SendAdminHostPendingAlert(ctx context.Context, hostName string, hostCity string, hostPhone string, adminPhoneNumbers []string) error {
	if len(adminPhoneNumbers) == 0 {
		log.Println("[ADMIN_ALERT] No active admin phone numbers configured for host pending alert")
		return nil
	}

	message := fmt.Sprintf(
		"🚨 NEW HOST APPLICATION PENDING APPROVAL!\n\nHost Name: %s\nCity: %s\nPhone: %s\n\nPlease log in to the Admin Dashboard to review and approve/reject.",
		hostName,
		hostCity,
		hostPhone,
	)

	var lastErr error
	for _, adminPhone := range adminPhoneNumbers {
		adminPhone = strings.TrimSpace(adminPhone)
		if adminPhone == "" {
			continue
		}

		if s.kapsoClient != nil {
			log.Printf("[ADMIN_ALERT] Sending Kapso WhatsApp alert to %s for host %s...\n", adminPhone, hostName)
			err := s.kapsoClient.SendTextMessage(ctx, adminPhone, message)
			if err != nil && (strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "24-hour")) {
				log.Printf("[ADMIN_ALERT] 24-hour window restriction detected. Retrying using WhatsApp template message...\n")
				templateName := "host_application_alert"
				templateLang := "en_US"
				if s.kapsoCfg != nil {
					if s.kapsoCfg.HostAlertTemplateName != "" {
						templateName = s.kapsoCfg.HostAlertTemplateName
					}
					if s.kapsoCfg.HostAlertTemplateLang != "" {
						templateLang = s.kapsoCfg.HostAlertTemplateLang
					}
				}
				err = s.kapsoClient.SendHostPendingAlertTemplateMessage(ctx, adminPhone, templateName, templateLang, hostName, hostCity, hostPhone)
			}

			if err != nil {
				log.Printf("[ADMIN_ALERT] Failed to send Kapso WhatsApp alert to %s: %v\n", adminPhone, err)
				lastErr = err
			} else {
				log.Printf("[ADMIN_ALERT] Successfully sent Kapso WhatsApp alert to %s\n", adminPhone)
			}
		} else if s.cfg != nil && s.cfg.WhatsappNumber != "" {
			log.Printf("[ADMIN_ALERT] Sending Twilio WhatsApp alert to %s for host %s...\n", adminPhone, hostName)
			params := &twilioapiv2010.CreateMessageParams{}
			params.SetFrom("whatsapp:" + formatPhoneNumber(s.cfg.WhatsappNumber))
			params.SetTo("whatsapp:" + formatPhoneNumber(adminPhone))
			params.SetBody(message)

			if _, err := s.client.Api.CreateMessage(params); err != nil {
				log.Printf("[ADMIN_ALERT] Failed to send Twilio WhatsApp alert to %s: %v\n", adminPhone, err)
				lastErr = err
			} else {
				log.Printf("[ADMIN_ALERT] Successfully sent Twilio WhatsApp alert to %s\n", adminPhone)
			}
		} else {
			log.Printf("[ADMIN_ALERT] Warning: Neither Kapso nor Twilio WhatsApp service is configured!\n")
		}
	}

	return lastErr
}

// ── RSVP join requests ──────────────────────────────────────────────────────
//
// Two WhatsApp messages carry the RSVP flow: the host is told someone applied,
// and the guest is told the answer. Both follow the same delivery strategy as
// SendAdminHostPendingAlert — try a free-form text first (cheap, and allowed
// while a 24-hour session window is open), and fall back to a pre-approved
// template when WhatsApp rejects it for being outside that window.
//
// Both are best-effort by design: a request must still be recorded, and an
// approval must still stand, if WhatsApp is down or the template is missing.

// sendWhatsAppWithTemplateFallback is the shared delivery path for the two RSVP
// messages. `tag` only labels the logs.
//
// The template is tried FIRST, not as a fallback, because these are
// business-initiated messages: nobody has necessarily messaged us recently, so
// there is usually no open 24-hour session window and a plain text is rejected.
//
// Trying text first cannot work here, and the reason is worth spelling out:
// Kapso ACCEPTS the message (HTTP 200) and forwards it to Meta, which rejects
// it asynchronously with error 131047 "re-engagement message". Nothing about
// that failure is visible in the HTTP response, so a text-then-template retry
// never fires — the send looks successful and the message silently never
// arrives. A template works whether or not a window is open.
//
// Plain text remains as the fallback for the case the template itself is
// rejected synchronously (wrong name, not approved yet, parameter mismatch) —
// then an in-window recipient still gets something.
func (s *TwilioNotificationService) sendWhatsAppWithTemplateFallback(
	ctx context.Context,
	tag, to, message string,
	templateName, templateLang string,
	firstName, firstValue, secondName, secondValue string,
) error {
	to = strings.TrimSpace(to)
	if to == "" {
		log.Printf("[%s] No phone number on file — skipping WhatsApp\n", tag)
		return nil
	}

	if s.kapsoClient != nil {
		err := s.kapsoClient.SendTwoParamTemplateMessage(
			ctx, to, templateName, templateLang,
			firstName, firstValue, secondName, secondValue,
		)
		if err != nil {
			log.Printf("[%s] Template %q failed (%v) — falling back to plain text\n",
				tag, templateName, err)
			err = s.kapsoClient.SendTextMessage(ctx, to, message)
		}
		if err != nil {
			log.Printf("[%s] WhatsApp to %s failed: %v\n", tag, to, err)
			return err
		}
		log.Printf("[%s] WhatsApp sent to %s via template %q\n", tag, to, templateName)
		return nil
	}

	if s.cfg != nil && s.cfg.WhatsappNumber != "" {
		params := &twilioapiv2010.CreateMessageParams{}
		params.SetFrom("whatsapp:" + formatPhoneNumber(s.cfg.WhatsappNumber))
		params.SetTo("whatsapp:" + formatPhoneNumber(to))
		params.SetBody(message)
		if _, err := s.client.Api.CreateMessage(params); err != nil {
			log.Printf("[%s] Twilio WhatsApp to %s failed: %v\n", tag, to, err)
			return err
		}
		log.Printf("[%s] Twilio WhatsApp sent to %s\n", tag, to)
		return nil
	}

	log.Printf("[%s] Neither Kapso nor Twilio WhatsApp is configured\n", tag)
	return nil
}

// SendJoinRequestReceivedWhatsapp tells a host that a guest has asked to join
// one of their request-only experiences.
func (s *TwilioNotificationService) SendJoinRequestReceivedWhatsapp(
	ctx context.Context, hostPhone, hostName, guestName, eventTitle string,
) error {
	message := fmt.Sprintf(
		"👋 New request to join!\n\n%s has asked to join *%s*.\n\nOpen your MySlotMate dashboard → Requests to approve or decline.",
		guestName, eventTitle,
	)
	templateName, templateLang := "join_request_received", "en_US"
	if s.kapsoCfg != nil {
		if s.kapsoCfg.JoinRequestTemplateName != "" {
			templateName = s.kapsoCfg.JoinRequestTemplateName
		}
		if s.kapsoCfg.JoinRequestTemplateLang != "" {
			templateLang = s.kapsoCfg.JoinRequestTemplateLang
		}
	}
	return s.sendWhatsAppWithTemplateFallback(
		ctx, "JOIN_REQUEST", hostPhone, message, templateName, templateLang,
		"name", guestName, "event_name", eventTitle,
	)
}

// SendJoinRequestApprovedWhatsapp tells a guest their request was accepted.
//
// Deliberately explicit that approval is not a booking: the guest still has to
// come back and book, and nothing is held for them until they do.
func (s *TwilioNotificationService) SendJoinRequestApprovedWhatsapp(
	ctx context.Context, guestPhone, guestName, eventTitle string,
) error {
	message := fmt.Sprintf(
		"🎉 You're approved!\n\nHi %s, the host has accepted your request to join *%s*.\n\nHead back to MySlotMate to book your spot — it isn't held until you do.",
		guestName, eventTitle,
	)
	templateName, templateLang := "join_request_approved", "en_US"
	if s.kapsoCfg != nil {
		if s.kapsoCfg.JoinApprovedTemplateName != "" {
			templateName = s.kapsoCfg.JoinApprovedTemplateName
		}
		if s.kapsoCfg.JoinApprovedTemplateLang != "" {
			templateLang = s.kapsoCfg.JoinApprovedTemplateLang
		}
	}
	return s.sendWhatsAppWithTemplateFallback(
		ctx, "JOIN_APPROVED", guestPhone, message, templateName, templateLang,
		"name", guestName, "event_name", eventTitle,
	)
}
