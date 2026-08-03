package controller

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"myslotmate-backend/internal/lib/payment"
	"myslotmate-backend/internal/lib/payout"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// WebhookController handles callbacks from external payment providers.
type WebhookController struct {
	payoutService   service.PayoutService
	userService     service.UserService
	payoutProvider  payout.Provider
	paymentProvider payment.Provider
}

func NewWebhookController(ps service.PayoutService, us service.UserService, payoutProv payout.Provider, paymentProv payment.Provider) *WebhookController {
	return &WebhookController{
		payoutService:   ps,
		userService:     us,
		payoutProvider:  payoutProv,
		paymentProvider: paymentProv,
	}
}

func (c *WebhookController) RegisterRoutes(r chi.Router) {
	r.Route("/webhooks", func(r chi.Router) {
		r.Post("/payout", c.HandlePayoutWebhook)
		r.Post("/payment", c.HandlePaymentWebhook)
	})
}

// ── Cashfree payout webhook structures ─────────────────────────────────────

type CashfreeTransferEntity struct {
	TransferID   string `json:"transfer_id"`
	ReferenceID  string `json:"reference_id"`
	Status       string `json:"status"`
	Message      string `json:"message"`
	Reason       string `json:"reason"`
	ErrorMessage string `json:"error_message"`
}

// CashfreePayoutWebhookEvent handles common Cashfree webhook payload shapes.
type CashfreePayoutWebhookEvent struct {
	Event        string                  `json:"event"`
	Type         string                  `json:"type"`
	TransferID   string                  `json:"transfer_id"`
	ReferenceID  string                  `json:"reference_id"`
	Status       string                  `json:"status"`
	Message      string                  `json:"message"`
	Reason       string                  `json:"reason"`
	ErrorMessage string                  `json:"error_message"`
	Data         *CashfreeTransferEntity `json:"data,omitempty"`
	Transfer     *CashfreeTransferEntity `json:"transfer,omitempty"`
}

// mapCashfreeWebhookStatus converts Cashfree event/status to internal status.
func mapCashfreeWebhookStatus(eventName string, status string) string {
	switch strings.ToUpper(strings.TrimSpace(eventName)) {
	case "TRANSFER_SUCCESS", "PAYOUT_SUCCESS", "PAYOUT_PROCESSED":
		return "completed"
	case "TRANSFER_FAILED", "PAYOUT_FAILED":
		return "failed"
	case "TRANSFER_REVERSED", "PAYOUT_REVERSED", "PAYOUT_RETURNED":
		return "reversed"
	case "TRANSFER_PENDING", "TRANSFER_PROCESSING", "PAYOUT_PENDING", "PAYOUT_PROCESSING":
		return "processing"
	}

	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "SUCCESS", "COMPLETED", "PROCESSED":
		return "completed"
	case "FAILED", "REJECTED", "CANCELLED", "CANCELED":
		return "failed"
	case "REVERSED", "RETURNED":
		return "reversed"
	case "PENDING", "PROCESSING", "QUEUED", "INITIATED":
		return "processing"
	default:
		return ""
	}
}

func pickFirst(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (c *WebhookController) HandlePayoutWebhook(w http.ResponseWriter, r *http.Request) {
	// 1. Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}
	defer r.Body.Close()

	// 2. Verify Cashfree webhook signature
	signature := pickFirst(
		r.Header.Get("x-webhook-signature"),
		r.Header.Get("X-Webhook-Signature"),
		r.Header.Get("x-cashfree-signature"),
		r.Header.Get("X-Cashfree-Signature"),
	)
	// Cashfree signs timestamp+body; the timestamp arrives in this header.
	timestamp := pickFirst(
		r.Header.Get("x-webhook-timestamp"),
		r.Header.Get("X-Webhook-Timestamp"),
	)
	if !c.payoutProvider.ValidateWebhookSignature(body, signature, timestamp) {
		log.Printf("[webhook/payout] signature rejected: sig_present=%t timestamp_present=%t body_len=%d",
			signature != "", timestamp != "", len(body))
		RespondError(w, http.StatusUnauthorized, "Invalid webhook signature")
		return
	}

	// 3. Parse Cashfree webhook event
	var event CashfreePayoutWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid webhook payload")
		return
	}

	// 4. Map event/status to internal status
	eventName := pickFirst(event.Event, event.Type)
	status := mapCashfreeWebhookStatus(eventName, pickFirst(
		event.Status,
		func() string {
			if event.Data != nil {
				return event.Data.Status
			}
			return ""
		}(),
		func() string {
			if event.Transfer != nil {
				return event.Transfer.Status
			}
			return ""
		}(),
	))
	if status == "" {
		// Unrecognised event — acknowledge so provider doesn't retry
		log.Printf("[webhook] ignoring unhandled cashfree event: %s", eventName)
		RespondSuccess(w, http.StatusOK, map[string]string{"message": "event ignored"})
		return
	}

	// 5. Extract our payment ID from transfer/reference ID.
	paymentIDString := pickFirst(
		event.TransferID,
		event.ReferenceID,
		func() string {
			if event.Data != nil {
				return pickFirst(event.Data.TransferID, event.Data.ReferenceID)
			}
			return ""
		}(),
		func() string {
			if event.Transfer != nil {
				return pickFirst(event.Transfer.TransferID, event.Transfer.ReferenceID)
			}
			return ""
		}(),
	)

	paymentID, err := uuid.Parse(paymentIDString)
	if err != nil {
		log.Printf("[webhook] could not parse transfer/reference ID as UUID: %s", paymentIDString)
		RespondError(w, http.StatusBadRequest, "Invalid transfer/reference ID in payout event")
		return
	}

	// 6. Build error message from available fields
	errMsg := pickFirst(
		event.ErrorMessage,
		event.Reason,
		event.Message,
		func() string {
			if event.Data != nil {
				return pickFirst(event.Data.ErrorMessage, event.Data.Reason, event.Data.Message)
			}
			return ""
		}(),
		func() string {
			if event.Transfer != nil {
				return pickFirst(event.Transfer.ErrorMessage, event.Transfer.Reason, event.Transfer.Message)
			}
			return ""
		}(),
	)

	// 7. Delegate to service layer
	if err := c.payoutService.HandlePayoutWebhook(r.Context(), paymentID, status, errMsg); err != nil {
		log.Printf("[webhook] error processing payout webhook for payment %s: %v", paymentID, err)
		RespondError(w, http.StatusInternalServerError, "Failed to process webhook")
		return
	}

	RespondSuccess(w, http.StatusOK, map[string]string{"message": "webhook processed"})
}

