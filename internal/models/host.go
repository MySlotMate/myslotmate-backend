package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Host is a verified user who can create events, see analytics, and manage payouts.
type Host struct {
	ID        uuid.UUID  `db:"id" json:"id"`
	UserID    uuid.UUID  `db:"user_id" json:"user_id"`
	AccountID *uuid.UUID `db:"account_id" json:"account_id,omitempty"`

	// Slug is the clean, URL-safe identifier used in public links
	// (/host/{slug}). Generated once at create time; immutable.
	Slug string `db:"slug" json:"slug"`

	// ── Personal / Profile ──────────────────────────────────────────────────
	FirstName string  `db:"first_name" json:"first_name"`
	LastName  string  `db:"last_name" json:"last_name"`
	PhnNumber string  `db:"phn_number" json:"phn_number"`
	City      string  `db:"city" json:"city"`
	AvatarURL *string `db:"avatar_url" json:"avatar_url,omitempty"`
	Tagline   *string `db:"tagline" json:"tagline,omitempty"`
	Bio       *string `db:"bio" json:"bio,omitempty"`

	// ── Host Application (Become a Host flow) ───────────────────────────────
	ApplicationStatus HostApplicationStatus `db:"application_status" json:"application_status"`
	ExperienceDesc    *string               `db:"experience_desc" json:"experience_desc,omitempty"`     // "What Experiences will you Host?"
	Moods             pq.StringArray        `db:"moods" json:"moods"`                                   // ["adventure","social","wellness"]
	Description       *string               `db:"description" json:"description,omitempty"`             // 300 char description
	PreferredDays     pq.StringArray        `db:"preferred_days" json:"preferred_days"`                 // ["mon","tue","wed"]
	GroupSize         *int                  `db:"group_size" json:"group_size,omitempty"`               // approximate group size
	GovernmentIDURL   *string               `db:"government_id_url" json:"government_id_url,omitempty"` // uploaded ID doc URL
	SubmittedAt       *time.Time            `db:"submitted_at" json:"submitted_at,omitempty"`
	ApprovedAt        *time.Time            `db:"approved_at" json:"approved_at,omitempty"`
	RejectedAt        *time.Time            `db:"rejected_at" json:"rejected_at,omitempty"`
	RejectionReason   *string               `db:"rejection_reason" json:"rejection_reason,omitempty"`

	// Two-path application answers (host_type, community / professional fields).
	HostApplicationAnswers

	// ── Trust & Safety badges ───────────────────────────────────────────────
	IsIdentityVerified bool `db:"is_identity_verified" json:"is_identity_verified"`
	IsSuperHost        bool `db:"is_super_host" json:"is_super_host"`
	IsCommunityChamp   bool `db:"is_community_champ" json:"is_community_champ"`
	IsProfessional     bool `db:"is_professional" json:"is_professional"`
	// IsActive false = deactivated by admin: hidden from public listings, data kept.
	IsActive bool `db:"is_active" json:"is_active"`

	// ── Expertise & Social ──────────────────────────────────────────────────
	ExpertiseTags   pq.StringArray `db:"expertise_tags" json:"expertise_tags"` // ["#Minimalism","#Wellness"]
	SocialInstagram *string        `db:"social_instagram" json:"social_instagram,omitempty"`
	SocialLinkedin  *string        `db:"social_linkedin" json:"social_linkedin,omitempty"`
	SocialWebsite   *string        `db:"social_website" json:"social_website,omitempty"`

	// ── Instagram media (one-time scrape on application submit) ────────────
	GalleryURLs         pq.StringArray `db:"gallery_urls" json:"gallery_urls"` // up to 4 recent IG post photos, re-hosted on S3
	InstagramScrapedAt  *time.Time     `db:"instagram_scraped_at" json:"instagram_scraped_at,omitempty"`
	AvatarFromInstagram bool           `db:"avatar_from_instagram" json:"avatar_from_instagram"` // avatar_url came from the IG scrape

	// ── Aggregate stats (denormalized for dashboard) ────────────────────────
	AvgRating    *float64 `db:"avg_rating" json:"avg_rating,omitempty"`
	TotalReviews int      `db:"total_reviews" json:"total_reviews"`

	// ── Admin stat overrides ────────────────────────────────────────────────
	// The three headline numbers on the public profile are normally derived
	// (events hosted / people met from the host's events; AvgRating from
	// reviews). A non-nil override here pins the displayed value instead; nil
	// means "show the derived number". AvgRatingOverride is separate from
	// AvgRating because review_service overwrites AvgRating on every review.
	EventsHostedOverride *int     `db:"events_hosted_override" json:"events_hosted_override,omitempty"`
	PeopleMetOverride    *int     `db:"people_met_override" json:"people_met_override,omitempty"`
	AvgRatingOverride    *float64 `db:"avg_rating_override" json:"avg_rating_override,omitempty"`

	// PlatformFeePercentage is this host's commission override — the
	// platform's cut of each booking (host keeps the remainder). nil means
	// "use the global platform_settings default" (see GetPlatformFeeConfig).
	PlatformFeePercentage *int `db:"platform_fee_percentage" json:"platform_fee_percentage,omitempty"`

	// AutoPayoutEnabled: admin opted this host into the weekly Tuesday payout
	// of their full withdrawable balance (verified bank account required).
	AutoPayoutEnabled bool `db:"auto_payout_enabled" json:"auto_payout_enabled"`

	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// Host types: the two paths of the /become-host form.
const (
	HostTypeCommunity    = "community"
	HostTypeProfessional = "professional"
)

// HostApplicationAnswers holds the two-path application answers (frontend
// spec docs/backend-spec-host-application.md §2). It is embedded in both
// Host and the apply request, so one assignment copies every answer and
// clears the other path's answers when an applicant switches paths.
//
// SelfieURL, CertificateURL and DegreeURL (like Host.GovernmentIDURL) are
// private documents: only the host and admins may see them, never a public
// host DTO.
type HostApplicationAnswers struct {
	HostType  string  `db:"host_type" json:"host_type"`
	SelfieURL *string `db:"selfie_url" json:"selfie_url,omitempty"`

	// Community path
	CommunityName      *string        `db:"community_name" json:"community_name,omitempty"`
	CommunityCategory  *string        `db:"community_category" json:"community_category,omitempty"`
	YearsExperience    *string        `db:"years_experience" json:"years_experience,omitempty"`
	EventsHosted       *string        `db:"events_hosted" json:"events_hosted,omitempty"`
	PortfolioURL       *string        `db:"portfolio_url" json:"portfolio_url,omitempty"`
	PortfolioPhotoURLs pq.StringArray `db:"portfolio_photo_urls" json:"portfolio_photo_urls"`

	// Professional path
	Profession       *string        `db:"profession" json:"profession,omitempty"`
	CertificateURL   *string        `db:"certificate_url" json:"certificate_url,omitempty"`
	DegreeURL        *string        `db:"degree_url" json:"degree_url,omitempty"`
	LicenseNumber    *string        `db:"license_number" json:"license_number,omitempty"`
	ProfessionalBody *string        `db:"professional_body" json:"professional_body,omitempty"`
	Specialization   *string        `db:"specialization" json:"specialization,omitempty"`
	WorkHistory      *string        `db:"work_history" json:"work_history,omitempty"`
	Languages        pq.StringArray `db:"languages" json:"languages"`
	Achievements     *string        `db:"achievements" json:"achievements,omitempty"`
	TestimonialURL   *string        `db:"testimonial_url" json:"testimonial_url,omitempty"`
}

// Normalized fills defaults the columns require: host_type defaults to
// community (old app builds don't send it) and the arrays are never NULL.
func (a HostApplicationAnswers) Normalized() HostApplicationAnswers {
	if a.HostType == "" {
		a.HostType = HostTypeCommunity
	}
	if a.PortfolioPhotoURLs == nil {
		a.PortfolioPhotoURLs = pq.StringArray{}
	}
	if a.Languages == nil {
		a.Languages = pq.StringArray{}
	}
	return a
}
