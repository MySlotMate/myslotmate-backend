package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/lib/instagram"
	"myslotmate-backend/internal/lib/storage"
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
	// AdminUpdateProfile is the admin-dashboard edit path: same fields plus
	// admin-only ones (city, phone, description, badges, government ID), and it
	// skips the Instagram rescrape side effect of UpdateProfile.
	AdminUpdateProfile(ctx context.Context, hostID uuid.UUID, req HostProfileUpdateRequest) (*models.Host, error)

	// Social media connect/disconnect
	ConnectSocial(ctx context.Context, hostID uuid.UUID, req SocialConnectRequest) (*models.Host, error)
	DisconnectSocial(ctx context.Context, hostID uuid.UUID, platform string) (*models.Host, error)

	// Dashboard overview
	GetDashboardOverview(ctx context.Context, hostID uuid.UUID) (*HostDashboardOverview, error)

	// Attention items
	GetAttentionItems(ctx context.Context, hostID uuid.UUID) (*HostAttentionItems, error)

	// Earnings breakdown
	GetEarningsBreakdown(ctx context.Context, hostID uuid.UUID) (*HostEarningsBreakdown, error)

	// SetPlatformFeePercentage sets this host's commission override — the
	// platform's cut of each of their bookings (host keeps the remainder).
	// Pass nil to clear the override and fall back to the global default.
	SetPlatformFeePercentage(ctx context.Context, hostID uuid.UUID, platformPercentage *int) (*models.Host, error)
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
	IsProfessional  *bool    `json:"is_professional,omitempty"`
}

// HostProfileUpdateRequest maps to the Host Profile edit screen. Every field is
// optional-by-pointer/slice: a nil pointer (or nil slice) means "leave
// unchanged", so partial updates only touch the fields the caller sends. The
// extended fields below the socials block are admin-only (see AdminUpdateProfile)
// — the host self-edit endpoint never populates them.
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

	// ── Admin-only extended fields ──────────────────────────────────────────
	City               *string  `json:"city,omitempty"`
	PhnNumber          *string  `json:"phn_number,omitempty"`
	Description        *string  `json:"description,omitempty"`
	ExperienceDesc     *string  `json:"experience_desc,omitempty"`
	GroupSize          *int     `json:"group_size,omitempty"`
	GovernmentIDURL    *string  `json:"government_id_url,omitempty"`
	GalleryURLs        []string `json:"gallery_urls,omitempty"`
	Moods              []string `json:"moods,omitempty"`
	PreferredDays      []string `json:"preferred_days,omitempty"`
	IsIdentityVerified *bool    `json:"is_identity_verified,omitempty"`
	IsSuperHost        *bool    `json:"is_super_host,omitempty"`
	IsCommunityChamp   *bool    `json:"is_community_champ,omitempty"`
	IsProfessional     *bool    `json:"is_professional,omitempty"`
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
	uploads     *storage.UploadService // nil when S3 is not configured
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
	us *storage.UploadService,
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
		uploads:     us,
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
		if req.IsProfessional != nil {
			existing.IsProfessional = *req.IsProfessional
		}
		existing.ApplicationStatus = status
		if status == models.HostApplicationPending {
			existing.SubmittedAt = &now
		}
		if err := s.hostRepo.Update(ctx, existing); err != nil {
			return nil, err
		}
		if status == models.HostApplicationPending {
			s.maybeScrapeInstagramMedia(existing)
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
		IsProfessional:    req.IsProfessional != nil && *req.IsProfessional,
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
		s.maybeScrapeInstagramMedia(newHost)
	}

	return newHost, nil
}

// maybeScrapeInstagramMedia runs the one-time Instagram scrape in the
// background when a host applies without a profile photo but with an
// Instagram link: it re-hosts the profile photo (used as the avatar) and up
// to 3 recent post photos (shown in the host page gallery) on S3. Strictly
// best-effort — any failure is logged and the application proceeds untouched.
func (s *hostService) maybeScrapeInstagramMedia(host *models.Host) {
	if s.uploads == nil || host.InstagramScrapedAt != nil {
		return
	}
	hasAvatar := host.AvatarURL != nil && *host.AvatarURL != ""
	if hasAvatar || host.SocialInstagram == nil {
		return
	}
	snapshot := *host // copy so the goroutine is not affected by later mutation
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if _, _, err := ScrapeInstagramMedia(ctx, s.uploads, s.hostRepo, &snapshot); err != nil {
			log.Printf("[instagram-scrape] host=%s: %v", snapshot.ID, err)
		}
	}()
}

