package service

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"

	"myslotmate-backend/internal/lib/timeutil"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

// Co-hosting — one host shares ONE event with another host.
//
// Sharing has two effects: the co-host co-manages the event (that half is
// enforced in EventRepository.HostCanManage, which every ownership check routes
// through), and — only while the owner leaves the withdraw toggle on — the
// co-host may pull that event's passed earnings to their own verified payout
// method.
//
// The money never changes hands early: booking earnings stay on the OWNER's
// account and ledger. A co-host withdrawal is an ordinary owner withdrawal with
// the payout destination redirected, capped at the event's own passed earnings,
// and recorded as a claim so the same earnings cannot go out twice.

// HostCanManageEvent is the one authorization question every host-side event
// action asks: is this host the owner, or an accepted co-host? Replaces the
// bare `evt.HostID != hostID` comparisons so co-hosting is enforced in one
// place. Fails closed — a DB error reads as "not allowed".
//
// Deliberate exception: deleting an event stays owner-only.
func HostCanManageEvent(ctx context.Context, eventRepo repository.EventRepository, evt *models.Event, hostID uuid.UUID) bool {
	if evt == nil {
		return false
	}
	if evt.HostID == hostID {
		return true
	}
	ok, err := eventRepo.HostCanManage(ctx, evt.ID, hostID)
	return err == nil && ok
}

// ownedEventCoHost loads a co-host row and proves the caller owns its event.
func (s *payoutService) ownedEventCoHost(ctx context.Context, ownerHostID, cohostID uuid.UUID) (*models.EventCoHost, error) {
	ch, err := s.cohostRepo.GetByID(ctx, cohostID)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return nil, errors.New("co-host not found")
	}
	evt, err := s.eventRepo.GetByID(ctx, ch.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil || evt.HostID != ownerHostID {
		return nil, errors.New("only the event owner can manage its co-hosts")
	}
	return ch, nil
}

func (s *payoutService) InviteCoHost(ctx context.Context, ownerHostID, eventID uuid.UUID, email string, canWithdraw bool) (*models.EventCoHost, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, errors.New("email is required")
	}

	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil || evt.HostID != ownerHostID {
		return nil, errors.New("only the event owner can invite a co-host")
	}

	// The invite resolves to an existing host account or it fails — no
	// pending-by-email state to reconcile later.
	user, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, errors.New("no MySlotMate account for that email; ask them to sign up and become a host first")
	}
	invitee, err := s.hostRepo.GetByUserID(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if invitee == nil {
		return nil, errors.New("that user is not a host yet; ask them to complete host onboarding first")
	}
	if invitee.ID == ownerHostID {
		return nil, errors.New("you already own this event")
	}

	existing, err := s.cohostRepo.GetByEventAndHost(ctx, eventID, invitee.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("that host is already a co-host of this event (%s)", existing.Status)
	}

	ch := &models.EventCoHost{
		ID:              uuid.New(),
		EventID:         eventID,
		HostID:          invitee.ID,
		InvitedByHostID: ownerHostID,
		Status:          models.CoHostPending,
		// Granted at invite time or later via SetCoHostCanWithdraw — either way it
		// only matters once the invitation is accepted.
		CanWithdraw: canWithdraw,
	}
	if err := s.cohostRepo.Create(ctx, ch); err != nil {
		return nil, err
	}

	// Tell them. Best-effort: a mail failure must not undo a stored invitation —
	// the invitee can still find it in their dashboard, and the owner can resend.
	owner, _ := s.hostRepo.GetByID(ctx, ownerHostID)
	s.sendCoHostInviteEmail(ctx, ch, evt, owner, user)
	return ch, nil
}

// ResendCoHostInvite re-sends the invitation email for a still-pending invite.
func (s *payoutService) ResendCoHostInvite(ctx context.Context, ownerHostID, cohostID uuid.UUID) error {
	ch, err := s.ownedEventCoHost(ctx, ownerHostID, cohostID)
	if err != nil {
		return err
	}
	if ch.Status != models.CoHostPending {
		return fmt.Errorf("invitation is already %s", ch.Status)
	}
	evt, err := s.eventRepo.GetByID(ctx, ch.EventID)
	if err != nil {
		return err
	}
	invitee, err := s.hostRepo.GetByID(ctx, ch.HostID)
	if err != nil {
		return err
	}
	if invitee == nil {
		return errors.New("invited host no longer exists")
	}
	user, err := s.userRepo.GetByID(ctx, invitee.UserID)
	if err != nil {
		return err
	}
	if user == nil || user.Email == "" {
		return errors.New("that host has no email address on file")
	}
	owner, _ := s.hostRepo.GetByID(ctx, ownerHostID)
	if err := s.emailCoHostInvite(ctx, ch, evt, owner, user); err != nil {
		return fmt.Errorf("could not send the invitation email: %w", err)
	}
	return nil
}

