package controller

import (
	"encoding/json"
	"net/http"
	"strconv"

	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// UserController handles HTTP requests for user operations
type UserController struct {
	userService service.UserService
}

// NewUserController Factory for UserController
func NewUserController(s service.UserService) *UserController {
	return &UserController{
		userService: s,
	}
}

// RegisterRoutes registers routes for the user controller on the provided router
func (c *UserController) RegisterRoutes(r chi.Router) {
	r.Post("/auth/signup", c.HandleSignUp)
	r.Post("/auth/verify-aadhar/init", c.InitiateAadharVerification)
	r.Post("/auth/verify-aadhar/complete", c.CompleteAadharVerification)
	r.Post("/auth/otp/send", c.SendPhoneOTP)
	r.Post("/auth/otp/verify", c.VerifyPhoneOTP)
	r.Post("/auth/otp/login/send", c.SendLoginOTP)
	r.Post("/auth/otp/login/verify", c.VerifyLoginOTP)
	r.Route("/users", func(r chi.Router) {
		r.Get("/me", c.GetProfile)
		r.Get("/by-firebase/{firebaseID}", c.GetUserByFirebaseID)
		r.Put("/me", c.UpdateProfile)
		r.Get("/attendee-profile", c.GetAttendeeProfile)
		r.Put("/attendee-profile", c.UpsertAttendeeProfile)
		r.Get("/{userID}", c.GetUserByID)
		r.Get("/wallet/balance", c.GetWalletBalance)
		r.Get("/wallet/transactions", c.GetWalletTransactions)
		r.Post("/wallet/topup", c.InitiateTopUp)
		r.Post("/wallet/topup/verify", c.VerifyTopUp)
		r.Post("/saved-experiences", c.SaveExperience)
		r.Delete("/saved-experiences/{eventID}", c.UnsaveExperience)
		r.Get("/saved-experiences", c.GetSavedExperiences)
		r.Get("/saved-experiences/{eventID}/check", c.IsExperienceSaved)
	})
}

type UserSignUpRequest struct {
	AuthUID   string  `json:"auth_uid"`
	Email     string  `json:"email"`
	Name      string  `json:"name"`
	PhnNumber string  `json:"phn_number"`
	AvatarURL *string `json:"avatar_url,omitempty"`
}

type InitiateAadharRequest struct {
	UserID       uuid.UUID `json:"user_id"`
	AadharNumber string    `json:"aadhar_number"`
}

type CompleteAadharRequest struct {
	UserID        uuid.UUID `json:"user_id"`
	TransactionID string    `json:"transaction_id"`
	OTP           string    `json:"otp"`
}

type UserProfileUpdateRequestBody struct {
	Name      *string `json:"name,omitempty"`
	AvatarURL *string `json:"avatar_url,omitempty"`
	City      *string `json:"city,omitempty"`
}

type SaveExperienceRequestBody struct {
	UserID  uuid.UUID `json:"user_id"`
	EventID uuid.UUID `json:"event_id"`
}

func (c *UserController) InitiateAadharVerification(w http.ResponseWriter, r *http.Request) {
	var req InitiateAadharRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	txnID, err := c.userService.InitiateAadharVerification(r.Context(), req.UserID, req.AadharNumber)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{
		"transaction_id": txnID,
		"message":        "OTP sent successfully",
	})
}

func (c *UserController) CompleteAadharVerification(w http.ResponseWriter, r *http.Request) {
	var req CompleteAadharRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	err := c.userService.CompleteAadharVerification(r.Context(), req.UserID, req.TransactionID, req.OTP)
	if err != nil {
		RespondError(w, http.StatusBadRequest, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{
		"message": "User verified successfully",
	})
}

// HandleSignUp handles the POST /auth/signup endpoint
func (c *UserController) HandleSignUp(w http.ResponseWriter, r *http.Request) {
	var req UserSignUpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	svcReq := service.SignUpRequest{
		AuthUID:   req.AuthUID,
		Email:     req.Email,
		Name:      req.Name,
		PhnNumber: req.PhnNumber,
		AvatarURL: req.AvatarURL,
	}

	user, err := c.userService.SignUp(r.Context(), svcReq)
	if err != nil {
		if err.Error() == "user already exists" {
			RespondError(w, http.StatusConflict, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, "Failed to create user")
		return
	}

	RespondSuccess(w, http.StatusCreated, user)
}

func (c *UserController) GetProfile(w http.ResponseWriter, r *http.Request) {
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

	user, err := c.userService.GetProfile(r.Context(), userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, user)
}

func (c *UserController) GetUserByID(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "userID")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid user ID")
		return
	}

	user, err := c.userService.GetProfile(r.Context(), userID)
	if err != nil {
		if err.Error() == "user not found" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, user)
}

