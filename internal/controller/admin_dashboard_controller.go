package controller

import (
	"net/http"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/repository"

	"github.com/go-chi/chi/v5"
)

// AdminDashboardController serves the admin dashboard's summary counts in a
// single dedicated endpoint.
type AdminDashboardController struct {
	repo      *repository.AdminDirectoryRepository
	jwtSecret string
}

func NewAdminDashboardController(repo *repository.AdminDirectoryRepository, jwtSecret string) *AdminDashboardController {
	return &AdminDashboardController{repo: repo, jwtSecret: jwtSecret}
}

func (c *AdminDashboardController) RegisterRoutes(r chi.Router) {
	r.With(auth.RequireAdminToken(c.jwtSecret)).Get("/admin/dashboard/stats", c.GetStats)
}

// ── Response shape (mirrors the admin dashboard's TS DashboardStats) ──────────

type dashboardMonthlyDTO struct {
	Month string `json:"month"`
	Count int64  `json:"count"`
}

type dashboardRecentBookingDTO struct {
	ID         string `json:"id"`
	User       string `json:"user"`
	Experience string `json:"experience"`
	Amount     int64  `json:"amount"`
	Date       string `json:"date"`
	Status     string `json:"status"`
}

type dashboardAttentionDTO struct {
	PendingHosts    int64 `json:"pendingHosts"`
	ReviewEvents    int64 `json:"reviewEvents"`
	RefundsToReview int64 `json:"refundsToReview"`
}

type dashboardTopHostDTO struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	City    string `json:"city"`
	Revenue int64  `json:"revenue"`
}

type dashboardTopExperienceDTO struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	HostName string `json:"hostName"`
	Bookings int64  `json:"bookings"`
}

type dashboardStatsDTO struct {
	TotalEvents     int64                       `json:"totalEvents"`
	TotalHosts      int64                       `json:"totalHosts"`
	TotalBookings   int64                       `json:"totalBookings"`
	TotalRevenue    int64                       `json:"totalRevenue"`
	PlatformIncome  int64                       `json:"platformIncome"`
	MonthlyBookings []dashboardMonthlyDTO       `json:"monthlyBookings"`
	RecentBookings  []dashboardRecentBookingDTO `json:"recentBookings"`
	Attention       dashboardAttentionDTO       `json:"attention"`
	TopHosts        []dashboardTopHostDTO       `json:"topHosts"`
	TopExperiences  []dashboardTopExperienceDTO `json:"topExperiences"`
}

func (c *AdminDashboardController) GetStats(w http.ResponseWriter, r *http.Request) {
	d, err := c.repo.GetDashboardStats(r.Context())
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Recent bookings — reuse the bookings list (newest first, top 5).
	recentRows, _, err := c.repo.ListBookings(r.Context(), repository.ListBookingsParams{Limit: 5, Offset: 0})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	monthly := make([]dashboardMonthlyDTO, 0, len(d.MonthlyBookings))
	for _, m := range d.MonthlyBookings {
		monthly = append(monthly, dashboardMonthlyDTO{Month: m.Month, Count: m.Count})
	}

	recent := make([]dashboardRecentBookingDTO, 0, len(recentRows))
	for _, b := range recentRows {
		guest := b.UserName.String
		if guest == "" {
			guest = "Unknown guest"
		}
		experience := b.EventTitle.String
		if experience == "" {
			experience = "—"
		}
		date := b.CreatedAt
		if b.OccurrenceDate.Valid {
			date = b.OccurrenceDate.Time
		}
		var amount int64
		if b.AmountCents.Valid {
			amount = centsToMajor(b.AmountCents.Int64)
		}
		recent = append(recent, dashboardRecentBookingDTO{
			ID:         b.ID.String(),
			User:       guest,
			Experience: experience,
			Amount:     amount,
			Date:       date.Format("02 Jan 2006"),
			Status:     bookingStatusLabel(b.Status),
		})
	}

	topHosts := make([]dashboardTopHostDTO, 0, len(d.TopHosts))
	for _, h := range d.TopHosts {
		name := joinName(h.FirstName, h.LastName)
		if name == "" {
			name = "Unnamed host"
		}
		city := h.City
		if city == "" {
			city = "—"
		}
		topHosts = append(topHosts, dashboardTopHostDTO{
			ID:      h.ID.String(),
			Name:    name,
			City:    city,
			Revenue: centsToMajor(h.RevenueCents),
		})
	}

	topExp := make([]dashboardTopExperienceDTO, 0, len(d.TopExperiences))
	for _, e := range d.TopExperiences {
		host := joinName(e.HostFirstName.String, e.HostLastName.String)
		if host == "" {
			host = "—"
		}
		topExp = append(topExp, dashboardTopExperienceDTO{
			ID:       e.ID.String(),
			Title:    e.Title,
			HostName: host,
			Bookings: e.Bookings,
		})
	}

	RespondSuccess(w, http.StatusOK, dashboardStatsDTO{
		TotalEvents:     d.TotalEvents,
		TotalHosts:      d.TotalHosts,
		TotalBookings:   d.TotalBookings,
		TotalRevenue:    centsToMajor(d.TotalRevenueCents),
		PlatformIncome:  centsToMajor(d.PlatformIncomeCents),
		MonthlyBookings: monthly,
		RecentBookings:  recent,
		Attention: dashboardAttentionDTO{
			PendingHosts:    d.PendingHosts,
			ReviewEvents:    d.ReviewEvents,
			RefundsToReview: d.RefundsToReview,
		},
		TopHosts:       topHosts,
		TopExperiences: topExp,
	})
}
