package repository

import (
	"context"
	"database/sql"
	"myslotmate-backend/internal/models"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type HostRepository interface {
	Create(ctx context.Context, host *models.Host) error
	GetByID(ctx context.Context, id uuid.UUID) (*models.Host, error)
	GetBySlug(ctx context.Context, slug string) (*models.Host, error)
	SlugExists(ctx context.Context, slug string) (bool, error)
	GetByUserID(ctx context.Context, userID uuid.UUID) (*models.Host, error)
	Update(ctx context.Context, host *models.Host) error
	UpdateApplicationStatus(ctx context.Context, id uuid.UUID, status models.HostApplicationStatus) error
	SetActive(ctx context.Context, id uuid.UUID, active bool) error
	ListByStatus(ctx context.Context, status models.HostApplicationStatus) ([]*models.Host, error)
	UpdateAverageRating(ctx context.Context, hostID uuid.UUID, avgRating float64, totalReviews int) error
	// SetPlatformFeePercentage sets this host's commission override (the
	// platform's cut). Pass nil to clear the override and fall back to the
	// global platform_settings default.
	SetPlatformFeePercentage(ctx context.Context, hostID uuid.UUID, platformPercentage *int) error
	// SetAutoPayout turns the weekly (Tuesday) automatic payout on or off.
	SetAutoPayout(ctx context.Context, hostID uuid.UUID, enabled bool) error
	// ListAutoPayoutHostIDs returns every host with auto payout turned on.
	ListAutoPayoutHostIDs(ctx context.Context) ([]uuid.UUID, error)
	// SaveInstagramMedia stores the result of the one-time Instagram scrape.
	// avatarURL only overwrites an empty avatar_url; instagram_scraped_at is
	// always stamped so the scrape never runs twice.
	SaveInstagramMedia(ctx context.Context, hostID uuid.UUID, avatarURL *string, galleryURLs []string) error
	// ListNeedingInstagramScrape returns hosts with no avatar, an Instagram
	// link, and no prior scrape — the candidates for the backfill job.
	ListNeedingInstagramScrape(ctx context.Context) ([]*models.Host, error)
	// ListScrapedWithInstagram returns hosts that were already scraped and
	// still have an Instagram link — used to re-pull media (e.g. after the
	// post count changed).
	ListScrapedWithInstagram(ctx context.Context) ([]*models.Host, error)
}

type postgresHostRepository struct {
	db *sql.DB
}

func NewHostRepository(db *sql.DB) HostRepository {
	return &postgresHostRepository{db: db}
}

var hostColumns = `id, user_id, account_id,
	first_name, last_name, phn_number, city, avatar_url, tagline, bio,
	application_status, experience_desc, moods, description, preferred_days, group_size,
	government_id_url, submitted_at, approved_at, rejected_at,
	is_identity_verified, is_super_host, is_community_champ, is_professional,
	expertise_tags, social_instagram, social_linkedin, social_website,
	gallery_urls, instagram_scraped_at, avatar_from_instagram,
	avg_rating, total_reviews, platform_fee_percentage, auto_payout_enabled,
	created_at, updated_at,
	is_active,
	events_hosted_override, people_met_override, avg_rating_override,
	slug,
	host_type, selfie_url, community_name, community_category, years_experience, events_hosted,
	portfolio_url, portfolio_photo_urls, profession, certificate_url, degree_url, license_number,
	professional_body, specialization, work_history, languages, achievements, testimonial_url,
	rejection_reason`

func scanHost(row interface {
	Scan(dest ...interface{}) error
}) (*models.Host, error) {
	h := &models.Host{}
	err := row.Scan(
		&h.ID, &h.UserID, &h.AccountID,
		&h.FirstName, &h.LastName, &h.PhnNumber, &h.City, &h.AvatarURL, &h.Tagline, &h.Bio,
		&h.ApplicationStatus, &h.ExperienceDesc, &h.Moods, &h.Description, &h.PreferredDays, &h.GroupSize,
		&h.GovernmentIDURL, &h.SubmittedAt, &h.ApprovedAt, &h.RejectedAt,
		&h.IsIdentityVerified, &h.IsSuperHost, &h.IsCommunityChamp, &h.IsProfessional,
		&h.ExpertiseTags, &h.SocialInstagram, &h.SocialLinkedin, &h.SocialWebsite,
		&h.GalleryURLs, &h.InstagramScrapedAt, &h.AvatarFromInstagram,
		&h.AvgRating, &h.TotalReviews, &h.PlatformFeePercentage, &h.AutoPayoutEnabled,
		&h.CreatedAt, &h.UpdatedAt,
		&h.IsActive,
		&h.EventsHostedOverride, &h.PeopleMetOverride, &h.AvgRatingOverride,
		&h.Slug,
		&h.HostType, &h.SelfieURL, &h.CommunityName, &h.CommunityCategory, &h.YearsExperience, &h.EventsHosted,
		&h.PortfolioURL, &h.PortfolioPhotoURLs, &h.Profession, &h.CertificateURL, &h.DegreeURL, &h.LicenseNumber,
		&h.ProfessionalBody, &h.Specialization, &h.WorkHistory, &h.Languages, &h.Achievements, &h.TestimonialURL,
		&h.RejectionReason,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return h, nil
}

func (r *postgresHostRepository) Create(ctx context.Context, host *models.Host) error {
	unsignPrivateDocs(host)
	query := `
		INSERT INTO hosts (
			id, user_id,
			first_name, last_name, phn_number, city, avatar_url, tagline, bio,
			application_status, experience_desc, moods, description, preferred_days, group_size,
			government_id_url, submitted_at,
			expertise_tags, social_instagram, social_linkedin, social_website,
			is_professional, created_at, updated_at,
			slug,
			host_type, selfie_url, community_name, community_category, years_experience, events_hosted,
	portfolio_url, portfolio_photo_urls, profession, certificate_url, degree_url, license_number,
	professional_body, specialization, work_history, languages, achievements, testimonial_url,
	rejection_reason
		) VALUES (
			$1, $2,
			$3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, $15,
			$16, $17,
			$18, $19, $20, $21,
			$22, $23, $24,
			$25,
			$26, $27, $28, $29, $30, $31,
			$32, $33, $34, $35, $36, $37,
			$38, $39, $40, $41, $42, $43,
			$44
		)
	`
	if host.ID == uuid.Nil {
		host.ID = uuid.New()
	}
	a := host.HostApplicationAnswers.Normalized()
	_, err := r.db.ExecContext(ctx, query,
		host.ID, host.UserID,
		host.FirstName, host.LastName, host.PhnNumber, host.City, host.AvatarURL, host.Tagline, host.Bio,
		host.ApplicationStatus, host.ExperienceDesc, pq.Array(host.Moods), host.Description, pq.Array(host.PreferredDays), host.GroupSize,
		host.GovernmentIDURL, host.SubmittedAt,
		pq.Array(host.ExpertiseTags), host.SocialInstagram, host.SocialLinkedin, host.SocialWebsite,
		host.IsProfessional, host.CreatedAt, host.UpdatedAt,
		host.Slug,
		a.HostType, a.SelfieURL, a.CommunityName, a.CommunityCategory, a.YearsExperience, a.EventsHosted,
		a.PortfolioURL, pq.Array(a.PortfolioPhotoURLs), a.Profession, a.CertificateURL, a.DegreeURL, a.LicenseNumber,
		a.ProfessionalBody, a.Specialization, a.WorkHistory, pq.Array(a.Languages), a.Achievements, a.TestimonialURL,
		host.RejectionReason,
	)
	return err
}

func (r *postgresHostRepository) GetByID(ctx context.Context, id uuid.UUID) (*models.Host, error) {
	query := `SELECT ` + hostColumns + ` FROM hosts WHERE id = $1`
	return scanHost(r.db.QueryRowContext(ctx, query, id))
}

func (r *postgresHostRepository) GetBySlug(ctx context.Context, slug string) (*models.Host, error) {
	query := `SELECT ` + hostColumns + ` FROM hosts WHERE slug = $1`
	return scanHost(r.db.QueryRowContext(ctx, query, slug))
}

func (r *postgresHostRepository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hosts WHERE slug = $1)`, slug).Scan(&exists)
	return exists, err
}

func (r *postgresHostRepository) GetByUserID(ctx context.Context, userID uuid.UUID) (*models.Host, error) {
	query := `SELECT ` + hostColumns + ` FROM hosts WHERE user_id = $1`
	return scanHost(r.db.QueryRowContext(ctx, query, userID))
}

func (r *postgresHostRepository) ListScrapedWithInstagram(ctx context.Context) ([]*models.Host, error) {
	query := `SELECT ` + hostColumns + ` FROM hosts
		WHERE social_instagram IS NOT NULL AND social_instagram <> ''
		  AND instagram_scraped_at IS NOT NULL
		ORDER BY instagram_scraped_at ASC`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []*models.Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func (r *postgresHostRepository) Update(ctx context.Context, host *models.Host) error {
	unsignPrivateDocs(host)
	query := `
		UPDATE hosts SET
			first_name = $1, last_name = $2, phn_number = $3, city = $4, avatar_url = $5, tagline = $6, bio = $7,
			application_status = $8, experience_desc = $9, moods = $10, description = $11, preferred_days = $12, group_size = $13,
			government_id_url = $14, submitted_at = $15, approved_at = $16, rejected_at = $17,
			is_identity_verified = $18, is_super_host = $19, is_community_champ = $20,
			expertise_tags = $21, social_instagram = $22, social_linkedin = $23, social_website = $24,
			avg_rating = $25, total_reviews = $26, platform_fee_percentage = $27,
			avatar_from_instagram = $28, gallery_urls = $29, is_professional = $30,
			events_hosted_override = $31, people_met_override = $32, avg_rating_override = $33,
			host_type = $34, selfie_url = $35, community_name = $36, community_category = $37,
			years_experience = $38, events_hosted = $39, portfolio_url = $40, portfolio_photo_urls = $41,
			profession = $42, certificate_url = $43, degree_url = $44, license_number = $45,
			professional_body = $46, specialization = $47, work_history = $48, languages = $49,
			achievements = $50, testimonial_url = $51, rejection_reason = $52
		WHERE id = $53
	`
	a := host.HostApplicationAnswers.Normalized()
	_, err := r.db.ExecContext(ctx, query,
		host.FirstName, host.LastName, host.PhnNumber, host.City, host.AvatarURL, host.Tagline, host.Bio,
		host.ApplicationStatus, host.ExperienceDesc, pq.Array(host.Moods), host.Description, pq.Array(host.PreferredDays), host.GroupSize,
		host.GovernmentIDURL, host.SubmittedAt, host.ApprovedAt, host.RejectedAt,
		host.IsIdentityVerified, host.IsSuperHost, host.IsCommunityChamp,
		pq.Array(host.ExpertiseTags), host.SocialInstagram, host.SocialLinkedin, host.SocialWebsite,
		host.AvgRating, host.TotalReviews, host.PlatformFeePercentage,
		host.AvatarFromInstagram, pq.Array(host.GalleryURLs), host.IsProfessional,
		host.EventsHostedOverride, host.PeopleMetOverride, host.AvgRatingOverride,
		a.HostType, a.SelfieURL, a.CommunityName, a.CommunityCategory, a.YearsExperience, a.EventsHosted,
		a.PortfolioURL, pq.Array(a.PortfolioPhotoURLs), a.Profession, a.CertificateURL, a.DegreeURL, a.LicenseNumber,
		a.ProfessionalBody, a.Specialization, a.WorkHistory, pq.Array(a.Languages), a.Achievements, a.TestimonialURL,
		host.RejectionReason,
		host.ID,
	)
	return err
}

func (r *postgresHostRepository) UpdateApplicationStatus(ctx context.Context, id uuid.UUID, status models.HostApplicationStatus) error {
	query := `UPDATE hosts SET application_status = $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, status, id)
	return err
}