func (c *UserController) UpdateProfile(w http.ResponseWriter, r *http.Request) {
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

	var req UserProfileUpdateRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	svcReq := service.UserProfileUpdateRequest{
		Name:      req.Name,
		AvatarURL: req.AvatarURL,
		City:      req.City,
	}

	user, err := c.userService.UpdateProfile(r.Context(), userID, svcReq)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, user)
}

// AttendeeProfileRequestBody is the upsert payload for a user's attendee details.
type AttendeeProfileRequestBody struct {
	UserID           uuid.UUID `json:"user_id"`
	Name             *string   `json:"name,omitempty"`
	Age              *int      `json:"age,omitempty"`
	Gender           *string   `json:"gender,omitempty"`
	Qualification    *string   `json:"qualification,omitempty"`
	Occupation       *string   `json:"occupation,omitempty"`
	MaritalStatus    *string   `json:"marital_status,omitempty"`
	ContactNumber    *string   `json:"contact_number,omitempty"`
	WhatsappNumber   *string   `json:"whatsapp_number,omitempty"`
	RegistrationType *string   `json:"registration_type,omitempty"`
	GovtIDURL        *string   `json:"govt_id_url,omitempty"`
	Travel           *bool     `json:"travel,omitempty"`
}

func (c *UserController) GetAttendeeProfile(w http.ResponseWriter, r *http.Request) {
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
	profile, err := c.userService.GetAttendeeProfile(r.Context(), userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// nil (none saved yet) is a valid empty state, not an error.
	RespondSuccess(w, http.StatusOK, profile)
}

func (c *UserController) UpsertAttendeeProfile(w http.ResponseWriter, r *http.Request) {
	var req AttendeeProfileRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if req.UserID == uuid.Nil {
		RespondError(w, http.StatusBadRequest, "Missing user_id")
		return
	}
	profile := &models.AttendeeProfile{
		UserID:           req.UserID,
		Name:             req.Name,
		Age:              req.Age,
		Gender:           req.Gender,
		Qualification:    req.Qualification,
		Occupation:       req.Occupation,
		MaritalStatus:    req.MaritalStatus,
		ContactNumber:    req.ContactNumber,
		WhatsappNumber:   req.WhatsappNumber,
		RegistrationType: req.RegistrationType,
		GovtIDURL:        req.GovtIDURL,
		Travel:           req.Travel,
	}
	saved, err := c.userService.UpsertAttendeeProfile(r.Context(), profile)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, saved)
}

func (c *UserController) SaveExperience(w http.ResponseWriter, r *http.Request) {
	var req SaveExperienceRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if err := c.userService.SaveExperience(r.Context(), req.UserID, req.EventID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, map[string]string{"message": "Experience saved"})
}

func (c *UserController) UnsaveExperience(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

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

	if err := c.userService.UnsaveExperience(r.Context(), userID, eventID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "Experience unsaved"})
}

func (c *UserController) GetSavedExperiences(w http.ResponseWriter, r *http.Request) {
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

	saved, err := c.userService.GetSavedExperiences(r.Context(), userID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, saved)
}

func (c *UserController) IsExperienceSaved(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuid.Parse(chi.URLParam(r, "eventID"))
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid event ID")
		return
	}

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

	exists, err := c.userService.IsExperienceSaved(r.Context(), userID, eventID)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]bool{"saved": exists})
}

