package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"myslotmate-backend/internal/auth"
	"myslotmate-backend/internal/lib/notification"
	"myslotmate-backend/internal/lib/timeutil"
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
	repo         *repository.AdminDirectoryRepository
	hostRepo     repository.HostRepository
	userRepo     repository.UserRepository
	bookingRepo  repository.BookingRepository
	eventRepo    repository.EventRepository
	notifService notification.NotificationService
	jwtSecret    string
	frontendURL  string
}

func NewAdminDirectoryController(
	repo *repository.AdminDirectoryRepository,
	hostRepo repository.HostRepository,
	userRepo repository.UserRepository,
	bookingRepo repository.BookingRepository,
	eventRepo repository.EventRepository,
	notifService notification.NotificationService,
	jwtSecret string,
	frontendURL string,
) *AdminDirectoryController {
	return &AdminDirectoryController{
		repo:         repo,
		hostRepo:     hostRepo,
		userRepo:     userRepo,
		bookingRepo:  bookingRepo,
		eventRepo:    eventRepo,
		notifService: notifService,
		jwtSecret:    jwtSecret,
		frontendURL:  strings.TrimRight(frontendURL, "/"),
	}
}

func (c *AdminDirectoryController) RegisterRoutes(r chi.Router) {
	r.Route("/admin/directory", func(r chi.Router) {
		r.Use(auth.RequireAdminToken(c.jwtSecret))
		r.Get("/users", c.ListUsers)
		r.Get("/hosts", c.ListHosts)
		r.Get("/hosts/{hostID}", c.GetHost)
		r.Get("/events", c.ListEvents)
		r.Get("/bookings", c.ListBookings)
		r.Get("/bookings/{bookingID}/reminder-preview", c.GetBookingReminderPreview)
		r.Post("/bookings/{bookingID}/send-reminder", c.SendBookingReminder)
		r.Post("/marketing/events/{eventID}/bulk-notify", c.BulkNotifyEventGuests)
		r.Post("/marketing/events/{eventID}/promote", c.PromoteEventToAllUsers)
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

type adminBookingDTO struct {
	ID            string `json:"id"`
	EventID       string `json:"event_id"`
	User          string `json:"user"`
	Experience    string `json:"experience"`
	Host          string `json:"host"`
	City          string `json:"city"`
	Date          string `json:"date"`
	OccurrenceDate string `json:"occurrence_date"` // RFC3339, for ticket generation
	Amount        int64  `json:"amount"`
	AmountCents   int64  `json:"amount_cents"`
	Quantity      int64  `json:"quantity"`
	PaymentStatus string `json:"paymentStatus"`
	BookingStatus string `json:"bookingStatus"`
}

type adminEventDTO struct {
	ID          string `json:"id"`
	HostID      string `json:"host_id"`
	IsRecurring bool   `json:"is_recurring"`

	Title    string  `json:"title"`
	HostName string  `json:"hostName"`
	City     string  `json:"city"`
	Category string  `json:"category"`
	Price    int64   `json:"price"`
	IsFree   bool    `json:"isFree"`
	Bookings int64   `json:"bookings"`
	Rating   float64 `json:"rating"`
	Status   string  `json:"status"`     // draft | live | paused | cancelled
	IsExpired bool   `json:"is_expired"` // non-recurring event whose time has passed
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
			ID:          e.ID.String(),
			HostID:      e.HostID.String,
			IsRecurring: e.IsRecurring,

			Title:    e.Title,
			HostName: hostName,
			City:     city,
			Category: category,
			Price:    price,
			IsFree:   e.IsFree,
			Bookings:  e.TotalBookings,
			Rating:    rating,
			Status:    e.Status,
			IsExpired: e.IsExpired,
		})
	}

	RespondSuccess(w, http.StatusOK, paginatedResponse{
		Items:    events,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// ListBookings returns a page of all bookings with guest, experience, host,
// city, amount, and payment/booking status.
func (c *AdminDirectoryController) ListBookings(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset := parsePagination(r)
	q := r.URL.Query()

	rows, total, err := c.repo.ListBookings(r.Context(), repository.ListBookingsParams{
		Limit:   pageSize,
		Offset:  offset,
		Search:  q.Get("search"),
		Status:  q.Get("status"),
		EventID: q.Get("event_id"),
	})
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	bookings := make([]adminBookingDTO, 0, len(rows))
	for _, b := range rows {
		guest := b.UserName.String
		if guest == "" {
			guest = "Unknown guest"
		}
		experience := b.EventTitle.String
		if experience == "" {
			experience = "—"
		}
		host := joinName(b.HostFirstName.String, b.HostLastName.String)
		if host == "" {
			host = "—"
		}
		city := b.HostCity.String
		if city == "" {
			city = "—"
		}
		// Prefer the occurrence date; fall back to when the booking was made.
		date := b.CreatedAt
		if b.OccurrenceDate.Valid {
			date = b.OccurrenceDate.Time
		}
		var amount int64
		if b.AmountCents.Valid {
			amount = centsToMajor(b.AmountCents.Int64)
		}

		var amountCents int64
		if b.AmountCents.Valid {
			amountCents = b.AmountCents.Int64
		}
		bookings = append(bookings, adminBookingDTO{
			ID:             b.ID.String(),
			EventID:        b.EventID.String(),
			User:           guest,
			Experience:     experience,
			Host:           host,
			City:           city,
			Date:           date.Format("02 Jan 2006"),
			OccurrenceDate: date.Format(time.RFC3339),
			Amount:         amount,
			AmountCents:    amountCents,
			Quantity:       b.Quantity,
			PaymentStatus:  paymentStatusLabel(b.PaymentStatus.String, b.Status),
			BookingStatus:  bookingStatusLabel(b.Status),
		})
	}

	RespondSuccess(w, http.StatusOK, paginatedResponse{
		Items:    bookings,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// ── Mapping helpers ──────────────────────────────────────────────────────────

// bookingStatusLabel humanizes the raw booking status.
func bookingStatusLabel(status string) string {
	switch status {
	case "confirmed":
		return "Confirmed"
	case "pending":
		return "Pending"
	case "cancelled":
		return "Cancelled"
	case "refunded":
		return "Refunded"
	default:
		return status
	}
}

// paymentStatusLabel maps the payment row's status to a display label, falling
// back to the booking status when there is no linked payment row.
func paymentStatusLabel(paymentStatus, bookingStatus string) string {
	switch paymentStatus {
	case "completed":
		return "Paid"
	case "pending", "processing":
		return "Pending"
	case "reversed":
		return "Refunded"
	case "failed":
		return "Failed"
	}
	// No (or unknown) payment row — infer from the booking lifecycle.
	switch bookingStatus {
	case "confirmed":
		return "Paid"
	case "refunded":
		return "Refunded"
	case "cancelled":
		return "Cancelled"
	default:
		return "Pending"
	}
}

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

func (c *AdminDirectoryController) GetBookingReminderPreview(w http.ResponseWriter, r *http.Request) {
	bookingIDStr := chi.URLParam(r, "bookingID")
	bookingID, err := uuid.Parse(bookingIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid booking ID")
		return
	}

	booking, err := c.bookingRepo.GetByID(r.Context(), bookingID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if booking == nil {
		RespondError(w, http.StatusNotFound, "Booking not found")
		return
	}

	user, err := c.userRepo.GetByID(r.Context(), booking.UserID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if user == nil {
		RespondError(w, http.StatusNotFound, "User not found")
		return
	}

	event, err := c.eventRepo.GetByID(r.Context(), booking.EventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if event == nil {
		RespondError(w, http.StatusNotFound, "Event not found")
		return
	}

	eventTimeStr := timeutil.FormatEventTime(event.Time)
	if !booking.OccurrenceDate.IsZero() {
		eventTimeStr = timeutil.FormatEventTime(booking.OccurrenceDate)
	}

	whatsappBody := fmt.Sprintf(
		"⏰ Event Starting Soon!\n\nEvent: %s\nTime: %s\n\nYour booking is confirmed. See you soon!",
		event.Title,
		eventTimeStr,
	)

	emailSubject := "Upcoming Event Reminder - MySlotMate"
	emailBody := fmt.Sprintf(`
<html>
<body style="font-family: Arial, sans-serif; line-height: 1.6; color: #333;">
	<h2>📅 Upcoming Event Reminder</h2>
	<p>Hi %s,</p>
	<p>This is a reminder that your event "<strong>%s</strong>" is scheduled for %s.</p>
	<p>Please review your event details and ensure all preparations are in place for a smooth experience.</p>
	<p>We wish you a successful event! 🎉</p>
	<hr style="margin: 30px 0;">
	<p style="font-size: 12px; color: #666;">MySlotMate - Event Management Made Easy</p>
</body>
</html>
`, user.Name, event.Title, eventTimeStr)

	RespondSuccess(w, http.StatusOK, map[string]string{
		"whatsapp_body": whatsappBody,
		"email_subject": emailSubject,
		"email_body":    emailBody,
		"user_email":    user.Email,
		"user_phone":    user.PhnNumber,
	})
}

func (c *AdminDirectoryController) SendBookingReminder(w http.ResponseWriter, r *http.Request) {
	bookingIDStr := chi.URLParam(r, "bookingID")
	bookingID, err := uuid.Parse(bookingIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid booking ID")
		return
	}

	booking, err := c.bookingRepo.GetByID(r.Context(), bookingID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if booking == nil {
		RespondError(w, http.StatusNotFound, "Booking not found")
		return
	}

	user, err := c.userRepo.GetByID(r.Context(), booking.UserID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if user == nil {
		RespondError(w, http.StatusNotFound, "User not found")
		return
	}

	event, err := c.eventRepo.GetByID(r.Context(), booking.EventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if event == nil {
		RespondError(w, http.StatusNotFound, "Event not found")
		return
	}

	// Send notifications
	var whatsappErr error
	if user.PhnNumber != "" && c.notifService != nil {
		whatsappErr = c.notifService.SendEventReminderWhatsapp(r.Context(), booking, user, event)
	}

	var emailErr error
	if user.Email != "" && c.notifService != nil {
		emailErr = c.notifService.SendEventReminderEmail(r.Context(), booking, user, event)
	}

	if whatsappErr != nil && emailErr != nil {
		RespondError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to send WhatsApp (%v) and Email (%v)", whatsappErr, emailErr))
		return
	}

	statusMsg := "Reminder sent successfully"
	if whatsappErr != nil {
		statusMsg = fmt.Sprintf("Reminder sent via email, but WhatsApp failed: %v", whatsappErr)
	} else if emailErr != nil {
		statusMsg = fmt.Sprintf("Reminder sent via WhatsApp, but Email failed: %v", emailErr)
	}

	RespondSuccess(w, http.StatusOK, map[string]string{
		"message": statusMsg,
	})
}

type BulkNotifyRequest struct {
	Message string `json:"message"`
	Channel string `json:"channel"` // "both" | "whatsapp" | "email"
}

func (c *AdminDirectoryController) BulkNotifyEventGuests(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "eventID")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	var req BulkNotifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	event, err := c.eventRepo.GetByID(r.Context(), eventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if event == nil {
		RespondError(w, http.StatusNotFound, "Event not found")
		return
	}

	bookings, err := c.bookingRepo.ListByEventID(r.Context(), eventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Filter active bookings: confirmed, pending
	var targetBookings []*models.Booking
	for _, b := range bookings {
		if b.Status == "confirmed" || b.Status == "pending" {
			targetBookings = append(targetBookings, b)
		}
	}

	if len(targetBookings) == 0 {
		RespondSuccess(w, http.StatusOK, map[string]interface{}{
			"message":        "No active bookings found for this event",
			"notified_count": 0,
		})
		return
	}

	// Queue notification broadcasts in the background
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		for _, booking := range targetBookings {
			user, err := c.userRepo.GetByID(bgCtx, booking.UserID)
			if err != nil || user == nil {
				continue
			}

			// WhatsApp Notification
			if (req.Channel == "both" || req.Channel == "whatsapp") && user.PhnNumber != "" && c.notifService != nil {
				if req.Message != "" {
					kapso := c.notifService.GetKapsoClient()
					if kapso != nil {
						_ = kapso.SendTextMessage(bgCtx, user.PhnNumber, req.Message)
					} else {
						// Fall back to standard template reminder on Twilio if Kapso is missing
						_ = c.notifService.SendEventReminderWhatsapp(bgCtx, booking, user, event)
					}
				} else {
					// Send standard reminder
					_ = c.notifService.SendEventReminderWhatsapp(bgCtx, booking, user, event)
				}
			}

			// Email Notification
			if (req.Channel == "both" || req.Channel == "email") && user.Email != "" && c.notifService != nil {
				if req.Message != "" {
					// Send custom email announcement
					subject := fmt.Sprintf("Announcement: %s", event.Title)
					emailBody := fmt.Sprintf(`
<html>
<body style="font-family: Arial, sans-serif; line-height: 1.6; color: #333;">
	<h2>Announcement regarding: %s</h2>
	<p>Hi %s,</p>
	<p>%s</p>
	<hr style="margin: 30px 0;">
	<p style="font-size: 12px; color: #666;">MySlotMate - Event Management Made Easy</p>
</body>
</html>
`, event.Title, user.Name, req.Message)
					
					_ = c.notifService.SendCustomEmail(bgCtx, user.Email, subject, emailBody)
				} else {
					// Send standard reminder email
					_ = c.notifService.SendEventReminderEmail(bgCtx, booking, user, event)
				}
			}
		}
	}()

	RespondSuccess(w, http.StatusOK, map[string]interface{}{
		"message":        fmt.Sprintf("Bulk notifications successfully queued to %d users", len(targetBookings)),
		"notified_count": len(targetBookings),
	})
}


// ── Marketing blast to all users ─────────────────────────────────────────────

// promoteInFlight guards against a second blast starting while one is running.
// The Send button is disabled client-side only, so without this a double-click
// mails everyone twice.
//
// ponytail: one global flag, not per-event — make it a map keyed by event ID if
// concurrent blasts for different events ever need to overlap.
var promoteInFlight sync.Mutex

type PromoteEventRequest struct {
	Message string `json:"message"`
	Channel string `json:"channel"` // "both" | "whatsapp" | "email"
	City    string `json:"city"`    // optional, exact match
	DryRun  bool   `json:"dry_run"` // resolve the audience and return, send nothing
	// TestPhone narrows the audience to the one user holding this number, so a
	// real send can be aimed at a single handset without touching anyone else.
	TestPhone string `json:"test_phone"`
}

// PromoteEventToAllUsers markets an upcoming event to every registered user,
// optionally narrowed by city — unlike BulkNotifyEventGuests, which only
// reaches people who already booked.
//
// WhatsApp goes out as an approved marketing template only (no session window
// exists for these recipients); email is a custom HTML mail. Per-channel
// successes and failures are counted and logged when the run finishes: there is
// no delivery table, so the log is the only record.
func (c *AdminDirectoryController) PromoteEventToAllUsers(w http.ResponseWriter, r *http.Request) {
	eventIDStr := chi.URLParam(r, "eventID")
	eventID, err := uuid.Parse(eventIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

	var req PromoteEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		RespondError(w, http.StatusBadRequest, "Message is required for a marketing blast")
		return
	}
	if req.Channel == "" {
		req.Channel = "both"
	}

	event, err := c.eventRepo.GetByID(r.Context(), eventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if event == nil {
		RespondError(w, http.StatusNotFound, "Event not found")
		return
	}

	recipients, err := c.repo.ListMarketingRecipients(r.Context(), req.City)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A test send beats every other filter: keep only the matching handset.
	// Compared on the last 10 digits because numbers are stored with and
	// without the +91 prefix.
	if tp := digitsOnly(req.TestPhone); tp != "" {
		filtered := recipients[:0]
		for _, u := range recipients {
			if lastN(digitsOnly(u.Phone), 10) == lastN(tp, 10) {
				filtered = append(filtered, u)
			}
		}
		recipients = filtered
	}

	if len(recipients) == 0 {
		RespondSuccess(w, http.StatusOK, map[string]interface{}{
			"message":        "No users matched this audience",
			"notified_count": 0,
		})
		return
	}

	// Dry run: the audience size is the only thing worth checking before a blast,
	// and an empty City means EVERY user — cheap insurance against a typo.
	if req.DryRun {
		sample := make([]string, 0, 5)
		for i, u := range recipients {
			if i == 5 {
				break
			}
			sample = append(sample, fmt.Sprintf("%s <%s> %s", u.Name, u.Email, u.Phone))
		}
		RespondSuccess(w, http.StatusOK, map[string]interface{}{
			"message":        fmt.Sprintf("DRY RUN — would send to %d users, nothing was sent", len(recipients)),
			"notified_count": len(recipients),
			"dry_run":        true,
			"event_url":      c.frontendURL + "/experience/" + event.Slug,
			"sample":         sample,
		})
		return
	}

	if !promoteInFlight.TryLock() {
		RespondError(w, http.StatusConflict, "A marketing blast is already running — wait for it to finish")
		return
	}

	eventURL := c.frontendURL + "/experience/" + event.Slug

	go func() {
		defer promoteInFlight.Unlock()
		// No overall deadline: a few thousand sequential sends can outlast any
		// sane one. Each send gets its own timeout instead.
		bgCtx := context.Background()

		var waSent, waFailed, mailSent, mailFailed int
		// Duplicate accounts sharing a phone or an email are common here, and the
		// audience is a list of user ROWS — without this, one handset receives the
		// same promo once per account behind it.
		sentPhone := map[string]bool{}
		sentEmail := map[string]bool{}

		for _, u := range recipients {
			phoneKey := lastN(digitsOnly(u.Phone), 10)
			emailKey := strings.ToLower(strings.TrimSpace(u.Email))

			if (req.Channel == "both" || req.Channel == "whatsapp") && u.Phone != "" && !sentPhone[phoneKey] {
				sentPhone[phoneKey] = true
				sendCtx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
				if err := c.notifService.SendEventPromoWhatsapp(sendCtx, u.Phone, u.Name, event.Title, event.Slug); err != nil {
					waFailed++
				} else {
					waSent++
				}
				cancel()
			}

			if (req.Channel == "both" || req.Channel == "email") && emailKey != "" && !sentEmail[emailKey] {
				sentEmail[emailKey] = true
				sendCtx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
				subject := fmt.Sprintf("Happening soon: %s", event.Title)
				body := fmt.Sprintf(`
<html>
<body style="font-family: Arial, sans-serif; line-height: 1.6; color: #333;">
	<h2>%s</h2>
	<p>Hi %s,</p>
	<p>%s</p>
	<p><a href="%s" style="display:inline-block;padding:12px 20px;background:#6d28d9;color:#fff;text-decoration:none;border-radius:8px;">View experience</a></p>
	<hr style="margin: 30px 0;">
	<p style="font-size: 12px; color: #666;">MySlotMate - Event Management Made Easy</p>
</body>
</html>
`, event.Title, u.Name, req.Message, eventURL)
				if err := c.notifService.SendCustomEmail(sendCtx, u.Email, subject, body); err != nil {
					mailFailed++
				} else {
					mailSent++
				}
				cancel()
			}

			// ponytail: fixed pacing, swap for a real rate limiter if a provider
			// starts throttling. SMTP hosts drop mail silently once tripped.
			time.Sleep(200 * time.Millisecond)
		}

		log.Printf("[EVENT_PROMO] event=%s rows=%d whatsapp sent=%d failed=%d email sent=%d failed=%d\n",
			eventID, len(recipients), waSent, waFailed, mailSent, mailFailed)
	}()

	RespondSuccess(w, http.StatusOK, map[string]interface{}{
		"message":        fmt.Sprintf("Marketing blast queued to %d users", len(recipients)),
		"notified_count": len(recipients),
	})
}

// digitsOnly strips everything but digits from a phone number.
func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lastN returns the final n characters of s, or all of s when it is shorter.
func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
