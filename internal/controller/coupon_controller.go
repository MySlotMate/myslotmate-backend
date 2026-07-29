package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CouponController exposes host-facing comp-coupon management plus the guest
// dry-run "validate" behind the checkout preview. Coupons are always full
// waivers (no partial discounts in v1).
type CouponController struct {
	couponRepo     repository.CouponRepository
	bookingService service.BookingService
}

func NewCouponController(couponRepo repository.CouponRepository, bookingService service.BookingService) *CouponController {
	return &CouponController{couponRepo: couponRepo, bookingService: bookingService}
}

func (c *CouponController) RegisterRoutes(r chi.Router) {
	r.Route("/coupons", func(r chi.Router) {
		r.Post("/", c.CreateCoupon)
		r.Post("/validate", c.ValidateCoupon)
		r.Get("/host/{hostID}", c.ListHostCoupons)
		r.Put("/{couponID}", c.UpdateCoupon)
		r.Delete("/{couponID}", c.DeleteCoupon)
	})
}

type couponRequestBody struct {
	HostID         uuid.UUID  `json:"host_id"`
	EventID        *uuid.UUID `json:"event_id,omitempty"`
	Code           string     `json:"code"`
	MaxRedemptions *int       `json:"max_redemptions,omitempty"`
	PerUserLimit   *int       `json:"per_user_limit,omitempty"`
	ValidFrom      *time.Time `json:"valid_from,omitempty"`
	ValidUntil     *time.Time `json:"valid_until,omitempty"`
	IsActive       *bool      `json:"is_active,omitempty"`
}

func (c *CouponController) CreateCoupon(w http.ResponseWriter, r *http.Request) {
	var body couponRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if body.HostID == uuid.Nil {
		RespondError(w, http.StatusBadRequest, "host_id is required")
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		RespondError(w, http.StatusBadRequest, "code is required")
		return
	}
	isActive := true
	if body.IsActive != nil {
		isActive = *body.IsActive
	}
	coupon := &models.Coupon{
		HostID:         body.HostID,
		EventID:        body.EventID,
		Code:           code,
		MaxRedemptions: body.MaxRedemptions,
		PerUserLimit:   body.PerUserLimit,
		ValidFrom:      body.ValidFrom,
		ValidUntil:     body.ValidUntil,
		IsActive:       isActive,
	}
	if err := c.couponRepo.Create(r.Context(), coupon); err != nil {
		// A duplicate code for the host trips the unique index.
		if strings.Contains(err.Error(), "coupons_host_code_key") {
			RespondError(w, http.StatusConflict, "you already have a coupon with this code")
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusCreated, coupon)
}

func (c *CouponController) ListHostCoupons(w http.ResponseWriter, r *http.Request) {
	hostID, err := uuid.Parse(chi.URLParam(r, "hostID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid host ID")
		return
	}
	coupons, err := c.couponRepo.ListByHost(r.Context(), hostID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, coupons)
}

func (c *CouponController) UpdateCoupon(w http.ResponseWriter, r *http.Request) {
	couponID, err := uuid.Parse(chi.URLParam(r, "couponID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid coupon ID")
		return
	}
	var body couponRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if body.HostID == uuid.Nil {
		RespondError(w, http.StatusBadRequest, "host_id is required")
		return
	}
	// Verify ownership before mutating.
	existing, err := c.couponRepo.GetByID(r.Context(), couponID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil || existing.HostID != body.HostID {
		RespondError(w, http.StatusNotFound, "coupon not found")
		return
	}
	// PUT is a partial update: an omitted field keeps its current value. Without
	// these fallbacks a partial payload would NULL out the limits — turning a
	// capped comp into an unlimited free-ticket generator.
	isActive := existing.IsActive
	if body.IsActive != nil {
		isActive = *body.IsActive
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		code = existing.Code
	}
	eventID := existing.EventID
	if body.EventID != nil {
		eventID = body.EventID
	}
	maxRedemptions := existing.MaxRedemptions
	if body.MaxRedemptions != nil {
		maxRedemptions = body.MaxRedemptions
	}
	perUserLimit := existing.PerUserLimit
	if body.PerUserLimit != nil {
		perUserLimit = body.PerUserLimit
	}
	validFrom := existing.ValidFrom
	if body.ValidFrom != nil {
		validFrom = body.ValidFrom
	}
	validUntil := existing.ValidUntil
	if body.ValidUntil != nil {
		validUntil = body.ValidUntil
	}
	updated := &models.Coupon{
		ID:             couponID,
		HostID:         body.HostID,
		EventID:        eventID,
		Code:           code,
		MaxRedemptions: maxRedemptions,
		PerUserLimit:   perUserLimit,
		ValidFrom:      validFrom,
		ValidUntil:     validUntil,
		IsActive:       isActive,
	}
	if err := c.couponRepo.Update(r.Context(), updated); err != nil {
		if strings.Contains(err.Error(), "coupons_host_code_key") {
			RespondError(w, http.StatusConflict, "you already have a coupon with this code")
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, updated)
}

func (c *CouponController) DeleteCoupon(w http.ResponseWriter, r *http.Request) {
	couponID, err := uuid.Parse(chi.URLParam(r, "couponID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid coupon ID")
		return
	}
	hostID, err := uuid.Parse(r.URL.Query().Get("host_id"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "host_id is required")
		return
	}
	if err := c.couponRepo.Delete(r.Context(), couponID, hostID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]bool{"deleted": true})
}

// ValidateCoupon is the guest checkout dry-run: given event + user + code it
// reports whether the code comps the booking, or a user-facing reason it doesn't.
// The authoritative re-check (and atomic redemption) happens in CreateBooking.
func (c *CouponController) ValidateCoupon(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EventID uuid.UUID `json:"event_id"`
		UserID  uuid.UUID `json:"user_id"`
		Code    string    `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	coupon, err := c.bookingService.ValidateCoupon(r.Context(), body.EventID, body.UserID, body.Code)
	if err != nil {
		// All validation failures are user-facing 400s with a specific message.
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, map[string]interface{}{
		"valid":         true,
		"comps_booking": true,
		"code":          coupon.Code,
	})
}
