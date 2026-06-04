package controller

import (
	"math"
	"net/http"
	"strconv"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	defaultPageSize = 10
	maxPageSize     = 100
)

// AdminDirectoryController serves the admin dashboard's Users and Hosts tabs
// with real, aggregated data. All routes require a valid admin session token.
type AdminDirectoryController struct {
	repo      *repository.AdminDirectoryRepository
	hostRepo  repository.HostRepository
	userRepo  repository.UserRepository
	jwtSecret string
}

func NewAdminDirectoryController(
	repo *repository.AdminDirectoryRepository,
	hostRepo repository.HostRepository,
	userRepo repository.UserRepository,
	jwtSecret string,
) *AdminDirectoryController {
	return &AdminDirectoryController{repo: repo, hostRepo: hostRepo, userRepo: userRepo, jwtSecret: jwtSecret}
}

func (c *AdminDirectoryController) RegisterRoutes(r chi.Router) {
	r.Route("/admin/directory", func(r chi.Router) {
		r.Use(auth.RequireAdminToken(c.jwtSecret))
		r.Get("/users", c.ListUsers)
		r.Get("/hosts", c.ListHosts)
		r.Get("/hosts/{hostID}", c.GetHost)
		r.Get("/events", c.ListEvents)
	})
}

// ── Pagination ───────────────────────────────────────────────────────────────