// ── Payment (collection) webhook ────────────────────────────────────────────

// RazorpayPaymentWebhookEvent is the top-level payload for payment.captured /
// payment.failed / refund.processed / refund.failed events. Razorpay puts the
// refund entity under payload.refund instead of payload.payment, so the struct
// carries both.
type RazorpayPaymentWebhookEvent struct {
	Entity    string `json:"entity"` // "event"
	AccountID string `json:"account_id"`
	Event     string `json:"event"` // payment.captured, refund.processed, refund.failed, ...
	Payload   struct {
		Payment struct {
			Entity struct {
				ID      string `json:"id"`       // pay_xxxxx
				OrderID string `json:"order_id"` // order_xxxxx
				Amount  int64  `json:"amount"`
				Status  string `json:"status"` // captured, failed
			} `json:"entity"`
		} `json:"payment"`
		Refund struct {
			Entity struct {
				ID                   string `json:"id"`         // rfnd_xxxxx
				PaymentID            string `json:"payment_id"` // pay_xxxxx of the original payment
				Amount               int64  `json:"amount"`
				Status               string `json:"status"` // pending, processed, failed
				ErrorCode            string `json:"error_code"`
				ErrorDescription     string `json:"error_description"`
			} `json:"entity"`
		} `json:"refund"`
	} `json:"payload"`
	CreatedAt int64 `json:"created_at"`
}

func (c *WebhookController) HandlePaymentWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}
	defer r.Body.Close()

	signature := r.Header.Get("X-Razorpay-Signature")
	if !c.paymentProvider.ValidateWebhookSignature(body, signature) {
		RespondError(w, http.StatusUnauthorized, "Invalid webhook signature")
		return
	}

	var event RazorpayPaymentWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		RespondError(w, http.StatusBadRequest, "Invalid webhook payload")
		return
	}

	switch event.Event {
	case "payment.captured":
		orderID := event.Payload.Payment.Entity.OrderID
		paymentID := event.Payload.Payment.Entity.ID
		if orderID == "" || paymentID == "" {
			RespondError(w, http.StatusBadRequest, "Missing order_id or payment id in webhook payload")
			return
		}
		if err := c.userService.CreditWalletFromWebhook(r.Context(), orderID, paymentID); err != nil {
			log.Printf("[webhook/payment] error crediting wallet for order %s: %v", orderID, err)
			RespondError(w, http.StatusInternalServerError, "Failed to process payment webhook")
			return
		}
		RespondSuccess(w, http.StatusOK, map[string]string{"message": "payment webhook processed"})

	case "refund.processed", "refund.failed":
		refundID := event.Payload.Refund.Entity.ID
		if refundID == "" {
			RespondError(w, http.StatusBadRequest, "Missing refund id in webhook payload")
			return
		}
		// Map the Razorpay refund status to our PaymentStatus.
		newStatus := models.PaymentStatusCompleted
		reason := ""
		if event.Event == "refund.failed" {
			newStatus = models.PaymentStatusFailed
			reason = event.Payload.Refund.Entity.ErrorDescription
			if reason == "" {
				reason = event.Payload.Refund.Entity.ErrorCode
			}
		}
		if err := c.userService.ApplyRefundWebhook(r.Context(), refundID, newStatus, reason); err != nil {
			log.Printf("[webhook/payment] error applying refund webhook %s: %v", refundID, err)
			RespondError(w, http.StatusInternalServerError, "Failed to process refund webhook")
			return
		}
		RespondSuccess(w, http.StatusOK, map[string]string{"message": "refund webhook processed"})

	default:
		// Other event types (payment.failed, etc.) are not yet handled; respond
		// 200 so Razorpay stops retrying.
		log.Printf("[webhook/payment] ignoring event: %s", event.Event)
		RespondSuccess(w, http.StatusOK, map[string]string{"message": "event ignored"})
	}
}
