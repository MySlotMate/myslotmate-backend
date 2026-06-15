package notification

import (
	"fmt"
	"myslotmate-backend/internal/config"
	"net/smtp"
)

// EmailService handles sending emails via SMTP
type EmailService struct {
	cfg *config.SMTPConfig
}

// NewEmailService creates a new email service
func NewEmailService(cfg *config.SMTPConfig) *EmailService {
	return &EmailService{
		cfg: cfg,
	}
}

// SendEmail sends an email to the specified recipient
func (s *EmailService) SendEmail(to, subject, htmlBody string) error {
	if s.cfg.Host == "" || s.cfg.User == "" || s.cfg.Password == "" {
		return fmt.Errorf("SMTP configuration incomplete")
	}

	// Create email content
	header := fmt.Sprintf("From: %s <%s>\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=\"UTF-8\"\r\n\r\n",
		s.cfg.FromName, s.cfg.User, to, subject)

	body := header + htmlBody

	// SMTP server address
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)

	// Send email
	auth := smtp.PlainAuth("", s.cfg.User, s.cfg.Password, s.cfg.Host)
	err := smtp.SendMail(addr, auth, s.cfg.User, []string{to}, []byte(body))
	if err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}

	return nil
}

// SendBookingConfirmationEmail sends booking confirmation email
func (s *EmailService) SendBookingConfirmationEmail(to, userName, eventTitle, eventTime, quantity string) error {
	subject := "Booking Confirmed - MySlotMate"
	htmlBody := fmt.Sprintf(`
<html>
<body style="font-family: Arial, sans-serif; line-height: 1.6; color: #333;">
	<h2>🎉 Booking Confirmed!</h2>
	<p>Hi %s,</p>
	<p>Your booking for <strong>%s</strong> has been confirmed!</p>
	<div style="background-color: #f5f5f5; padding: 15px; border-radius: 5px; margin: 20px 0;">
		<p><strong>Event:</strong> %s</p>
		<p><strong>Date & Time:</strong> %s</p>
		<p><strong>Tickets:</strong> %d</p>
	</div>
	<p>Thank you for using MySlotMate!</p>
	<p>If you have any questions, please contact our support team.</p>
	<hr style="margin: 30px 0;">
	<p style="font-size: 12px; color: #666;">MySlotMate - Event Management Made Easy</p>
</body>
</html>
`, userName, eventTitle, eventTitle, eventTime, len(quantity))

	return s.SendEmail(to, subject, htmlBody)
}

// SendEventReminderEmail sends event reminder email
func (s *EmailService) SendEventReminderEmail(to, userName, eventTitle, eventTime string) error {
	subject := "Upcoming Event Reminder - MySlotMate"
	htmlBody := fmt.Sprintf(`
<html>
<body style="font-family: Arial, sans-serif; line-height: 1.6; color: #333;">
	<h2>📅 Upcoming Event Reminder</h2>
	<p>Hi %s,</p>
	<p>This is a reminder that your event "<strong>%s</strong>" is scheduled for %s.</p>
	<p>Please review your event details and ensure all preparations are in place for a smooth experience.</p>
	<p>We wish you a successful event! 🎉</p>
	<hr style="margin: 30px 0;">
	<p style="font-size: 12px; color: #666;">MySlotMate - Event Management Made Easy</p>
</body>
</html>
`, userName, eventTitle, eventTime)

	return s.SendEmail(to, subject, htmlBody)
}
