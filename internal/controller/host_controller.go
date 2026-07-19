package controller

import (
	"encoding/json"
	"fmt"
	"net/http"

	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type HostController struct {
	hostService service.HostService
}

func NewHostController(s service.HostService) *HostController {
	return &HostController{hostService: s}
}

// PublicHostProfile is the public-facing view of a host, omitting sensitive fields.
type PublicHostProfile struct {
	ID                  uuid.UUID `json:"id"`
	FirstName           string    `json:"first_name"`
	LastName            string    `json:"last_name"`
	City                string    `json:"city"`
	AvatarURL           *string   `json:"avatar_url,omitempty"`
	AvatarFromInstagram bool      `json:"avatar_from_instagram"`
	Tagline             *string   `json:"tagline,omitempty"`
	Bio                 *string   `json:"bio,omitempty"`
	IsIdentityVerified  bool      `json:"is_identity_verified"`
	IsSuperHost         bool      `json:"is_super_host"`
	IsCommunityChamp    bool      `json:"is_community_champ"`
	IsProfessional      bool      `json:"is_professional"`
	ExpertiseTags       []string  `json:"expertise_tags"`
	SocialInstagram     *string   `json:"social_instagram,omitempty"`
	SocialLinkedin      *string   `json:"social_linkedin,omitempty"`
	SocialWebsite       *string   `json:"social_website,omitempty"`
	GalleryURLs         []string  `json:"gallery_urls"`
	AvgRating           *float64  `json:"avg_rating,omitempty"`
	TotalReviews        int       `json:"total_reviews"`

	// Admin-set overrides for the three headline profile stats. Non-nil means
	// the client should display these instead of the derived values.
	EventsHostedOverride *int     `json:"events_hosted_override,omitempty"`
	PeopleMetOverride    *int     `json:"people_met_override,omitempty"`
	AvgRatingOverride    *float64 `json:"avg_rating_override,omitempty"`
}

func (c *HostController) RegisterRoutes(r chi.Router) {
	r.Route("/hosts", func(r chi.Router) {
		r.Get("/", c.ListHosts)
		r.Get("/{hostID}", c.GetPublicHostProfile)
		r.Post("/apply", c.SubmitApplication)
		r.Post("/apply/draft", c.SaveDraft)
		r.Get("/application-status", c.GetApplicationStatus)
		r.Get("/me", c.GetMyHostProfile)
		r.Put("/me", c.UpdateProfile)
		r.Put("/me/social", c.ConnectSocial)
		r.Delete("/me/social/{platform}", c.DisconnectSocial)
		r.Get("/dashboard", c.GetDashboardOverview)
		r.Get("/attention-items", c.GetAttentionItems)
		r.Get("/earnings/breakdown", c.GetEarningsBreakdown)
	})
}

// ── Request types ───────────────────────────────────────────────────────────

type HostApplicationRequestBody struct {
	UserID          uuid.UUID `json:"user_id"`
	FirstName       string    `json:"first_name"`
	LastName        string    `json:"last_name"`
	City            string    `json:"city"`
	PhnNumber       string    `json:"phn_number"`
	ExperienceDesc  *string   `json:"experience_desc,omitempty"`
	Moods           []string  `json:"moods"`
	Description     *string   `json:"description,omitempty"`
	PreferredDays   []string  `json:"preferred_days"`
	GroupSize       *int      `json:"group_size,omitempty"`
	GovernmentIDURL *string   `json:"government_id_url,omitempty"`
	AvatarURL       *string   `json:"avatar_url,omitempty"`
	Tagline         *string   `json:"tagline,omitempty"`
	Bio             *string   `json:"bio,omitempty"`
	SocialInstagram *string   `json:"social_instagram,omitempty"`
	SocialLinkedin  *string   `json:"social_linkedin,omitempty"`
	SocialWebsite   *string   `json:"social_website,omitempty"`
	IsProfessional  *bool     `json:"is_professional,omitempty"`
}

type HostProfileUpdateRequestBody struct {
	Tagline         *string  `json:"tagline,omitempty"`
	Bio             *string  `json:"bio,omitempty"`
	AvatarURL       *string  `json:"avatar_url,omitempty"`
	City            *string  `json:"city,omitempty"`
	ExpertiseTags   []string `json:"expertise_tags,omitempty"`
	SocialInstagram *string  `json:"social_instagram,omitempty"`
	SocialLinkedin  *string  `json:"social_linkedin,omitempty"`
	SocialWebsite   *string  `json:"social_website,omitempty"`
}

func (c *HostController) resolveHostIDFromRequest(r *http.Request) (uuid.UUID, int, string) {
	hostIDStr := r.URL.Query().Get("host_id")
	fmt.Printf("[RESOLVE_HOST] Query params: host_id=%s\n", hostIDStr)

	if hostIDStr != "" {
		hostID, err := uuid.Parse(hostIDStr)
		if err != nil {
			fmt.Printf("[RESOLVE_HOST] Invalid host_id: %v\n", err)
			return uuid.Nil, http.StatusBadRequest, "Invalid host_id"
		}
		fmt.Printf("[RESOLVE_HOST] Resolved via host_id: %s\n", hostID)
		return hostID, 0, ""
	}

	userIDStr := r.URL.Query().Get("user_id")
	fmt.Printf("[RESOLVE_HOST] user_id=%s\n", userIDStr)

	if userIDStr == "" {
		fmt.Printf("[RESOLVE_HOST] ERROR: Missing both host_id and user_id\n")
		return uuid.Nil, http.StatusBadRequest, "Missing host_id or user_id"
	}

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		fmt.Printf("[RESOLVE_HOST] ERROR: Invalid user_id: %v\n", err)
		return uuid.Nil, http.StatusBadRequest, "Invalid user_id"
	}

	fmt.Printf("[RESOLVE_HOST] Looking up host for user_id: %s\n", userID)
	host, err := c.hostService.GetHostByUserID(r.Context(), userID)
	if err != nil {
		fmt.Printf("[RESOLVE_HOST] ERROR: GetHostByUserID error: %v\n", err)
		return uuid.Nil, http.StatusInternalServerError, err.Error()
	}
	if host == nil {
		fmt.Printf("[RESOLVE_HOST] ERROR: No host profile found for user_id: %s\n", userID)
		return uuid.Nil, http.StatusNotFound, "Host profile not found"
	}

	fmt.Printf("[RESOLVE_HOST] Resolved via user_id: host_id=%s (user_id=%s)\n", host.ID, userID)
	return host.ID, 0, ""
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (c *HostController) ListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := c.hostService.ListApprovedHosts(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	profiles := make([]PublicHostProfile, 0, len(hosts))
	for _, h := range hosts {
		profiles = append(profiles, PublicHostProfile{
			ID:                  h.ID,
			FirstName:           h.FirstName,
			LastName:            h.LastName,
			City:                h.City,
			AvatarURL:           h.AvatarURL,
			AvatarFromInstagram: h.AvatarFromInstagram,
			Tagline:             h.Tagline,
			Bio:                 h.Bio,
			IsIdentityVerified:  h.IsIdentityVerified,
			IsSuperHost:         h.IsSuperHost,
			IsCommunityChamp:    h.IsCommunityChamp,
			IsProfessional:      h.IsProfessional,
			ExpertiseTags:       h.ExpertiseTags,
			SocialInstagram:     h.SocialInstagram,
			SocialLinkedin:      h.SocialLinkedin,
			SocialWebsite:       h.SocialWebsite,
			GalleryURLs:         h.GalleryURLs,
			AvgRating:           h.AvgRating,
			TotalReviews:        h.TotalReviews,

			EventsHostedOverride: h.EventsHostedOverride,
			PeopleMetOverride:    h.PeopleMetOverride,
			AvgRatingOverride:    h.AvgRatingOverride,
		})
	}

	RespondSuccess(w, http.StatusOK, profiles)
}

func (c *HostController) GetPublicHostProfile(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	host, err := c.hostService.GetHostByID(r.Context(), hostID)
	if err != nil {
		if err.Error() == "host not found" {
			RespondError(w, http.StatusNotFound, "Host not found")
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	profile := PublicHostProfile{
		ID:                  host.ID,
		FirstName:           host.FirstName,
		LastName:            host.LastName,
		City:                host.City,
		AvatarURL:           host.AvatarURL,
		AvatarFromInstagram: host.AvatarFromInstagram,
		Tagline:             host.Tagline,
		Bio:                 host.Bio,
		IsIdentityVerified:  host.IsIdentityVerified,
		IsSuperHost:         host.IsSuperHost,
		IsCommunityChamp:    host.IsCommunityChamp,
		IsProfessional:      host.IsProfessional,
		ExpertiseTags:       host.ExpertiseTags,
		SocialInstagram:     host.SocialInstagram,
		SocialLinkedin:      host.SocialLinkedin,
		SocialWebsite:       host.SocialWebsite,
		GalleryURLs:         host.GalleryURLs,
		AvgRating:           host.AvgRating,
		TotalReviews:        host.TotalReviews,

		EventsHostedOverride: host.EventsHostedOverride,
		PeopleMetOverride:    host.PeopleMetOverride,
		AvgRatingOverride:    host.AvgRatingOverride,
	}

	RespondSuccess(w, http.StatusOK, profile)
}

// toHostApplicationRequest maps the wire body onto the service request. UserID
// is not part of it — the service takes it separately. PhnNumber is read from
// the user record by the service, so it is ignored here.
func toHostApplicationRequest(req HostApplicationRequestBody) service.HostApplicationRequest {
	return service.HostApplicationRequest{
		FirstName:       req.FirstName,
		LastName:        req.LastName,
		City:            req.City,
		ExperienceDesc:  req.ExperienceDesc,
		Moods:           req.Moods,
		Description:     req.Description,
		PreferredDays:   req.PreferredDays,
		GroupSize:       req.GroupSize,
		GovernmentIDURL: req.GovernmentIDURL,
		AvatarURL:       req.AvatarURL,
		Tagline:         req.Tagline,
		Bio:             req.Bio,
		SocialInstagram: req.SocialInstagram,
		SocialLinkedin:  req.SocialLinkedin,
		SocialWebsite:   req.SocialWebsite,
		IsProfessional:  req.IsProfessional,
	}
}

func (c *HostController) SubmitApplication(w http.ResponseWriter, r *http.Request) {
	var req HostApplicationRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	host, err := c.hostService.SubmitApplication(r.Context(), req.UserID, toHostApplicationRequest(req))
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, host)
}

func (c *HostController) SaveDraft(w http.ResponseWriter, r *http.Request) {
	var req HostApplicationRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	host, err := c.hostService.SaveDraft(r.Context(), req.UserID, toHostApplicationRequest(req))
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, host)
}

func (c *HostController) GetApplicationStatus(w http.ResponseWriter, r *http.Request) {
	userIDStr := r.URL.Query().Get("user_id")
	if userIDStr == "" {
		RespondError(w, http.StatusBadRequest, "Missing user_id")
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid user_id")
		return
	}

	status, err := c.hostService.GetApplicationStatus(r.Context(), userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]interface{}{
		"status": status,
	})
}