// paginatedResponse is the envelope returned for paged list endpoints.
type paginatedResponse struct {
	Items    interface{} `json:"items"`
	Total    int         `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
}

// parsePagination reads ?page (1-based) and ?page_size, applying sane defaults
// and bounds. Returns the page, pageSize, and the derived offset.
func parsePagination(r *http.Request) (page, pageSize, offset int) {
	page = 1
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}

	pageSize = defaultPageSize
	if v, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && v > 0 {
		pageSize = v
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	offset = (page - 1) * pageSize
	return page, pageSize, offset
}

// ── Response shapes (mirror the admin dashboard's TS types) ──────────────────

type adminUserDTO struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	City          string `json:"city"`
	TotalBookings int64  `json:"totalBookings"`
	TotalSpent    int64  `json:"totalSpent"`
	JoinDate      string `json:"joinDate"`
	Status        string `json:"status"` // Active | Watchlist | Suspended | VIP
}

// adminHostDetailDTO is the full host profile returned for the detail page.
type adminHostDetailDTO struct {
	Host  *models.Host       `json:"host"`
	User  *adminHostUserDTO  `json:"user"`
	Stats adminHostStatsDTO  `json:"stats"`
}

type adminHostUserDTO struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	City       string `json:"city"`
	IsVerified bool   `json:"isVerified"`
}

type adminHostStatsDTO struct {
	ExperiencesCreated int64 `json:"experiencesCreated"`
	BookingsGenerated  int64 `json:"bookingsGenerated"`
	RevenueGenerated   int64 `json:"revenueGenerated"`
}

type adminEventDTO struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	HostName string  `json:"hostName"`
	City     string  `json:"city"`
	Category string  `json:"category"`
	Price    int64   `json:"price"`
	IsFree   bool    `json:"isFree"`
	Bookings int64   `json:"bookings"`
	Rating   float64 `json:"rating"`
	Status   string  `json:"status"` // draft | live | paused | cancelled
}

type adminHostDTO struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	City               string  `json:"city"`
	SocialFollowers    string  `json:"socialFollowers"`
	ExperiencesCreated int64   `json:"experiencesCreated"`
	BookingsGenerated  int64   `json:"bookingsGenerated"`
	AverageRating      float64 `json:"averageRating"`
	RevenueGenerated   int64   `json:"revenueGenerated"`
	VerificationStatus string  `json:"verificationStatus"` // Verified | Pending review | Re-verification | Suspended
	ApplicationStatus  string  `json:"applicationStatus"`  // raw: draft | pending | under_review | approved | rejected
}

// ── Handlers ─────────────────────────────────────────────────────────────────

func (c *AdminDirectoryController) ListUsers(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset := parsePagination(r)
	q := r.URL.Query()

	rows, total, err := c.repo.ListUsers(r.Context(), repository.ListUsersParams{
		Limit:  pageSize,
		Offset: offset,
		Search: q.Get("search"),
		City:   q.Get("city"),
		Tier:   q.Get("tier"),
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	users := make([]adminUserDTO, 0, len(rows))
	for _, u := range rows {
		city := u.City
		if city == "" {
			city = "—"
		}
		users = append(users, adminUserDTO{
			ID:            u.ID.String(),
			Name:          u.Name,
			Email:         u.Email,
			City:          city,
			TotalBookings: u.TotalBookings,
			TotalSpent:    centsToMajor(u.TotalSpentCents),
			JoinDate:      u.CreatedAt.Format("02 Jan 2006"),
			Status:        userStatus(u.IsVerified),
		})
	}

	RespondSuccess(w, http.StatusOK, paginatedResponse{
		Items:    users,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

func (c *AdminDirectoryController) ListHosts(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset := parsePagination(r)

	rows, total, err := c.repo.ListHosts(r.Context(), repository.ListHostsParams{
		Limit:  pageSize,
		Offset: offset,
		Search: r.URL.Query().Get("search"),
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	hosts := make([]adminHostDTO, 0, len(rows))
	for _, h := range rows {
		name := h.FirstName
		if h.LastName != "" {
			if name != "" {
				name += " "
			}
			name += h.LastName
		}
		if name == "" {
			name = "Unnamed host"
		}

		rating := 0.0
		if h.AvgRating.Valid {
			rating = math.Round(h.AvgRating.Float64*100) / 100
		}

		hosts = append(hosts, adminHostDTO{
			ID:                 h.ID.String(),
			Name:               name,
			City:               h.City,
			SocialFollowers:    hostFollowers(h.SocialInstagram.String),
			ExperiencesCreated: h.ExperiencesCreated,
			BookingsGenerated:  h.BookingsGenerated,
			AverageRating:      rating,
			RevenueGenerated:   centsToMajor(h.RevenueCents),
			VerificationStatus: hostVerificationStatus(h.ApplicationStatus),
			ApplicationStatus:  h.ApplicationStatus,
		})
	}

	RespondSuccess(w, http.StatusOK, paginatedResponse{
		Items:    hosts,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// GetHost returns a single host's full profile, linked user contact, and stats.
func (c *AdminDirectoryController) GetHost(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}

	host, err := c.hostRepo.GetByID(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if host == nil {
		RespondError(w, http.StatusNotFound, "Host not found")
		return
	}

	var userPayload *adminHostUserDTO
	if u, uerr := c.userRepo.GetByID(r.Context(), host.UserID); uerr == nil && u != nil {
		city := ""
		if u.City != nil {
			city = *u.City
		}
		userPayload = &adminHostUserDTO{
			Name:       u.Name,
			Email:      u.Email,
			Phone:      u.PhnNumber,
			City:       city,
			IsVerified: u.IsVerified,
		}
	}

	stats, err := c.repo.GetHostAggregates(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, adminHostDetailDTO{
		Host: host,
		User: userPayload,
		Stats: adminHostStatsDTO{
			ExperiencesCreated: stats.ExperiencesCreated,
			BookingsGenerated:  stats.BookingsGenerated,
			RevenueGenerated:   centsToMajor(stats.RevenueCents),
		},
	})
}

// ListEvents returns a page of all events (any status) with host name + city.
func (c *AdminDirectoryController) ListEvents(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset := parsePagination(r)
	q := r.URL.Query()

	rows, total, err := c.repo.ListEvents(r.Context(), repository.ListEventsParams{
		Limit:  pageSize,
		Offset: offset,
		Search: q.Get("search"),
		Status: q.Get("status"),
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	events := make([]adminEventDTO, 0, len(rows))
	for _, e := range rows {
		hostName := joinName(e.HostFirstName.String, e.HostLastName.String)
		if hostName == "" {
			hostName = "Unknown host"
		}
		city := e.HostCity.String
		if city == "" {
			city = "—"
		}
		category := e.Mood.String
		if category == "" {
			category = "—"
		}
		var price int64
		if !e.IsFree && e.PriceCents.Valid {
			price = centsToMajor(e.PriceCents.Int64)
		}
		rating := 0.0
		if e.AvgRating.Valid {
			rating = math.Round(e.AvgRating.Float64*100) / 100
		}

		events = append(events, adminEventDTO{
			ID:       e.ID.String(),
			Title:    e.Title,
			HostName: hostName,
			City:     city,
			Category: category,
			Price:    price,
			IsFree:   e.IsFree,
			Bookings: e.TotalBookings,
			Rating:   rating,
			Status:   e.Status,
		})
	}

	RespondSuccess(w, http.StatusOK, paginatedResponse{
		Items:    events,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// ── Mapping helpers ──────────────────────────────────────────────────────────

// joinName combines first and last name, trimming extra space.
func joinName(first, last string) string {
	name := first
	if last != "" {
		if name != "" {
			name += " "
		}
		name += last
	}
	return name
}

// centsToMajor converts integer cents to whole major-currency units for display.
func centsToMajor(cents int64) int64 {
	return int64(math.Round(float64(cents) / 100.0))
}

// userStatus maps the (currently limited) user model to the dashboard's status
// vocabulary. Suspended/VIP are not yet modelled in the DB, so unverified users
// surface as "Watchlist" and verified users as "Active".
func userStatus(isVerified bool) string {
	if isVerified {
		return "Active"
	}
	return "Watchlist"
}

// hostFollowers shows the connected Instagram handle when present; follower
// counts are not stored in the DB yet.
func hostFollowers(instagram string) string {
	if instagram != "" {
		return instagram
	}
	return "—"
}

// hostVerificationStatus maps the host application lifecycle to the dashboard's
// verification vocabulary.
func hostVerificationStatus(applicationStatus string) string {
	switch applicationStatus {
	case "approved":
		return "Verified"
	case "under_review":
		return "Re-verification"
	case "rejected":
		return "Suspended"
	default: // pending / submitted
		return "Pending review"
	}
}
