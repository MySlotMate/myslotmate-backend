package service

import (
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

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
	if !emailPattern.MatchString(email) {
		return nil, errors.New("that doesn't look like an email address")
	}

	evt, err := s.eventRepo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil || evt.HostID != ownerHostID {
		return nil, errors.New("only the event owner can invite a co-host")
	}

	owner, _ := s.hostRepo.GetByID(ctx, ownerHostID)
	if ownerEmail := s.hostEmail(ctx, owner); ownerEmail != "" && ownerEmail == email {
		return nil, errors.New("that's your own email; enter the email of the host you want to invite")
	}

	user, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	var invitee *models.Host
	if user != nil {
		if invitee, err = s.hostRepo.GetByUserID(ctx, user.ID); err != nil {
			return nil, err
		}
	}
	if invitee == nil {
		// Not on MySlotMate yet, or not a host yet: the invite waits for that
		// email and becomes a normal invitation once they are a host
		// (AttachEmailInvites, run when their dashboard loads).
		inv := &repository.CoHostEmailInvite{EventID: eventID, Email: email, InvitedByHostID: ownerHostID, CanWithdraw: canWithdraw}
		if err := s.cohostRepo.CreateEmailInvite(ctx, inv); err != nil {
			if errors.Is(err, repository.ErrEmailInviteOpen) {
				return nil, errors.New("you've already invited that email to this event")
			}
			return nil, err
		}
		s.inBackground("signup invite to "+email, func(ctx context.Context) error {
			return s.emailCoHostSignupInvite(ctx, inv, evt, owner, user != nil)
		})
		return &models.EventCoHost{ID: inv.ID, EventID: eventID, InvitedByHostID: ownerHostID,
			Status: models.CoHostAwaitingSignup, CanWithdraw: canWithdraw}, nil
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

	// Tell them, in the app and by email. Both best-effort and off the request:
	// a slow mail server must not hold up (or undo) a stored invitation — the
	// invitee still finds it in their dashboard, and the owner can resend.
	s.notifyCoHostInvite(ctx, user.ID, owner, evt)
	s.inBackground("invite email to "+user.Email, func(ctx context.Context) error {
		return s.emailCoHostInvite(ctx, ch, evt, owner, user)
	})
	return ch, nil
}

// inBackground runs a best-effort job (an email) off the request, with its own
// time limit, and logs a failure. The caller's context ends with the request.
func (s *payoutService) inBackground(what string, job func(ctx context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := job(ctx); err != nil {
			fmt.Printf("[COHOST] %s failed: %v\n", what, err)
		}
	}()
}

// hostEmail is the host's sign-in email, lower-cased ("" when unknown).
func (s *payoutService) hostEmail(ctx context.Context, h *models.Host) string {
	if h == nil {
		return ""
	}
	u, err := s.userRepo.GetByID(ctx, h.UserID)
	if err != nil || u == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(u.Email))
}

func hostDisplayName(h *models.Host) string {
	if h != nil {
		if n := strings.TrimSpace(h.FirstName + " " + h.LastName); n != "" {
			return n
		}
	}
	return "A host"
}

// notifyCoHostInvite puts the invitation in the invitee's in-app inbox.
func (s *payoutService) notifyCoHostInvite(ctx context.Context, userID uuid.UUID, owner *models.Host, evt *models.Event) {
	title := "an experience"
	if evt != nil {
		title = evt.Title
	}
	if err := s.cohostRepo.Notify(ctx, userID, "Co-host invitation",
		fmt.Sprintf("%s invited you to co-host %s. Accept or decline it under Co-hosts in your host dashboard.", hostDisplayName(owner), title), nil); err != nil {
		fmt.Printf("[COHOST] in-app notice failed: %v\n", err)
	}
}

var emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// ResendCoHostInvite re-sends the invitation email for a still-pending invite.
func (s *payoutService) ResendCoHostInvite(ctx context.Context, ownerHostID, cohostID uuid.UUID) error {
	if s.notif == nil {
		return errors.New("email is not configured on this server")
	}
	if inv, err := s.ownedEmailInvite(ctx, ownerHostID, cohostID); err != nil {
		return err
	} else if inv != nil {
		if inv.Status != "pending" {
			return fmt.Errorf("invitation is already %s", inv.Status)
		}
		evt, err := s.eventRepo.GetByID(ctx, inv.EventID)
		if err != nil {
			return err
		}
		owner, _ := s.hostRepo.GetByID(ctx, ownerHostID)
		user, _ := s.userRepo.GetByEmail(ctx, inv.Email)
		s.inBackground("signup invite to "+inv.Email, func(ctx context.Context) error {
			return s.emailCoHostSignupInvite(ctx, inv, evt, owner, user != nil)
		})
		return nil
	}
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
	s.inBackground("invite email to "+user.Email, func(ctx context.Context) error {
		return s.emailCoHostInvite(ctx, ch, evt, owner, user)
	})
	return nil
}

// ownedEmailInvite loads an email invite by id and proves the caller owns its
// event. (nil, nil) when the id is not an email invite.
func (s *payoutService) ownedEmailInvite(ctx context.Context, ownerHostID, id uuid.UUID) (*repository.CoHostEmailInvite, error) {
	inv, err := s.cohostRepo.GetEmailInvite(ctx, id)
	if err != nil || inv == nil {
		return nil, err
	}
	evt, err := s.eventRepo.GetByID(ctx, inv.EventID)
	if err != nil {
		return nil, err
	}
	if evt == nil || evt.HostID != ownerHostID {
		return nil, errors.New("only the event owner can manage its co-hosts")
	}
	return inv, nil
}