func (c *HostController) GetMyHostProfile(w http.ResponseWriter, r *http.Request) {
	userIDStr := r.URL.Query().Get("user_id")
	if userIDStr == "" {
		RespondError(w, http.StatusBadRequest, "Missing user_id")
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid user_id")
		return
	}

	host, err := c.hostService.GetHostByUserID(r.Context(), userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if host == nil {
		RespondError(w, http.StatusNotFound, "Host profile not found")
		return
	}

	RespondSuccess(w, http.StatusOK, host)
}

func (c *HostController) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	var req HostProfileUpdateRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	hostID, statusCode, message := c.resolveHostIDFromRequest(r)
	if statusCode != 0 {
		RespondError(w, statusCode, message)
		return
	}

	svcReq := service.HostProfileUpdateRequest{
		Tagline:         req.Tagline,
		Bio:             req.Bio,
		AvatarURL:       req.AvatarURL,
		ExpertiseTags:   req.ExpertiseTags,
		SocialInstagram: req.SocialInstagram,
		SocialLinkedin:  req.SocialLinkedin,
		SocialWebsite:   req.SocialWebsite,
	}

	host, err := c.hostService.UpdateProfile(r.Context(), hostID, svcReq)
	if err != nil {
		if err.Error() == "host not found" {
			RespondError(w, http.StatusNotFound, "Host profile not found")
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, host)
}

func (c *HostController) GetDashboardOverview(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("[DASHBOARD_CTRL] GetDashboardOverview called\n")

	hostID, statusCode, message := c.resolveHostIDFromRequest(r)
	if statusCode != 0 {
		fmt.Printf("[DASHBOARD_CTRL] Resolution error: %s\n", message)
		RespondError(w, statusCode, message)
		return
	}

	fmt.Printf("[DASHBOARD_CTRL] Calling hostService.GetDashboardOverview with hostID: %s\n", hostID)
	overview, err := c.hostService.GetDashboardOverview(r.Context(), hostID)
	if err != nil {
		fmt.Printf("[DASHBOARD_CTRL] ERROR: %v\n", err)
		if err.Error() == "host not found" {
			RespondError(w, http.StatusNotFound, "Host profile not found")
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	fmt.Printf("[DASHBOARD_CTRL] SUCCESS: Overview - Events: %d, Bookings: %d\n",
		overview.TotalEvents, overview.TotalBookings)
	RespondSuccess(w, http.StatusOK, overview)
}

// ── Social Connect/Disconnect ───────────────────────────────────────────────

type SocialConnectRequestBody struct {
	UserID   uuid.UUID `json:"user_id"`
	Platform string    `json:"platform"` // "instagram", "youtube", "twitter", "linkedin", "website"
	URL      string    `json:"url"`
}

func (c *HostController) ConnectSocial(w http.ResponseWriter, r *http.Request) {
	var req SocialConnectRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	// Resolve host ID from user ID
	host, err := c.hostService.GetHostByUserID(r.Context(), req.UserID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if host == nil {
		RespondError(w, http.StatusNotFound, "Host profile not found")
		return
	}

	svcReq := service.SocialConnectRequest{
		Platform: req.Platform,
		URL:      req.URL,
	}

	updated, err := c.hostService.ConnectSocial(r.Context(), host.ID, svcReq)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, updated)
}

func (c *HostController) DisconnectSocial(w http.ResponseWriter, r *http.Request) {
	platform := chi.URLParam(r, "platform")
	if platform == "" {
		RespondError(w, http.StatusBadRequest, "Missing platform")
		return
	}

	hostID, statusCode, message := c.resolveHostIDFromRequest(r)
	if statusCode != 0 {
		RespondError(w, statusCode, message)
		return
	}

	updated, err := c.hostService.DisconnectSocial(r.Context(), hostID, platform)
	if err != nil {
		if err.Error() == "host not found" {
			RespondError(w, http.StatusNotFound, "Host profile not found")
			return
		}
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, updated)
}

// ── Attention Items ─────────────────────────────────────────────────────────

func (c *HostController) GetAttentionItems(w http.ResponseWriter, r *http.Request) {
	hostID, statusCode, message := c.resolveHostIDFromRequest(r)
	if statusCode != 0 {
		RespondError(w, statusCode, message)
		return
	}

	items, err := c.hostService.GetAttentionItems(r.Context(), hostID)
	if err != nil {
		if err.Error() == "host not found" {
			RespondError(w, http.StatusNotFound, "Host profile not found")
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, items)
}

// ── Earnings Breakdown ──────────────────────────────────────────────────────

func (c *HostController) GetEarningsBreakdown(w http.ResponseWriter, r *http.Request) {
	hostID, statusCode, message := c.resolveHostIDFromRequest(r)
	if statusCode != 0 {
		RespondError(w, statusCode, message)
		return
	}

	breakdown, err := c.hostService.GetEarningsBreakdown(r.Context(), hostID)
	if err != nil {
		if err.Error() == "host not found" {
			RespondError(w, http.StatusNotFound, "Host profile not found")
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, breakdown)
}
