package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/lib/validation"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type HostService interface {
	// Application flow (Become a Host)
	SubmitApplication(ctx context.Context, userID uuid.UUID, req HostApplicationRequest) (*models.Host, error)
	SaveDraft(ctx context.Context, userID uuid.UUID, req HostApplicationRequest) (*models.Host, error)
	GetApplicationStatus(ctx context.Context, userID uuid.UUID) (*models.Host, error)

	// Admin — approve / reject applications
	ApproveApplication(ctx context.Context, hostID uuid.UUID) (*models.Host, error)
	RejectApplication(ctx context.Context, hostID uuid.UUID, reason string) (*models.Host, error)
	// SetApplicationStatus is an admin override that moves a host to any
	// application status (used by the admin dashboard). It applies the same
	// side effects as approve/reject when entering those states.
	SetApplicationStatus(ctx context.Context, hostID uuid.UUID, status models.HostApplicationStatus) (*models.Host, error)
	ListPendingApplications(ctx context.Context) ([]*models.Host, error)

	// Public
	GetHostByID(ctx context.Context, hostID uuid.UUID) (*models.Host, error)
	ListApprovedHosts(ctx context.Context) ([]*models.Host, error)

	// Profile management
	GetHostByUserID(ctx context.Context, userID uuid.UUID) (*models.Host, error)
	UpdateProfile(ctx context.Context, hostID uuid.UUID, req HostProfileUpdateRequest) (*models.Host, error)

	// Social media connect/disconnect
	ConnectSocial(ctx context.Context, hostID uuid.UUID, req SocialConnectRequest) (*models.Host, error)
	DisconnectSocial(ctx context.Context, hostID uuid.UUID, platform string) (*models.Host, error)

	// Dashboard overview
	GetDashboardOverview(ctx context.Context, hostID uuid.UUID) (*HostDashboardOverview, error)

	// Attention items
	GetAttentionItems(ctx context.Context, hostID uuid.UUID) (*HostAttentionItems, error)

	// Earnings breakdown
	GetEarningsBreakdown(ctx context.Context, hostID uuid.UUID) (*HostEarningsBreakdown, error)
}

// HostApplicationRequest maps to the "Become a Host" form (Steps 1 & 2).
type HostApplicationRequest struct {
	FirstName       string   `json:"first_name"`
	LastName        string   `json:"last_name"`
	City            string   `json:"city"`
	ExperienceDesc  *string  `json:"experience_desc,omitempty"`
	Moods           []string `json:"moods,omitempty"`
	Description     *string  `json:"description,omitempty"`
	PreferredDays   []string `json:"preferred_days,omitempty"`
	GroupSize       *int     `json:"group_size,omitempty"`
	GovernmentIDURL *string  `json:"government_id_url,omitempty"`
	AvatarURL       *string  `json:"avatar_url,omitempty"`
	Tagline         *string  `json:"tagline,omitempty"`
	Bio             *string  `json:"bio,omitempty"`
	SocialInstagram *string  `json:"social_instagram,omitempty"`
	SocialLinkedin  *string  `json:"social_linkedin,omitempty"`
	SocialWebsite   *string  `json:"social_website,omitempty"`
}

// HostProfileUpdateRequest maps to the Host Profile edit screen.
type HostProfileUpdateRequest struct {
	FirstName       *string  `json:"first_name,omitempty"`
	LastName        *string  `json:"last_name,omitempty"`
	AvatarURL       *string  `json:"avatar_url,omitempty"`
	Tagline         *string  `json:"tagline,omitempty"`
	Bio             *string  `json:"bio,omitempty"`
	ExpertiseTags   []string `json:"expertise_tags,omitempty"`
	SocialInstagram *string  `json:"social_instagram,omitempty"`
	SocialLinkedin  *string  `json:"social_linkedin,omitempty"`
	SocialWebsite   *string  `json:"social_website,omitempty"`
}

// HostDashboardOverview powers the Host Dashboard overview screen.
type HostDashboardOverview struct {
	TotalEvents     int     `json:"total_events"`
	TotalBookings   int     `json:"total_bookings"`
	TotalEarnings   int64   `json:"total_earnings_cents"`
	AvgRating       float64 `json:"avg_rating"`
	TotalReviews    int     `json:"total_reviews"`
	UpcomingToday   int     `json:"upcoming_today"`
	MonthlyBookings int     `json:"monthly_bookings"`
}

// SocialConnectRequest maps to social media connect/url-submit.
type SocialConnectRequest struct {
	Platform string `json:"platform"` // "instagram", "youtube", "twitter", "linkedin", "website"
	URL      string `json:"url"`
}