// instagramHandleChanged reports whether two Instagram links point at a
// different profile (by username). Trivial URL variations (trailing slash,
// tracking query params) are treated as unchanged; adding a link where there
// was none counts as a change.
func instagramHandleChanged(oldLink, newLink *string) bool {
	old := ""
	if oldLink != nil {
		old = instagram.UsernameFromURL(*oldLink)
	}
	nw := ""
	if newLink != nil {
		nw = instagram.UsernameFromURL(*newLink)
	}
	return nw != "" && nw != old
}

// rescrapeInstagram re-pulls a host's Instagram media in the background after
// their Instagram link changed: it refreshes the gallery, and — only if the
// current avatar itself came from Instagram — the avatar too. A host-uploaded
// avatar is left untouched. Best-effort; failures are logged and the existing
// media is kept.
func (s *hostService) rescrapeInstagram(host *models.Host) {
	if s.uploads == nil || host.SocialInstagram == nil {
		return
	}
	if instagram.UsernameFromURL(*host.SocialInstagram) == "" {
		return
	}
	snapshot := *host
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if _, _, err := ScrapeInstagramMedia(ctx, s.uploads, s.hostRepo, &snapshot); err != nil {
			log.Printf("[instagram-rescrape] host=%s: %v", snapshot.ID, err)
		}
	}()
}

// ScrapeInstagramMedia fetches a host's public Instagram profile photo and up
// to 4 recent post photos, re-hosts them on S3, and persists them via
// SaveInstagramMedia (which stamps instagram_scraped_at so it never repeats).
// Synchronous and self-contained so both the create-time goroutine and the
// backfill job can call it. Best-effort: a fetch/upload failure returns an
// error but any media that did succeed is still saved.
func ScrapeInstagramMedia(ctx context.Context, uploads *storage.UploadService, hostRepo repository.HostRepository, host *models.Host) (avatarSet bool, galleryCount int, err error) {
	if uploads == nil {
		return false, 0, errors.New("upload service not configured")
	}
	if host.SocialInstagram == nil {
		return false, 0, errors.New("host has no instagram link")
	}
	username := instagram.UsernameFromURL(*host.SocialInstagram)
	if username == "" {
		return false, 0, fmt.Errorf("could not extract username from %q", *host.SocialInstagram)
	}

	profile, err := instagram.FetchProfile(ctx, username, 4)
	if err != nil {
		return false, 0, fmt.Errorf("user=%s: fetch failed: %w", username, err)
	}

	// Pull the avatar when the host has none yet, or when their current avatar
	// itself came from Instagram (so a handle change can refresh it). A photo
	// the host uploaded themselves is never touched.
	hasAvatar := host.AvatarURL != nil && *host.AvatarURL != ""
	var avatarURL *string
	if !hasAvatar || host.AvatarFromInstagram {
		if res, upErr := uploads.UploadFromURL(ctx, "hosts/avatars", profile.ProfilePicURL, username+"_profile"); upErr != nil {
			log.Printf("[instagram-scrape] host=%s user=%s: avatar upload failed: %v", host.ID, username, upErr)
		} else {
			avatarURL = &res.URL
		}
	}

	var galleryURLs []string
	for i, postURL := range profile.RecentPosts {
		res, upErr := uploads.UploadFromURL(ctx, "hosts/gallery", postURL, fmt.Sprintf("%s_post_%d", username, i+1))
		if upErr != nil {
			log.Printf("[instagram-scrape] host=%s user=%s: post %d upload failed: %v", host.ID, username, i+1, upErr)
			continue
		}
		galleryURLs = append(galleryURLs, res.URL)
	}

	if avatarURL == nil && len(galleryURLs) == 0 {
		// Nothing worth persisting; leave instagram_scraped_at unset so a
		// later retry (re-submission or a re-run of the backfill) can try again.
		return false, 0, fmt.Errorf("user=%s: nothing usable scraped", username)
	}
	if err := hostRepo.SaveInstagramMedia(ctx, host.ID, avatarURL, galleryURLs); err != nil {
		return false, 0, fmt.Errorf("user=%s: save failed: %w", username, err)
	}
	log.Printf("[instagram-scrape] host=%s user=%s: saved avatar=%t gallery=%d", host.ID, username, avatarURL != nil, len(galleryURLs))
	return avatarURL != nil, len(galleryURLs), nil
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

	oldInstagram := host.SocialInstagram

	if err := applyProfileUpdate(host, req); err != nil {
		return nil, err
	}

	if err := s.hostRepo.Update(ctx, host); err != nil {
		return nil, err
	}

	// If the host pointed us at a different Instagram profile, refresh their
	// scraped media to match the new handle.
	if instagramHandleChanged(oldInstagram, host.SocialInstagram) {
		s.rescrapeInstagram(host)
	}

	return host, nil
}

