package service

import (
	"context"
	"fmt"

	"myslotmate-backend/internal/lib/notification"
	"myslotmate-backend/internal/models"
)

// joinRequestNotifier carries the two RSVP messages: the host hears that
// someone applied, and the guest hears the answer.
//
// Both matter more than a usual notification. Approval only UNLOCKS booking, so
// a guest who is never told simply never comes back and the seat goes unsold;
// and a host who never hears about a request leaves it sitting in a queue they
// have no reason to open.
//
// WhatsApp is the primary channel (that is where these users actually are) and
// the guest also gets an email on a decision. Every send is best-effort and
// deliberately swallows errors — a messaging outage must not roll back a
// decision the host already made, nor reject a request that is already stored.
type joinRequestNotifier struct {
	notif notification.NotificationService
}

// NewJoinRequestNotifier returns nil when no notification service is
// configured, which the service treats as "don't notify".
func NewJoinRequestNotifier(n notification.NotificationService) JoinRequestNotifier {
	if n == nil {
		return nil
	}
	return &joinRequestNotifier{notif: n}
}

// NotifyJoinRequestReceived pings the host on WhatsApp that a guest applied.
func (e *joinRequestNotifier) NotifyJoinRequestReceived(
	ctx context.Context, req *models.EventJoinRequest, hostName, hostPhone string,
) {
	if req == nil || hostPhone == "" {
		return
	}
	guest := req.UserName
	if guest == "" {
		guest = "A guest"
	}
	if err := e.notif.SendJoinRequestReceivedWhatsapp(
		ctx, hostPhone, hostName, guest, req.EventTitle,
	); err != nil {
		fmt.Printf("[JOIN_REQUEST] host WhatsApp to %s failed: %v\n", hostPhone, err)
	}
}

func (e *joinRequestNotifier) NotifyJoinRequestDecided(
	ctx context.Context, req *models.EventJoinRequest, approved bool,
) {
	if req == nil || req.UserEmail == "" {
		return
	}

	name := req.UserName
	if name == "" {
		name = "there"
	}

	// WhatsApp on approval only. A decline is softer by email — a rejection
	// pushed to someone's WhatsApp reads harshly, and there is nothing for them
	// to act on.
	if approved && req.UserPhone != "" {
		if err := e.notif.SendJoinRequestApprovedWhatsapp(
			ctx, req.UserPhone, name, req.EventTitle,
		); err != nil {
			fmt.Printf("[JOIN_REQUEST] guest WhatsApp to %s failed: %v\n", req.UserPhone, err)
		}
	}

	var subject, body string
	if approved {
		subject = fmt.Sprintf("You're in — %s", req.EventTitle)
		body = fmt.Sprintf(`
			<p>Hi %s,</p>
			<p>Your request to join <strong>%s</strong> has been approved.</p>
			<p>You can now book your spot. Head back to the experience page to
			   confirm — your place isn't held until you book.</p>
		`, name, req.EventTitle)
	} else {
		subject = fmt.Sprintf("About your request to join %s", req.EventTitle)
		body = fmt.Sprintf(`
			<p>Hi %s,</p>
			<p>Thanks for your interest in <strong>%s</strong>. The host wasn't
			   able to accept your request this time.</p>
		`, name, req.EventTitle)
	}
	if req.ReviewNote != nil && *req.ReviewNote != "" {
		body += fmt.Sprintf("<p><em>A note from the host: %s</em></p>", *req.ReviewNote)
	}

	if err := e.notif.SendCustomEmail(ctx, req.UserEmail, subject, body); err != nil {
		// Logged, never returned: the decision is already committed.
		fmt.Printf("[JOIN_REQUEST] email to %s failed: %v\n", req.UserEmail, err)
	}
}
