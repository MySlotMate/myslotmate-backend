package controller

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// codeAlphabet excludes visually ambiguous characters (0/O, 1/I) so generated
// codes are safe to read off a CSV and type in.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generateCouponCode returns a random 8-char code, optionally prefixed
// (e.g. "SUMMER-ABCD2345"). Entropy is high enough that collisions are rare; the
// caller retries on the unique-index violation just in case.
func generateCouponCode(prefix string) string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	out := make([]byte, 8)
	for i, b := range buf {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	code := string(out)
	if p := sanitizePrefix(prefix); p != "" {
		code = p + "-" + code
	}
	return code
}

// sanitizePrefix keeps only A–Z/0–9 (uppercased) and caps length, so a
// host-supplied prefix can never inject a comma/quote into a CSV export or
// otherwise produce an un-typeable code.
func sanitizePrefix(prefix string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(prefix) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 12 {
			break
		}
	}
	return b.String()
}

// CouponController exposes host-facing comp-coupon management plus the guest
// dry-run "validate" behind the checkout preview. Coupons are always full
// waivers (no partial discounts in v1).
type CouponController struct {
	couponRepo     repository.CouponRepository
	bookingService service.BookingService
	eventRepo      repository.EventRepository
}

func NewCouponController(couponRepo repository.CouponRepository, bookingService service.BookingService, eventRepo repository.EventRepository) *CouponController {
	return &CouponController{couponRepo: couponRepo, bookingService: bookingService, eventRepo: eventRepo}
}

// resolveEventID validates that an event-scoped coupon targets a real event
// owned by this host, returning the event's canonical UUID. A nil eventID
// (host-wide coupon) is allowed through. Returns a user-facing error otherwise —
// this is the guard that turns a raw FK violation into a clear message and stops
// a host scoping codes to an event they don't own.
func (c *CouponController) resolveEventID(ctx context.Context, hostID uuid.UUID, eventID *uuid.UUID) (*uuid.UUID, error) {
	if eventID == nil {
		return nil, nil
	}
	evt, err := c.eventRepo.GetByID(ctx, *eventID)
	if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, errEventNotFound
	}
	if evt.HostID != hostID {
		return nil, errEventNotOwned
	}
	return &evt.ID, nil
}

var (
	errEventNotFound = errors.New("event not found")
	errEventNotOwned = errors.New("you do not own this event")
)

// respondEventError maps resolveEventID failures to a clean HTTP response.
func (c *CouponController) respondEventError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errEventNotFound):
		RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errEventNotOwned):
		RespondError(w, http.StatusForbidden, err.Error())
	default:
		RespondError(w, http.StatusInternalServerError, err.Error())
	}
}

func (c *CouponController) RegisterRoutes(r chi.Router) {
	r.Route("/coupons", func(r chi.Router) {
		r.Post("/", c.CreateCoupon)
		r.Post("/batch", c.BatchCreateCoupons)
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
	// GrantsFree: true = free-booking code (comp); false = access code (a
	// per-guest passkey — unlocks but the guest pays). Defaults to true.
	GrantsFree     *bool      `json:"grants_free,omitempty"`
	MaxRedemptions *int       `json:"max_redemptions,omitempty"`
	PerUserLimit   *int       `json:"per_user_limit,omitempty"`
	ValidFrom      *time.Time `json:"valid_from,omitempty"`
	ValidUntil     *time.Time `json:"valid_until,omitempty"`
	IsActive       *bool      `json:"is_active,omitempty"`
}

// boolOr returns *p when set, else def — for optional booleans that default true.
func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
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
	eventID, err := c.resolveEventID(r.Context(), body.HostID, body.EventID)
	if err != nil {
		c.respondEventError(w, err)
		return
	}
	isActive := true
	if body.IsActive != nil {
		isActive = *body.IsActive
	}
	coupon := &models.Coupon{
		HostID:         body.HostID,
		EventID:        eventID,
		Code:           code,
		GrantsFree:     boolOr(body.GrantsFree, true),
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

// BatchCreateCoupons generates `count` unique codes in one go. Each is
// single-use by default (max_redemptions=1, per_user_limit=1) — ideal for
// handing a distinct code to each invited guest. Codes are generated
// server-side to guarantee uniqueness (retried on the unique-index violation).
func (c *CouponController) BatchCreateCoupons(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HostID         uuid.UUID  `json:"host_id"`
		EventID        *uuid.UUID `json:"event_id,omitempty"`
		Count          int        `json:"count"`
		Prefix         string     `json:"prefix,omitempty"`
		GrantsFree     *bool      `json:"grants_free,omitempty"`
		MaxRedemptions *int       `json:"max_redemptions,omitempty"`
		PerUserLimit   *int       `json:"per_user_limit,omitempty"`
		ValidUntil     *time.Time `json:"valid_until,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if body.HostID == uuid.Nil {
		RespondError(w, http.StatusBadRequest, "host_id is required")
		return
	}
	if body.Count < 1 || body.Count > 500 {
		RespondError(w, http.StatusBadRequest, "count must be between 1 and 500")
		return
	}

	eventID, err := c.resolveEventID(r.Context(), body.HostID, body.EventID)
	if err != nil {
		c.respondEventError(w, err)
		return
	}

	grantsFree := boolOr(body.GrantsFree, true)

	// Default to single-use (one code per guest).
	maxRedemptions := body.MaxRedemptions
	if maxRedemptions == nil {
		one := 1
		maxRedemptions = &one
	}
	perUserLimit := body.PerUserLimit
	if perUserLimit == nil {
		one := 1
		perUserLimit = &one
	}

	created := make([]*models.Coupon, 0, body.Count)
	for i := 0; i < body.Count; i++ {
		var made *models.Coupon
		for attempt := 0; attempt < 8; attempt++ {
			coupon := &models.Coupon{
				HostID:         body.HostID,
				EventID:        eventID,
				Code:           generateCouponCode(body.Prefix),
				GrantsFree:     grantsFree,
				MaxRedemptions: maxRedemptions,
				PerUserLimit:   perUserLimit,
				ValidUntil:     body.ValidUntil,
				IsActive:       true,
			}
			err := c.couponRepo.Create(r.Context(), coupon)
			if err == nil {
				made = coupon
				break
			}
			if strings.Contains(err.Error(), "coupons_host_code_key") {
				continue // regenerate on the rare collision
			}
			RespondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if made == nil {
			RespondError(w, http.StatusInternalServerError, "could not generate unique codes; please try again")
			return
		}
		created = append(created, made)
	}

	RespondSuccess(w, http.StatusCreated, created)
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
		"valid": true,
		// A free-booking code waives payment; an access code just unlocks.
		"comps_booking": coupon.GrantsFree,
		"code":          coupon.Code,
	})
}