// sendCoHostInviteEmail is the fire-and-forget wrapper used on the invite path.
func (s *payoutService) sendCoHostInviteEmail(ctx context.Context, ch *models.EventCoHost, evt *models.Event, owner *models.Host, user *models.User) {
	if err := s.emailCoHostInvite(ctx, ch, evt, owner, user); err != nil {
		fmt.Printf("[COHOST] invite email to %s failed: %v\n", user.Email, err)
	}
}

func (s *payoutService) emailCoHostInvite(ctx context.Context, ch *models.EventCoHost, evt *models.Event, owner *models.Host, user *models.User) error {
	if s.notif == nil {
		return errors.New("email is not configured on this server")
	}
	if user == nil || user.Email == "" {
		return errors.New("no email address on file")
	}

	ownerName := "A host"
	if owner != nil {
		if n := strings.TrimSpace(owner.FirstName + " " + owner.LastName); n != "" {
			ownerName = n
		}
	}
	title := "an experience"
	when := ""
	if evt != nil {
		title = evt.Title
		when = timeutil.FormatEventTime(evt.Time)
	}

	access := "They have <strong>not</strong> given you access to this experience's earnings. " +
		"They can turn that on later."
	if ch.CanWithdraw {
		access = "They have also given you access to <strong>withdraw this experience's earnings</strong> " +
			"to your own bank account, once a session has happened."
	}

	link := s.frontendBaseURL + "/host-dashboard/co-hosting"
	body := fmt.Sprintf(`
<p>Hi %s,</p>
<p><strong>%s</strong> invited you to co-host <strong>%s</strong>%s.</p>
<p>As a co-host you can manage the experience with them — edit it, see who booked, and check guests in. %s</p>
<p><a href="%s">Open your dashboard to accept or decline</a></p>
<p style="color:#666;font-size:12px">You are seeing this because someone entered this email address as a co-host on MySlotMate.</p>`,
		htmlEscape(firstNonEmpty(user.Name, "there")),
		htmlEscape(ownerName),
		htmlEscape(title),
		htmlWhen(when),
		access,
		link,
	)

	subject := fmt.Sprintf("%s invited you to co-host %s", ownerName, title)
	return s.notif.SendCustomEmail(ctx, user.Email, subject, body)
}

