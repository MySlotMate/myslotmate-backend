package notification

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"myslotmate-backend/internal/config"
	"net/smtp"
	"net/textproto"
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

// Attachment is a file sent with an email.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// SendEmailWithAttachments sends an HTML email with files attached
// (multipart/mixed). With no attachments it is the same as SendEmail.
func (s *EmailService) SendEmailWithAttachments(to, subject, htmlBody string, files ...Attachment) error {
	if len(files) == 0 {
		return s.SendEmail(to, subject, htmlBody)
	}
	if s.cfg.Host == "" || s.cfg.User == "" || s.cfg.Password == "" {
		return fmt.Errorf("SMTP configuration incomplete")
	}
	msg, err := buildMultipartEmail(fmt.Sprintf("%s <%s>", s.cfg.FromName, s.cfg.User), to, subject, htmlBody, files)
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	auth := smtp.PlainAuth("", s.cfg.User, s.cfg.Password, s.cfg.Host)
	if err := smtp.SendMail(addr, auth, s.cfg.User, []string{to}, msg); err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}
	return nil
}

// buildMultipartEmail writes the raw message: an HTML part, then each file
// base64-encoded in 76-character lines.
func buildMultipartEmail(from, to, subject, htmlBody string, files []Attachment) ([]byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fmt.Fprintf(&buf, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n",
		from, to, mime.QEncoding.Encode("utf-8", subject), w.Boundary())

	h := textproto.MIMEHeader{}
	h.Set("Content-Type", `text/html; charset="UTF-8"`)
	h.Set("Content-Transfer-Encoding", "8bit")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write([]byte(htmlBody)); err != nil {
		return nil, err
	}

	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", fmt.Sprintf("%s; name=%q", f.ContentType, f.Filename))
		h.Set("Content-Transfer-Encoding", "base64")
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Filename))
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(f.Data)
		for len(enc) > 76 {
			if _, err := part.Write([]byte(enc[:76] + "\r\n")); err != nil {
				return nil, err
			}
			enc = enc[76:]
		}
		if _, err := part.Write([]byte(enc + "\r\n")); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SendBookingConfirmationEmail sends the booking confirmation. Files (the
// platform fee receipt) are attached when given.
func (s *EmailService) SendBookingConfirmationEmail(to, userName, eventTitle, eventTime, quantity string, files ...Attachment) error {
	subject := "Booking Confirmed - MySlotMate"
	receipt := ""
	if len(files) > 0 {
		receipt = `<p>Your platform fee receipt is attached as a PDF. You can also download it any time from My Journey.</p>`
	}
	name, title := html.EscapeString(userName), html.EscapeString(eventTitle)
	htmlBody := fmt.Sprintf(`
<html>
<body style="font-family: Arial, sans-serif; line-height: 1.6; color: #333;">
	<h2>🎉 Booking Confirmed!</h2>
	<p>Hi %s,</p>
	<p>Your booking for <strong>%s</strong> has been confirmed!</p>
	<div style="background-color: #f5f5f5; padding: 15px; border-radius: 5px; margin: 20px 0;">
		<p><strong>Event:</strong> %s</p>
		<p><strong>Date & Time:</strong> %s</p>
		<p><strong>Tickets:</strong> %s</p>
	</div>
	%s
	<p>Thank you for using MySlotMate!</p>
	<p>If you have any questions, please contact our support team.</p>
	<hr style="margin: 30px 0;">
	<p style="font-size: 12px; color: #666;">MySlotMate - Event Management Made Easy</p>
</body>
</html>
`, name, title, title, html.EscapeString(eventTime), html.EscapeString(quantity), receipt)

	return s.SendEmailWithAttachments(to, subject, htmlBody, files...)
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