// emailCoHostSignupInvite asks someone with no host profile yet to join (or
// become a host) to co-host the event. hasAccount: they already have a
// MySlotMate account and only need to become a host.
func (s *payoutService) emailCoHostSignupInvite(ctx context.Context, inv *repository.CoHostEmailInvite, evt *models.Event, owner *models.Host, hasAccount bool) error {
	if s.notif == nil {
		return errors.New("email is not configured on this server")
	}
	title, when := "an experience", ""
	if evt != nil {
		title = evt.Title
		when = timeutil.FormatEventTime(evt.Time)
	}
	step := "Create your MySlotMate account with this email address, then apply to become a host."
	if hasAccount {
		step = "Sign in to MySlotMate with this email address and apply to become a host."
	}
	body := fmt.Sprintf(`
<p>Hi there,</p>
<p><strong>%s</strong> invited you to co-host <strong>%s</strong>%s on MySlotMate.</p>
<p>As a co-host you can manage the experience with them: edit it, see who booked, and check guests in.</p>
<p>%s The invitation will be waiting under <strong>Co-hosts</strong> in your host dashboard, ready to accept.</p>
<p><a href="%s">Become a host on MySlotMate</a></p>
<p style="color:#666;font-size:12px">You are seeing this because someone entered this email address as a co-host on MySlotMate.</p>`,
		htmlEscape(hostDisplayName(owner)), htmlEscape(title), htmlWhen(when), step, s.frontendBaseURL+"/become-host")
	subject := fmt.Sprintf("%s invited you to co-host %s on MySlotMate", hostDisplayName(owner), title)
	return s.notif.SendCustomEmail(ctx, inv.Email, subject, body)
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
	rows, err := s.cohostRepo.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	waiting, err := s.cohostRepo.ListEmailInvites(ctx, eventID)
	if err != nil {
		return nil, err
	}
	return append(waiting, rows...), nil
}

func (s *payoutService) SetCoHostCanWithdraw(ctx context.Context, ownerHostID, cohostID uuid.UUID, canWithdraw bool) error {
	if inv, err := s.ownedEmailInvite(ctx, ownerHostID, cohostID); err != nil {
		return err
	} else if inv != nil {
		return s.cohostRepo.SetEmailInviteCanWithdraw(ctx, inv.ID, canWithdraw)
	}
	ch, err := s.ownedEventCoHost(ctx, ownerHostID, cohostID)
	if err != nil {
		return err
	}
	// Turning the toggle off only gates FUTURE withdrawals; a payout already in
	// flight or completed is untouched by design.
	return s.cohostRepo.SetCanWithdraw(ctx, ch.ID, canWithdraw)
}

func (s *payoutService) RevokeCoHost(ctx context.Context, ownerHostID, cohostID uuid.UUID) error {
	if inv, err := s.ownedEmailInvite(ctx, ownerHostID, cohostID); err != nil {
		return err
	} else if inv != nil {
		return s.cohostRepo.SetEmailInviteStatus(ctx, inv.ID, "revoked")
	}
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
	status := models.CoHostDeclined
	if accept {
		status = models.CoHostAccepted
	}
	if err := s.cohostRepo.SetStatus(ctx, ch.ID, status); err != nil {
		return err
	}
	// Tell the owner, in the app. Best-effort.
	if evt, _ := s.eventRepo.GetByID(ctx, ch.EventID); evt != nil {
		if owner, _ := s.hostRepo.GetByID(ctx, ch.InvitedByHostID); owner != nil {
			me, _ := s.hostRepo.GetByID(ctx, hostID)
			verb := "declined"
			if accept {
				verb = "accepted"
			}
			if err := s.cohostRepo.Notify(ctx, owner.UserID, "Co-host invitation "+verb,
				fmt.Sprintf("%s %s your invitation to co-host %s.", hostDisplayName(me), verb, evt.Title), nil); err != nil {
				fmt.Printf("[COHOST] in-app notice failed: %v\n", err)
			}
		}
	}
	return nil
}

// attachEmailInvites turns invites sent to this host's email before they were a
// host into normal invitations, and tells them in the app. Best-effort: it runs
// on every dashboard load (summary, shared list), so a miss is retried.
func (s *payoutService) attachEmailInvites(ctx context.Context, hostID uuid.UUID) {
	attached, err := s.cohostRepo.AttachEmailInvites(ctx, hostID)
	if err != nil {
		fmt.Printf("[COHOST] attach email invites for %s: %v\n", hostID, err)
		return
	}
	if len(attached) == 0 {
		return
	}
	me, _ := s.hostRepo.GetByID(ctx, hostID)
	if me == nil {
		return
	}
	for _, a := range attached {
		if err := s.cohostRepo.Notify(ctx, me.UserID, "Co-host invitation",
			fmt.Sprintf("%s invited you to co-host %s. Accept or decline it under Co-hosts in your host dashboard.",
				firstNonEmpty(a.OwnerName, "A host"), a.EventTitle), nil); err != nil {
			fmt.Printf("[COHOST] in-app notice failed: %v\n", err)
		}
	}
}

func (s *payoutService) CoHostSummary(ctx context.Context, hostID uuid.UUID) (*CoHostSummary, error) {
	s.attachEmailInvites(ctx, hostID)
	received, granted, err := s.cohostRepo.CountsForHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	return &CoHostSummary{Received: received, Granted: granted}, nil
}

func (s *payoutService) ListSharedEvents(ctx context.Context, hostID uuid.UUID) ([]*repository.SharedEventRow, error) {
	s.attachEmailInvites(ctx, hostID)
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