// SetActive toggles a host's active state (admin deactivate/reactivate). A
// deactivated host is hidden from public listings; all data is preserved.
func (r *postgresHostRepository) SetActive(ctx context.Context, id uuid.UUID, active bool) error {
	query := `UPDATE hosts SET is_active = $1, updated_at = now() WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, active, id)
	return err
}

func (r *postgresHostRepository) ListByStatus(ctx context.Context, status models.HostApplicationStatus) ([]*models.Host, error) {
	query := `SELECT ` + hostColumns + ` FROM hosts WHERE application_status = $1 ORDER BY submitted_at DESC`
	rows, err := r.db.QueryContext(ctx, query, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []*models.Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func (r *postgresHostRepository) UpdateAverageRating(ctx context.Context, hostID uuid.UUID, avgRating float64, totalReviews int) error {
	query := `UPDATE hosts SET avg_rating = $1, total_reviews = $2, updated_at = now() WHERE id = $3`
	_, err := r.db.ExecContext(ctx, query, avgRating, totalReviews, hostID)
	return err
}

func (r *postgresHostRepository) SaveInstagramMedia(ctx context.Context, hostID uuid.UUID, avatarURL *string, galleryURLs []string) error {
	// avatar_url is (re)placed from Instagram only when the host has no avatar
	// yet OR their current avatar itself came from Instagram — a host-uploaded
	// photo (avatar_from_instagram = false) is never overwritten. gallery_urls is
	// replaced only when this call actually fetched posts, so a fetch that returns
	// none (common from datacenter IPs) keeps the existing gallery rather than
	// wiping it. Every CASE arm reads pre-update column values.
	query := `UPDATE hosts SET
		avatar_from_instagram = CASE
			WHEN $1::text IS NOT NULL AND (avatar_url IS NULL OR avatar_url = '' OR avatar_from_instagram)
			THEN true ELSE avatar_from_instagram END,
		avatar_url = CASE
			WHEN $1::text IS NOT NULL AND (avatar_url IS NULL OR avatar_url = '' OR avatar_from_instagram)
			THEN $1::text ELSE avatar_url END,
		gallery_urls = CASE
			WHEN array_length($2::text[], 1) IS NOT NULL THEN $2::text[] ELSE gallery_urls END,
		instagram_scraped_at = now(),
		updated_at = now()
	WHERE id = $3`
	_, err := r.db.ExecContext(ctx, query, avatarURL, pq.Array(galleryURLs), hostID)
	return err
}

func (r *postgresHostRepository) ListNeedingInstagramScrape(ctx context.Context) ([]*models.Host, error) {
	query := `SELECT ` + hostColumns + ` FROM hosts
		WHERE (avatar_url IS NULL OR avatar_url = '')
		  AND social_instagram IS NOT NULL AND social_instagram <> ''
		  AND instagram_scraped_at IS NULL
		ORDER BY created_at ASC`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []*models.Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func (r *postgresHostRepository) SetPlatformFeePercentage(ctx context.Context, hostID uuid.UUID, platformPercentage *int) error {
	query := `UPDATE hosts SET platform_fee_percentage = $1, updated_at = now() WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, platformPercentage, hostID)
	return err
}

func (r *postgresHostRepository) SetAutoPayout(ctx context.Context, hostID uuid.UUID, enabled bool) error {
	query := `UPDATE hosts SET auto_payout_enabled = $1, updated_at = now() WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, enabled, hostID)
	return err
}

func (r *postgresHostRepository) ListAutoPayoutHostIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM hosts WHERE auto_payout_enabled = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// unsignPrivateDocs stores private document URLs without a presign query, in
// case a client echoed back the signed link it was shown.
func unsignPrivateDocs(h *models.Host) {
	h.GovernmentIDURL = models.UnsignURL(h.GovernmentIDURL)
	h.SelfieURL = models.UnsignURL(h.SelfieURL)
	h.CertificateURL = models.UnsignURL(h.CertificateURL)
	h.DegreeURL = models.UnsignURL(h.DegreeURL)
}