func htmlWhen(when string) string {
	if when == "" {
		return ""
	}
	return " (" + htmlEscape(when) + ")"
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// htmlEscape keeps host-supplied text (names, titles) from breaking the mail.
func htmlEscape(s string) string { return html.EscapeString(s) }

func (s *payoutService) ListEventCoHosts(ctx context.Context, ownerHostID, eventID uuid.UUID) ([]*repository.CoHostRow, error) {
	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	// A co-host sees the roster too — they co-manage the event.
	if evt == nil {
		return nil, errors.New("event not found")
	}
	if evt.HostID != ownerHostID {
		ch, err := s.cohostRepo.GetByEventAndHost(ctx, eventID, ownerHostID)
		if err != nil {
			return nil, err
		}
		if ch == nil || ch.Status != models.CoHostAccepted {
			return nil, errors.New("not your event")
		}
	}
	return s.cohostRepo.ListByEvent(ctx, eventID)
}

func (s *payoutService) SetCoHostCanWithdraw(ctx context.Context, ownerHostID, cohostID uuid.UUID, canWithdraw bool) error {
	ch, err := s.ownedEventCoHost(ctx, ownerHostID, cohostID)
	if err != nil {
		return err
	}
	// Turning the toggle off only gates FUTURE withdrawals; a payout already in
	// flight or completed is untouched by design.
	return s.cohostRepo.SetCanWithdraw(ctx, ch.ID, canWithdraw)
}

func (s *payoutService) RevokeCoHost(ctx context.Context, ownerHostID, cohostID uuid.UUID) error {
	ch, err := s.ownedEventCoHost(ctx, ownerHostID, cohostID)
	if err != nil {
		return err
	}
	return s.cohostRepo.SetStatus(ctx, ch.ID, models.CoHostRevoked)
}

func (s *payoutService) RespondToCoHostInvite(ctx context.Context, hostID, cohostID uuid.UUID, accept bool) error {
	ch, err := s.cohostRepo.GetByID(ctx, cohostID)
	if err != nil {
		return err
	}
	if ch == nil || ch.HostID != hostID {
		return errors.New("invitation not found")
	}
	if ch.Status != models.CoHostPending {
		return fmt.Errorf("invitation is already %s", ch.Status)
	}
	if accept {
		return s.cohostRepo.SetStatus(ctx, ch.ID, models.CoHostAccepted)
	}
	return s.cohostRepo.SetStatus(ctx, ch.ID, models.CoHostDeclined)
}

func (s *payoutService) CoHostSummary(ctx context.Context, hostID uuid.UUID) (*CoHostSummary, error) {
	received, granted, err := s.cohostRepo.CountsForHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	return &CoHostSummary{Received: received, Granted: granted}, nil
}

func (s *payoutService) ListSharedEvents(ctx context.Context, hostID uuid.UUID) ([]*repository.SharedEventRow, error) {
	return s.cohostRepo.ListSharedWithHost(ctx, hostID)
}

// RequestCoHostWithdrawal pays an event's passed earnings to the co-host.
//
// Order matters: the claim is RESERVED first, because the reservation is the
// atomic affordability check (see CoHostRepository.ReserveClaim). Only then is
// the normal withdrawal run — with EventID set (caps it at this event) and
// PayoutHostID set (redirects the destination). If the payout never starts, or
// fails synchronously, the reservation is released so the money is claimable
// again.
func (s *payoutService) RequestCoHostWithdrawal(ctx context.Context, cohostHostID, cohostID uuid.UUID, req WithdrawalRequest) (*models.Payment, error) {
	ch, err := s.cohostRepo.GetByID(ctx, cohostID)
	if err != nil {
		return nil, err
	}
	if ch == nil || ch.HostID != cohostHostID {
		return nil, errors.New("shared event not found")
	}
	switch ch.Status {
	case models.CoHostAccepted:
		// proceed
	case models.CoHostPending:
		return nil, errors.New("accept the co-host invitation first")
	default:
		return nil, fmt.Errorf("you are no longer a co-host of this event (%s)", ch.Status)
	}
	if !ch.CanWithdraw {
		return nil, errors.New("the event owner has not granted you withdrawal access")
	}

	evt, err := s.eventRepo.GetByID(ctx, ch.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errors.New("event not found")
	}

	// amount_cents = 0 means "whatever is left on this event".
	amount := req.AmountCents
	if amount <= 0 {
		passed, err := s.cohostRepo.EventPassedEarnings(ctx, ch.EventID)
		if err != nil {
			return nil, err
		}
		claimed, err := s.cohostRepo.EventClaimedCents(ctx, ch.EventID)
		if err != nil {
			return nil, err
		}
		amount = passed - claimed
	}
	if amount <= 0 {
		return nil, errors.New("nothing left to withdraw on this event")
	}

	claimID, err := s.cohostRepo.ReserveClaim(ctx, ch.ID, ch.EventID, cohostHostID, amount)
	if err != nil {
		return nil, err
	}
	if claimID == uuid.Nil {
		return nil, errors.New("this event's earnings are already withdrawn (or the amount exceeds what is left)")
	}

	payment, err := s.RequestWithdrawal(ctx, evt.HostID, WithdrawalRequest{
		AmountCents:    amount,
		PayoutMethodID: req.PayoutMethodID,
		IdempotencyKey: req.IdempotencyKey,
		EventID:        &ch.EventID,
		PayoutHostID:   &cohostHostID,
	})
	if err != nil {
		// Nothing left the platform — release the slice.
		_ = s.cohostRepo.SetClaimStatus(ctx, claimID, models.CoHostClaimFailed)
		return nil, err
	}

	_ = s.cohostRepo.AttachPayment(ctx, claimID, payment.ID)
	switch payment.Status {
	case models.PaymentStatusFailed, models.PaymentStatusReversed:
		_ = s.cohostRepo.SetClaimStatus(ctx, claimID, models.CoHostClaimFailed)
	case models.PaymentStatusCompleted:
		_ = s.cohostRepo.SetClaimStatus(ctx, claimID, models.CoHostClaimCompleted)
	}
	// "processing" keeps the claim reserved until the payout webhook mirrors the
	// terminal state onto it (SetClaimStatusByPayment).
	return payment, nil
}