// AttentionItem represents a single item that needs the host's attention.
type AttentionItem struct {
	Type    string      `json:"type"` // "cancelled_booking", "pending_review", "unread_message", "low_rating"
	Count   int         `json:"count"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// HostAttentionItems aggregates all items needing the host's attention.
type HostAttentionItems struct {
	Items      []AttentionItem `json:"items"`
	TotalCount int             `json:"total_count"`
}

// EarningsBreakdownItem represents earnings for a single event.
type EarningsBreakdownItem struct {
	EventID       uuid.UUID `json:"event_id"`
	EventTitle    string    `json:"event_title"`
	TotalBookings int       `json:"total_bookings"`
	GrossEarnings int64     `json:"gross_earnings_cents"`
	ServiceFee    int64     `json:"service_fee_cents"`
	NetEarnings   int64     `json:"net_earnings_cents"`
}

// HostEarningsBreakdown contains per-event earnings detail.
type HostEarningsBreakdown struct {
	TotalEarningsCents    int64                   `json:"total_earnings_cents"`
	PendingClearanceCents int64                   `json:"pending_clearance_cents"`
	AvailableBalanceCents int64                   `json:"available_balance_cents"`
	Events                []EarningsBreakdownItem `json:"events"`
}

type hostService struct {
	hostRepo    repository.HostRepository
	userRepo    repository.UserRepository
	eventRepo   repository.EventRepository
	bookingRepo repository.BookingRepository
	reviewRepo  repository.ReviewRepository
	payoutRepo  repository.PayoutRepository
	accountRepo repository.AccountRepository
	dispatcher  *event.Dispatcher
}

func NewHostService(
	hr repository.HostRepository,
	ur repository.UserRepository,
	er repository.EventRepository,
	br repository.BookingRepository,
	rr repository.ReviewRepository,
	pr repository.PayoutRepository,
	ar repository.AccountRepository,
	d *event.Dispatcher,
) HostService {
	return &hostService{
		hostRepo:    hr,
		userRepo:    ur,
		eventRepo:   er,
		bookingRepo: br,
		reviewRepo:  rr,
		payoutRepo:  pr,
		accountRepo: ar,
		dispatcher:  d,
	}
}

func (s *hostService) SaveDraft(ctx context.Context, userID uuid.UUID, req HostApplicationRequest) (*models.Host, error) {
	return s.saveHostApplication(ctx, userID, req, models.HostApplicationDraft)
}

func (s *hostService) SubmitApplication(ctx context.Context, userID uuid.UUID, req HostApplicationRequest) (*models.Host, error) {
	return s.saveHostApplication(ctx, userID, req, models.HostApplicationPending)
}

func (s *hostService) saveHostApplication(ctx context.Context, userID uuid.UUID, req HostApplicationRequest, status models.HostApplicationStatus) (*models.Host, error) {
	// 1. Check if user exists
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, errors.New("user not found")
	}

	now := time.Now()

	// 2. Check if host application already exists — update if so
	existing, err := s.hostRepo.GetByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		// Can only update if draft or rejected
		if existing.ApplicationStatus != models.HostApplicationDraft && existing.ApplicationStatus != models.HostApplicationRejected {
			return nil, errors.New("application already submitted")
		}
		existing.FirstName = req.FirstName
		existing.LastName = req.LastName
		existing.City = req.City
		existing.ExperienceDesc = req.ExperienceDesc
		existing.Moods = pq.StringArray(req.Moods)
		existing.Description = req.Description
		existing.PreferredDays = pq.StringArray(req.PreferredDays)
		existing.GroupSize = req.GroupSize
		existing.GovernmentIDURL = req.GovernmentIDURL
		existing.AvatarURL = req.AvatarURL
		existing.Tagline = req.Tagline
		existing.Bio = req.Bio
		existing.SocialInstagram = req.SocialInstagram
		existing.SocialLinkedin = req.SocialLinkedin
		existing.SocialWebsite = req.SocialWebsite
		existing.ApplicationStatus = status
		if status == models.HostApplicationPending {
			existing.SubmittedAt = &now
		}
		if err := s.hostRepo.Update(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	// 3. Create new host application
	newHost := &models.Host{
		ID:                uuid.New(),
		UserID:            userID,
		FirstName:         req.FirstName,
		LastName:          req.LastName,
		PhnNumber:         user.PhnNumber,
		City:              req.City,
		AvatarURL:         req.AvatarURL,
		Tagline:           req.Tagline,
		Bio:               req.Bio,
		ApplicationStatus: status,
		ExperienceDesc:    req.ExperienceDesc,
		Moods:             pq.StringArray(req.Moods),
		Description:       req.Description,
		PreferredDays:     pq.StringArray(req.PreferredDays),
		GroupSize:         req.GroupSize,
		GovernmentIDURL:   req.GovernmentIDURL,
		SocialInstagram:   req.SocialInstagram,
		SocialLinkedin:    req.SocialLinkedin,
		SocialWebsite:     req.SocialWebsite,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if status == models.HostApplicationPending {
		newHost.SubmittedAt = &now
	}

	if err := s.hostRepo.Create(ctx, newHost); err != nil {
		return nil, err
	}

	if status == models.HostApplicationPending {
		s.dispatcher.Publish(event.HostCreated, newHost)
	}

	return newHost, nil
}

func (s *hostService) GetApplicationStatus(ctx context.Context, userID uuid.UUID) (*models.Host, error) {
	return s.hostRepo.GetByUserID(ctx, userID)
}

func (s *hostService) ListApprovedHosts(ctx context.Context) ([]*models.Host, error) {
	return s.hostRepo.ListByStatus(ctx, models.HostApplicationApproved)
}

func (s *hostService) GetHostByID(ctx context.Context, hostID uuid.UUID) (*models.Host, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}
	if host.ApplicationStatus != models.HostApplicationApproved {
		return nil, errors.New("host not found")
	}
	return host, nil
}

func (s *hostService) GetHostByUserID(ctx context.Context, userID uuid.UUID) (*models.Host, error) {
	return s.hostRepo.GetByUserID(ctx, userID)
}

func (s *hostService) UpdateProfile(ctx context.Context, hostID uuid.UUID, req HostProfileUpdateRequest) (*models.Host, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	if req.FirstName != nil {
		host.FirstName = *req.FirstName
	}
	if req.LastName != nil {
		host.LastName = *req.LastName
	}
	if req.AvatarURL != nil {
		// Validate avatar URL: reject blob URLs and localhost URLs
		if err := validation.ValidateImageURL(*req.AvatarURL); err != nil {
			return nil, err
		}
		host.AvatarURL = req.AvatarURL
	}
	if req.Tagline != nil {
		host.Tagline = req.Tagline
	}
	if req.Bio != nil {
		host.Bio = req.Bio
	}
	if req.ExpertiseTags != nil {
		host.ExpertiseTags = pq.StringArray(req.ExpertiseTags)
	}
	if req.SocialInstagram != nil {
		host.SocialInstagram = req.SocialInstagram
	}
	if req.SocialLinkedin != nil {
		host.SocialLinkedin = req.SocialLinkedin
	}
	if req.SocialWebsite != nil {
		host.SocialWebsite = req.SocialWebsite
	}

	if err := s.hostRepo.Update(ctx, host); err != nil {
		return nil, err
	}

	return host, nil
}

func (s *hostService) GetDashboardOverview(ctx context.Context, hostID uuid.UUID) (*HostDashboardOverview, error) {
	fmt.Printf("[DASHBOARD] GetDashboardOverview: hostID=%s\n", hostID)

	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		fmt.Printf("[DASHBOARD] GetDashboardOverview: host fetch error: %v\n", err)
		return nil, err
	}
	if host == nil {
		fmt.Printf("[DASHBOARD] GetDashboardOverview: host not found\n")
		return nil, errors.New("host not found")
	}
	fmt.Printf("[DASHBOARD] GetDashboardOverview: host found - %s %s\n", host.FirstName, host.LastName)

	// Get all events for this host
	events, err := s.eventRepo.ListByHostID(ctx, hostID)
	if err != nil {
		fmt.Printf("[DASHBOARD] GetDashboardOverview: events fetch error: %v\n", err)
		return nil, err
	}
	fmt.Printf("[DASHBOARD] GetDashboardOverview: found %d events\n", len(events))

	// Calculate total bookings by summing all event.total_bookings
	totalBookings := 0
	for i, evt := range events {
		fmt.Printf("[DASHBOARD]   Event[%d]: id=%s, title=%s, total_bookings=%d\n",
			i, evt.ID, evt.Title, evt.TotalBookings)
		totalBookings += evt.TotalBookings
	}
	fmt.Printf("[DASHBOARD] GetDashboardOverview: total bookings aggregated = %d\n", totalBookings)

	// Earnings
	earnings, err := s.payoutRepo.GetHostEarnings(ctx, hostID)
	if err != nil {
		fmt.Printf("[DASHBOARD] GetDashboardOverview: earnings fetch error: %v\n", err)
		return nil, err
	}

	overview := &HostDashboardOverview{
		TotalEvents:   len(events),
		TotalBookings: totalBookings,
		TotalReviews:  host.TotalReviews,
	}
	if host.AvgRating != nil {
		overview.AvgRating = *host.AvgRating
	}
	if earnings != nil {
		overview.TotalEarnings = earnings.TotalEarningsCents
	}

	fmt.Printf("[DASHBOARD] GetDashboardOverview: returning overview - events=%d, bookings=%d\n",
		overview.TotalEvents, overview.TotalBookings)
	return overview, nil
}

// ── Admin: Approve / Reject ─────────────────────────────────────────────────

func (s *hostService) ApproveApplication(ctx context.Context, hostID uuid.UUID) (*models.Host, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	if host.ApplicationStatus != models.HostApplicationPending && host.ApplicationStatus != models.HostApplicationUnderReview {
		return nil, errors.New("application is not in a reviewable state")
	}

	now := time.Now()
	host.ApplicationStatus = models.HostApplicationApproved
	host.ApprovedAt = &now
	host.IsIdentityVerified = true

	if err := s.hostRepo.Update(ctx, host); err != nil {
		return nil, err
	}

	if err := s.userRepo.SetVerified(ctx, host.UserID); err != nil {
		return nil, fmt.Errorf("failed to mark user as verified: %w", err)
	}

	s.dispatcher.Publish(event.HostApproved, host)
	return host, nil
}

func (s *hostService) RejectApplication(ctx context.Context, hostID uuid.UUID, reason string) (*models.Host, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	if host.ApplicationStatus != models.HostApplicationPending && host.ApplicationStatus != models.HostApplicationUnderReview {
		return nil, errors.New("application is not in a reviewable state")
	}

	now := time.Now()
	host.ApplicationStatus = models.HostApplicationRejected
	host.RejectedAt = &now

	if err := s.hostRepo.Update(ctx, host); err != nil {
		return nil, err
	}

	s.dispatcher.Publish(event.HostRejected, host)
	return host, nil
}

// SetApplicationStatus moves a host to an arbitrary application status (admin
// override). Unlike Approve/Reject it has no "reviewable state" guard, so an
// admin can correct a status from any state. Entering approved/rejected applies
// the same side effects as the dedicated handlers.
func (s *hostService) SetApplicationStatus(ctx context.Context, hostID uuid.UUID, status models.HostApplicationStatus) (*models.Host, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	// No-op if already in the requested state.
	if host.ApplicationStatus == status {
		return host, nil
	}

	now := time.Now()
	host.ApplicationStatus = status
	switch status {
	case models.HostApplicationApproved:
		host.ApprovedAt = &now
		host.IsIdentityVerified = true
	case models.HostApplicationRejected:
		host.RejectedAt = &now
	}

	if err := s.hostRepo.Update(ctx, host); err != nil {
		return nil, err
	}

	// Side effects when entering a terminal review state.
	switch status {
	case models.HostApplicationApproved:
		if err := s.userRepo.SetVerified(ctx, host.UserID); err != nil {
			return nil, fmt.Errorf("failed to mark user as verified: %w", err)
		}
		s.dispatcher.Publish(event.HostApproved, host)
	case models.HostApplicationRejected:
		s.dispatcher.Publish(event.HostRejected, host)
	}

	return host, nil
}

func (s *hostService) ListPendingApplications(ctx context.Context) ([]*models.Host, error) {
	return s.hostRepo.ListByStatus(ctx, models.HostApplicationPending)
}

// ── Social Connect / Disconnect ─────────────────────────────────────────────

func (s *hostService) ConnectSocial(ctx context.Context, hostID uuid.UUID, req SocialConnectRequest) (*models.Host, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	switch req.Platform {
	case "instagram":
		host.SocialInstagram = &req.URL
	case "linkedin":
		host.SocialLinkedin = &req.URL
	case "website":
		host.SocialWebsite = &req.URL
	case "youtube":
		// YouTube stored in SocialWebsite as secondary, or add a field.
		// For now, use a convention: store in social_website with prefix.
		url := req.URL
		host.SocialWebsite = &url
	case "twitter":
		// Same pattern — stored in social_website for now.
		url := req.URL
		host.SocialWebsite = &url
	default:
		return nil, errors.New("unsupported platform: " + req.Platform)
	}

	host.UpdatedAt = time.Now()
	if err := s.hostRepo.Update(ctx, host); err != nil {
		return nil, err
	}
	return host, nil
}

func (s *hostService) DisconnectSocial(ctx context.Context, hostID uuid.UUID, platform string) (*models.Host, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	switch platform {
	case "instagram":
		host.SocialInstagram = nil
	case "linkedin":
		host.SocialLinkedin = nil
	case "website", "youtube", "twitter":
		host.SocialWebsite = nil
	default:
		return nil, errors.New("unsupported platform: " + platform)
	}

	host.UpdatedAt = time.Now()
	if err := s.hostRepo.Update(ctx, host); err != nil {
		return nil, err
	}
	return host, nil
}

// ── Attention Items ─────────────────────────────────────────────────────────

func (s *hostService) GetAttentionItems(ctx context.Context, hostID uuid.UUID) (*HostAttentionItems, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	var items []AttentionItem

	// 1. Get all event IDs for this host
	eventIDs, err := s.eventRepo.ListByHostIDForIDs(ctx, hostID)
	if err != nil {
		return nil, err
	}

	// 2. Recently cancelled bookings
	if len(eventIDs) > 0 {
		cancelled, err := s.bookingRepo.ListRecentCancelledByEventIDs(ctx, eventIDs, 10)
		if err != nil {
			return nil, err
		}
		if len(cancelled) > 0 {
			items = append(items, AttentionItem{
				Type:    "cancelled_booking",
				Count:   len(cancelled),
				Message: fmt.Sprintf("You have %d recently cancelled booking(s)", len(cancelled)),
				Data:    cancelled,
			})
		}
	}

	// 3. Pending reviews (confirmed bookings without reviews)
	if len(eventIDs) > 0 {
		confirmedCount, err := s.bookingRepo.CountConfirmedByEventIDs(ctx, eventIDs)
		if err != nil {
			return nil, err
		}
		pendingReviews, err := s.reviewRepo.CountPendingReviewsByEventIDs(ctx, eventIDs, confirmedCount)
		if err != nil {
			return nil, err
		}
		if pendingReviews > 0 {
			items = append(items, AttentionItem{
				Type:    "pending_review",
				Count:   pendingReviews,
				Message: fmt.Sprintf("%d booking(s) awaiting guest reviews", pendingReviews),
			})
		}
	}

	// 4. Low rating warning
	if host.AvgRating != nil && *host.AvgRating < 3.5 && host.TotalReviews > 0 {
		items = append(items, AttentionItem{
			Type:    "low_rating",
			Count:   1,
			Message: fmt.Sprintf("Your average rating is %.1f — consider improving guest experience", *host.AvgRating),
		})
	}

	totalCount := 0
	for _, item := range items {
		totalCount += item.Count
	}

	return &HostAttentionItems{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}

// ── Earnings Breakdown ──────────────────────────────────────────────────────

func (s *hostService) GetEarningsBreakdown(ctx context.Context, hostID uuid.UUID) (*HostEarningsBreakdown, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	// Get aggregate earnings
	earnings, err := s.payoutRepo.GetHostEarnings(ctx, hostID)
	if err != nil {
		return nil, err
	}

	// Get host account balance
	var availableBalance int64
	if s.accountRepo != nil {
		account, err := s.accountRepo.GetByOwner(ctx, models.AccountOwnerHost, hostID)
		if err == nil && account != nil {
			availableBalance = account.BalanceCents
		}
	}

	// Get all events for this host with booking data
	events, err := s.eventRepo.ListByHostID(ctx, hostID)
	if err != nil {
		return nil, err
	}

	var breakdownItems []EarningsBreakdownItem
	for _, evt := range events {
		// Get bookings for this event
		bookings, err := s.bookingRepo.ListByEventID(ctx, evt.ID)
		if err != nil {
			continue
		}

		var grossEarnings, serviceFee, netEarnings int64
		for _, b := range bookings {
			if b.AmountCents != nil {
				grossEarnings += *b.AmountCents
			}
			if b.ServiceFeeCents != nil {
				serviceFee += *b.ServiceFeeCents
			}
			if b.NetEarningCents != nil {
				netEarnings += *b.NetEarningCents
			}
		}

		if len(bookings) > 0 {
			breakdownItems = append(breakdownItems, EarningsBreakdownItem{
				EventID:       evt.ID,
				EventTitle:    evt.Title,
				TotalBookings: len(bookings),
				GrossEarnings: grossEarnings,
				ServiceFee:    serviceFee,
				NetEarnings:   netEarnings,
			})
		}
	}

	result := &HostEarningsBreakdown{
		Events: breakdownItems,
	}
	if earnings != nil {
		result.TotalEarningsCents = earnings.TotalEarningsCents
		result.PendingClearanceCents = earnings.PendingClearanceCents
	}
	result.AvailableBalanceCents = availableBalance

	return result, nil
}