type TopUpRequestBody struct {
	UserID         uuid.UUID `json:"user_id"`
	AmountCents    int64     `json:"amount_cents"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
}

type VerifyTopUpRequestBody struct {
	UserID            uuid.UUID `json:"user_id"`
	RazorpayOrderID   string    `json:"razorpay_order_id"`
	RazorpayPaymentID string    `json:"razorpay_payment_id"`
	RazorpaySignature string    `json:"razorpay_signature"`
}

func (c *UserController) GetWalletBalance(w http.ResponseWriter, r *http.Request) {
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

	balance, err := c.userService.GetWalletBalance(r.Context(), userID)
	if err != nil {
		if err.Error() == "wallet not found" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, balance)
}

// GetWalletTransactions returns the user's payment history for the wallet
// history UI. `user_id` via query string (matches the rest of /users/* — same
// H5 caveat applies); `limit` (default 50, max 200) and `offset` paginate.
func (c *UserController) GetWalletTransactions(w http.ResponseWriter, r *http.Request) {
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
	limit := 50
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	txns, err := c.userService.GetWalletTransactions(r.Context(), userID, limit, offset)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	RespondSuccess(w, http.StatusOK, txns)
}

// InitiateTopUp creates a Razorpay order and returns checkout details to the client.
func (c *UserController) InitiateTopUp(w http.ResponseWriter, r *http.Request) {
	var req TopUpRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if req.AmountCents <= 0 {
		RespondError(w, http.StatusBadRequest, "amount_cents must be positive")
		return
	}

	svcReq := service.TopUpRequest{
		AmountCents:    req.AmountCents,
		IdempotencyKey: req.IdempotencyKey,
	}

	result, err := c.userService.InitiateTopUp(r.Context(), req.UserID, svcReq)
	if err != nil {
		if err.Error() == "wallet not found" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusCreated, result)
}

// VerifyTopUp verifies the Razorpay checkout callback and credits the wallet.
func (c *UserController) VerifyTopUp(w http.ResponseWriter, r *http.Request) {
	var req VerifyTopUpRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	if req.RazorpayOrderID == "" || req.RazorpayPaymentID == "" || req.RazorpaySignature == "" {
		RespondError(w, http.StatusBadRequest, "Missing razorpay_order_id, razorpay_payment_id, or razorpay_signature")
		return
	}

	svcReq := service.VerifyTopUpRequest{
		RazorpayOrderID:   req.RazorpayOrderID,
		RazorpayPaymentID: req.RazorpayPaymentID,
		RazorpaySignature: req.RazorpaySignature,
	}

	result, err := c.userService.VerifyTopUp(r.Context(), req.UserID, svcReq)
	if err != nil {
		if err.Error() == "invalid payment signature" {
			RespondError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if err.Error() == "payment record not found for this order" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, result)
}

func (c *UserController) GetUserByFirebaseID(w http.ResponseWriter, r *http.Request) {
	firebaseID := chi.URLParam(r, "firebaseID")
	if firebaseID == "" {
		RespondError(w, http.StatusBadRequest, "Missing firebase ID")
		return
	}

	user, err := c.userService.GetByAuthUID(r.Context(), firebaseID)
	if err != nil {
		if err.Error() == "user not found" {
			RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, user)
}

// SendPhoneOTP handles sending OTP to user's phone
func (c *UserController) SendPhoneOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid user_id")
		return
	}

	if err := c.userService.SendPhoneOTP(r.Context(), userID); err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "OTP sent successfully"})
}

// VerifyPhoneOTP handles verifying the OTP
func (c *UserController) VerifyPhoneOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"user_id"`
		OTP    string `json:"otp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid user_id")
		return
	}

	if err := c.userService.VerifyPhoneOTP(r.Context(), userID, req.OTP); err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "OTP verified successfully"})
}

type SendLoginOTPRequest struct {
	Phone string `json:"phone"`
}

type VerifyLoginOTPRequest struct {
	Phone     string `json:"phone"`
	SessionID string `json:"session_id"`
	OTP       string `json:"otp"`
}

// SendLoginOTP handles initiating OTP flow via 2Factor
func (c *UserController) SendLoginOTP(w http.ResponseWriter, r *http.Request) {
	var req SendLoginOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if req.Phone == "" {
		RespondError(w, http.StatusBadRequest, "Phone number is required")
		return
	}

	sessionID, err := c.userService.SendLoginOTP(r.Context(), req.Phone)
	if err != nil {
		RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{
		"session_id": sessionID,
		"message":    "OTP sent successfully",
	})
}

// VerifyLoginOTP handles verifying the OTP and issuing JWT
func (c *UserController) VerifyLoginOTP(w http.ResponseWriter, r *http.Request) {
	var req VerifyLoginOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}

	if req.Phone == "" || req.SessionID == "" || req.OTP == "" {
		RespondError(w, http.StatusBadRequest, "phone, session_id, and otp are required")
		return
	}

	user, token, firebaseCustomToken, err := c.userService.VerifyLoginOTP(r.Context(), req.Phone, req.SessionID, req.OTP)
	if err != nil {
		RespondError(w, http.StatusUnauthorized, err.Error())
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]interface{}{
		"user":                  user,
		"token":                 token,
		"firebase_custom_token": firebaseCustomToken,
	})
}