// AdminUpdateProfile applies a profile edit made from the admin dashboard. It
// shares field-application logic with UpdateProfile but deliberately skips the
// Instagram rescrape side effect: an admin correcting a host's fields should
// never silently clobber the host's scraped gallery/avatar. Admin-only fields
// (city, phone, description, badges, government ID, etc.) are applied here too.
func (s *hostService) AdminUpdateProfile(ctx context.Context, hostID uuid.UUID, req HostProfileUpdateRequest) (*models.Host, error) {
	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	if err := applyProfileUpdate(host, req); err != nil {
		return nil, err
	}

	if err := s.hostRepo.Update(ctx, host); err != nil {
		return nil, err
	}
	return host, nil
}

// applyProfileUpdate mutates host in place from the non-nil fields of req. A nil
// pointer / nil slice leaves the corresponding field untouched; an empty slice
// clears it. Shared by UpdateProfile (host self-edit) and AdminUpdateProfile.
func applyProfileUpdate(host *models.Host, req HostProfileUpdateRequest) error {
	if req.FirstName != nil {
		host.FirstName = *req.FirstName
	}
	if req.LastName != nil {
		host.LastName = *req.LastName
	}
	if req.AvatarURL != nil {
		// Validate avatar URL: reject blob URLs and localhost URLs
		if err := validation.ValidateImageURL(*req.AvatarURL); err != nil {
			return err
		}
		host.AvatarURL = req.AvatarURL
		// A supplied photo is no longer sourced from Instagram.
		host.AvatarFromInstagram = false
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

	// ── Admin-only extended fields ──────────────────────────────────────────
	if req.City != nil {
		host.City = *req.City
	}
	if req.PhnNumber != nil {
		host.PhnNumber = *req.PhnNumber
	}
	if req.Description != nil {
		host.Description = req.Description
	}
	if req.ExperienceDesc != nil {
		host.ExperienceDesc = req.ExperienceDesc
	}
	if req.GroupSize != nil {
		host.GroupSize = req.GroupSize
	}
	if req.GovernmentIDURL != nil {
		host.GovernmentIDURL = req.GovernmentIDURL
	}
	if req.GalleryURLs != nil {
		// Validate each gallery URL the same way avatars are checked (reject
		// blob:/localhost). An empty slice is allowed — it clears the gallery.
		for _, u := range req.GalleryURLs {
			if err := validation.ValidateImageURL(u); err != nil {
				return err
			}
		}
		host.GalleryURLs = pq.StringArray(req.GalleryURLs)
	}
	if req.Moods != nil {
		host.Moods = pq.StringArray(req.Moods)
	}
	if req.PreferredDays != nil {
		host.PreferredDays = pq.StringArray(req.PreferredDays)
	}
	if req.IsIdentityVerified != nil {
		host.IsIdentityVerified = *req.IsIdentityVerified
	}
	if req.IsSuperHost != nil {
		host.IsSuperHost = *req.IsSuperHost
	}
	if req.IsCommunityChamp != nil {
		host.IsCommunityChamp = *req.IsCommunityChamp
	}
	if req.IsProfessional != nil {
		host.IsProfessional = *req.IsProfessional
	}
	return nil
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

// ── Admin: Commission override ──────────────────────────────────────────────

func (s *hostService) SetPlatformFeePercentage(ctx context.Context, hostID uuid.UUID, platformPercentage *int) (*models.Host, error) {
	if platformPercentage != nil && (*platformPercentage < 0 || *platformPercentage > 100) {
		return nil, errors.New("platform_percentage must be between 0 and 100")
	}

	host, err := s.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("host not found")
	}

	if err := s.hostRepo.SetPlatformFeePercentage(ctx, hostID, platformPercentage); err != nil {
		return nil, err
	}
	host.PlatformFeePercentage = platformPercentage
	return host, nil
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

	oldInstagram := host.SocialInstagram

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

	// Connecting/changing the Instagram account refreshes the scraped media.
	if req.Platform == "instagram" && instagramHandleChanged(oldInstagram, host.SocialInstagram) {
		s.rescrapeInstagram(host)
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
