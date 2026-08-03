package payout

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CashfreeConfig holds credentials for Cashfree Payouts API.
type CashfreeConfig struct {
	BaseURL       string // CASHFREE_BASE_URL (default: https://payout-api.cashfree.com)
	ClientID      string // CASHFREE_CLIENT_ID
	ClientSecret  string // CASHFREE_CLIENT_SECRET
	PublicKey     string // CASHFREE_PUBLIC_KEY (PEM-encoded RSA public key for signature generation)
	WebhookSecret string // CASHFREE_WEBHOOK_SECRET
	APIVersion    string // CASHFREE_API_VERSION
	BearerToken   string // CASHFREE_BEARER_TOKEN (optional)
}

// CashfreeProvider implements Provider using Cashfree Payouts API.
type CashfreeProvider struct {
	cfg    CashfreeConfig
	client *http.Client
}

const defaultCashfreeBaseURL = "https://payout-api.cashfree.com"

func NewCashfreeProvider(cfg CashfreeConfig) Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultCashfreeBaseURL
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = "2024-01-01"
	}

	provider := &CashfreeProvider{
		cfg: cfg,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	return provider
}

type cashfreeTransferReq struct {
	TransferID         string                        `json:"transfer_id"`
	TransferAmount     string                        `json:"transfer_amount"`
	TransferCurrency   string                        `json:"transfer_currency"`
	TransferMode       string                        `json:"transfer_mode"`
	TransferRemarks    string                        `json:"transfer_remarks,omitempty"`
	BeneficiaryDetails cashfreerV2BeneficiaryDetails `json:"beneficiary_details"`
}

type cashfreerV2BeneficiaryDetails struct {
	BeneficiaryID string `json:"beneficiary_id"`
}

type cashfreeV2BeneficiaryRequest struct {
	BeneficiaryID                string                      `json:"beneficiary_id"`
	BeneficiaryName              string                      `json:"beneficiary_name"`
	BeneficiaryInstrumentDetails cashfreeV2InstrumentDetails `json:"beneficiary_instrument_details"`
	BeneficiaryContactDetails    *cashfreeV2ContactDetails   `json:"beneficiary_contact_details,omitempty"`
}

type cashfreeV2InstrumentDetails struct {
	BankAccountNumber string `json:"bank_account_number,omitempty"`
	BankIFSC          string `json:"bank_ifsc,omitempty"`
	VPA               string `json:"vpa,omitempty"`
}

type cashfreeV2ContactDetails struct {
	BeneficiaryEmail       string `json:"beneficiary_email,omitempty"`
	BeneficiaryPhone       string `json:"beneficiary_phone,omitempty"`
	BeneficiaryCountryCode string `json:"beneficiary_country_code,omitempty"`
	BeneficiaryAddress     string `json:"beneficiary_address,omitempty"`
	BeneficiaryCity        string `json:"beneficiary_city,omitempty"`
	BeneficiaryState       string `json:"beneficiary_state,omitempty"`
	BeneficiaryPostalCode  string `json:"beneficiary_postal_code,omitempty"`
}

// RegisterBeneficiary creates/registers a beneficiary on Cashfree without initiating a transfer.
func (p *CashfreeProvider) RegisterBeneficiary(ctx context.Context, req TransferRequest) error {
	fmt.Printf("[CASHFREE] RegisterBeneficiary called: beneId=%s, name=%s, type=%s\n", req.BeneID, req.BeneficiaryName, req.MethodType)

	// 1) Authorize to get a short-lived token for payouts
	token, err := p.authorize(ctx)
	if err != nil {
		fmt.Printf("[CASHFREE] RegisterBeneficiary authorize error: %v\n", err)
		return fmt.Errorf("failed to authorize cashfree payout: %w", err)
	}
	p.cfg.BearerToken = token

	// 2) Beneficiary id must be provided by caller (payout method ID from service layer)
	beneID := strings.TrimSpace(req.BeneID)
	if beneID == "" {
		return fmt.Errorf("beneficiary ID is required (must be provided by service layer)")
	}

	fmt.Printf("[CASHFREE] RegisterBeneficiary: checking beneficiary ID: %s\n", beneID)

	// Try to create/register the beneficiary
	// If it already exists, Cashfree will handle the idempotency and return an error we can safely ignore
	fmt.Printf("[CASHFREE] RegisterBeneficiary: attempting to create beneficiary (idempotent operation)...\n")
	if err := p.addBeneficiary(ctx, beneID, req); err != nil {
		// Check if error is "already exists" - if so, continue (idempotent)
		if strings.Contains(err.Error(), "already") || strings.Contains(err.Error(), "exists") {
			fmt.Printf("[CASHFREE] RegisterBeneficiary: beneficiary already exists (expected for idempotent operation)\n")
		} else {
			// Real error
			fmt.Printf("[CASHFREE] RegisterBeneficiary addBeneficiary error: %v\n", err)
			return fmt.Errorf("failed to create beneficiary: %w", err)
		}
	} else {
		fmt.Printf("[CASHFREE] RegisterBeneficiary: beneficiary created successfully\n")
	}

	fmt.Printf("[CASHFREE] RegisterBeneficiary succeeded: beneId=%s\n", beneID)
	return nil
}

func (p *CashfreeProvider) InitiateTransfer(ctx context.Context, req TransferRequest) (*TransferResponse, error) {
	// 1) Authorize to get a short-lived token for payouts
	token, err := p.authorize(ctx)
	if err != nil {
		fmt.Printf("[CASHFREE] Authorize error: %v\n", err)
		return nil, fmt.Errorf("failed to authorize cashfree payout: %w", err)
	}
	p.cfg.BearerToken = token

	// 2) Beneficiary id must be provided by caller (payout method ID from service layer)
	beneID := strings.TrimSpace(req.BeneID)
	if beneID == "" {
		return nil, fmt.Errorf("beneficiary ID is required (must be provided by service layer)")
	}

	fmt.Printf("[CASHFREE] Step 2: Checking beneficiary ID: %s\n", beneID)

	// Try to create/register the beneficiary
	// If it already exists, Cashfree will handle the idempotency and return an error we can safely ignore
	fmt.Printf("[CASHFREE] Attempting to create beneficiary (idempotent operation)...\n")
	if err := p.addBeneficiary(ctx, beneID, req); err != nil {
		// Check if error is "already exists" - if so, continue (idempotent)
		if strings.Contains(err.Error(), "already") || strings.Contains(err.Error(), "exists") {
			fmt.Printf("[CASHFREE] Beneficiary already exists (expected for idempotent operation)\n")
		} else {
			// Real error
			fmt.Printf("[CASHFREE] addBeneficiary error: %v\n", err)
			return nil, fmt.Errorf("failed to create beneficiary: %w", err)
		}
	} else {
		fmt.Printf("[CASHFREE] Beneficiary created/verified successfully\n")
	}

	// 3) Build transfer request and include the beneId
	payoutReq, err := buildCashfreeTransferRequest(req)
	if err != nil {
		fmt.Printf("[CASHFREE] InitiateTransfer error: %v\n", err)
		return nil, err
	}
	// Set beneficiary ID in the transfer request
	payoutReq.BeneficiaryDetails.BeneficiaryID = beneID

	body, err := json.Marshal(payoutReq)
	if err != nil {
		fmt.Printf("[CASHFREE] Marshal error: %v\n", err)
		return nil, fmt.Errorf("failed to marshal cashfree payout request: %w", err)
	}

	// Use the Direct Transfer v1.2 endpoint for payout requests
	fmt.Printf("[CASHFREE] InitiateTransfer request: paymentID=%s, amount=%d, method=%s, url=%s\n",
		req.PaymentID, req.AmountCents, req.MethodType, strings.TrimRight(p.cfg.BaseURL, "/")+"/payout/transfers")

	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/payout/transfers"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fmt.Printf("[CASHFREE] HTTP request creation error: %v\n", err)
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	if err := p.setHeaders(httpReq); err != nil {
		fmt.Printf("[CASHFREE] Header setup error: %v\n", err)
		return nil, fmt.Errorf("failed to set headers: %w", err)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		fmt.Printf("[CASHFREE] HTTP call error: %v\n", err)
		return nil, fmt.Errorf("cashfree API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("[CASHFREE] Response read error: %v\n", err)
		return nil, fmt.Errorf("failed to read cashfree response: %w", err)
	}

	fmt.Printf("[CASHFREE] Response received: httpStatus=%d, bodyLength=%d, body=%s\n",
		resp.StatusCode, len(respBody), string(respBody))

	if resp.StatusCode >= 400 {
		errorResp := &TransferResponse{
			Status: "failed",
			Error:  fmt.Sprintf("cashfree API error (HTTP %d): %s", resp.StatusCode, string(respBody)),
		}
		fmt.Printf("[CASHFREE] API error response: %v\n", errorResp)
		return errorResp, nil
	}

	parsed, err := parseCashfreeTransferResponse(respBody)
	if err != nil {
		fmt.Printf("[CASHFREE] Response parse error: %v\n", err)
		return nil, fmt.Errorf("failed to parse cashfree response: %w", err)
	}

	fmt.Printf("[CASHFREE] Response parsed: status=%s, providerRefID=%s, error=%s\n",
		parsed.Status, parsed.ProviderRefID, parsed.Error)

	if parsed.ProviderRefID == "" {
		parsed.ProviderRefID = req.PaymentID.String()
		fmt.Printf("[CASHFREE] No provider ref ID in response, using paymentID: %s\n", parsed.ProviderRefID)
	}

	fmt.Printf("[CASHFREE] InitiateTransfer completed: paymentID=%s, status=%s\n", req.PaymentID, parsed.Status)
	return parsed, nil
}

func (p *CashfreeProvider) CheckStatus(ctx context.Context, providerRefID string) (*TransferResponse, error) {
	if providerRefID == "" {
		return nil, fmt.Errorf("providerRefID is required")
	}

	path := "/payout/transfers/" + providerRefID
	url := strings.TrimRight(p.cfg.BaseURL, "/") + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	if err := p.setHeaders(httpReq); err != nil {
		return nil, fmt.Errorf("failed to set headers: %w", err)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cashfree API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read cashfree response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cashfree API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	parsed, err := parseCashfreeTransferResponse(respBody)
	if err != nil {
		return nil, fmt.Errorf("failed to parse cashfree response: %w", err)
	}

	if parsed.ProviderRefID == "" {
		parsed.ProviderRefID = providerRefID
	}

	return parsed, nil
}

// ValidateWebhookSignature verifies a Cashfree Payouts webhook.
//
// Cashfree signs webhooks as base64(HMAC-SHA256(x-webhook-timestamp + rawBody,
// clientSecret)), delivered in the x-webhook-signature header (see Cashfree
// "Webhook Signature Verification"). The signing key is the account CLIENT
// SECRET — Payouts has no separate webhook secret — so we try the client secret
// first, and also CASHFREE_WEBHOOK_SECRET if one was explicitly configured. For
// resilience across Cashfree products/versions we also accept the legacy
// body-only message (no timestamp) and hex / base64url encodings.
func (p *CashfreeProvider) ValidateWebhookSignature(payload []byte, signature, timestamp string) bool {
	sig := strings.TrimSpace(signature)
	if sig == "" {
		return false
	}
	sig = strings.TrimPrefix(sig, "sha256=")

	// Candidate signing keys.
	var keys [][]byte
	if p.cfg.ClientSecret != "" {
		keys = append(keys, []byte(p.cfg.ClientSecret))
	}
	if p.cfg.WebhookSecret != "" {
		keys = append(keys, []byte(p.cfg.WebhookSecret))
	}
	if len(keys) == 0 {
		return false
	}

	// Candidate signed messages: Cashfree's correct timestamp-prefixed body,
	// plus the legacy body-only form as a fallback.
	msgs := [][]byte{}
	if ts := strings.TrimSpace(timestamp); ts != "" {
		msgs = append(msgs, append([]byte(ts), payload...))
	}
	msgs = append(msgs, payload)

	for _, key := range keys {
		for _, msg := range msgs {
			mac := hmac.New(sha256.New, key)
			mac.Write(msg)
			sum := mac.Sum(nil)

			if hmac.Equal([]byte(strings.ToLower(hex.EncodeToString(sum))), []byte(strings.ToLower(sig))) {
				return true
			}
			if hmac.Equal([]byte(base64.StdEncoding.EncodeToString(sum)), []byte(sig)) {
				return true
			}
			if hmac.Equal([]byte(base64.URLEncoding.EncodeToString(sum)), []byte(sig)) {
				return true
			}
		}
	}
	return false
}

func (p *CashfreeProvider) setHeaders(req *http.Request) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-client-id", p.cfg.ClientID)
	req.Header.Set("x-client-secret", p.cfg.ClientSecret)
	req.Header.Set("x-api-version", p.cfg.APIVersion)

	// Generate RSA signature: ClientId.timestamp encrypted with public key
	sig, timestamp, err := p.generateRSASignature()
	if err != nil {
		fmt.Printf("[CASHFREE] RSA signature generation error: %v\n", err)
		return err
	}

	req.Header.Set("x-cf-signature", sig)
	// Include timestamp header used to create the signature
	req.Header.Set("x-cf-timestamp", strconv.FormatInt(timestamp, 10))
	// Add Authorization header if we have a bearer token
	if strings.TrimSpace(p.cfg.BearerToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(p.cfg.BearerToken))
	}

	fmt.Printf("[CASHFREE] Headers set with x-cf-signature header %s\n", sig)
	return nil
}

// generateRSASignature creates RSA signature for Cashfree Payouts API 2FA
// Format: ClientId.timestamp encrypted with RSA public key, then base64 encoded
func (p *CashfreeProvider) generateRSASignature() (string, int64, error) {
	fmt.Printf("[CASHFREE_PROVIDER] generateRSASignature called, public key length: %d\n", len(p.cfg.PublicKey))

	if p.cfg.PublicKey == "" {
		fmt.Printf("[CASHFREE_PROVIDER] ERROR: Public key is empty!\n")
		return "", 0, fmt.Errorf("public key is not configured")
	}

	// Create message: clientId.timestamp (must be exact format)
	timestamp := time.Now().Unix()
	messageStr := fmt.Sprintf("%s.%d", p.cfg.ClientID, timestamp)
	messageBytes := []byte(messageStr)
	fmt.Printf("[CASHFREE_PROVIDER] RSA message string: %s\n", messageStr)
	fmt.Printf("[CASHFREE_PROVIDER] RSA message bytes length: %d\n", len(messageBytes))

	// Parse PEM-encoded public key
	pubKeyBlock, restPEM := pem.Decode([]byte(p.cfg.PublicKey))
	if pubKeyBlock == nil {
		fmt.Printf("[CASHFREE_PROVIDER] ERROR: Failed to decode PEM block from key\n")
		return "", 0, fmt.Errorf("failed to decode public key PEM block")
	}

	fmt.Printf("[CASHFREE_PROVIDER] PEM block decoded successfully (type=%s, remaining=%d bytes)\n", pubKeyBlock.Type, len(restPEM))

	// Parse the public key
	pubKey, err := x509.ParsePKIXPublicKey(pubKeyBlock.Bytes)
	if err != nil {
		fmt.Printf("[CASHFREE_PROVIDER] ERROR: Failed to parse PKIX public key: %v\n", err)
		return "", 0, fmt.Errorf("failed to parse public key: %w", err)
	}

	rsaPubKey, ok := pubKey.(*rsa.PublicKey)
	if !ok {
		fmt.Printf("[CASHFREE_PROVIDER] ERROR: Public key is not RSA type\n")
		return "", 0, fmt.Errorf("public key is not RSA")
	}

	fmt.Printf("[CASHFREE_PROVIDER] RSA public key parsed successfully (keySize=%d bits)\n", rsaPubKey.N.BitLen())

	// Encrypt using RSA PKCS#1 v1.5 (Cashfree requirement for 2FA)
	ciphertext, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, rsaPubKey, messageBytes, nil)
	if err != nil {
		fmt.Printf("[CASHFREE_PROVIDER] ERROR: RSA PKCS#1 v1.5 encryption failed: %v\n", err)
		return "", 0, fmt.Errorf("failed to encrypt with RSA: %w", err)
	}

	fmt.Printf("[CASHFREE_PROVIDER] RSA encryption successful (ciphertext length=%d bytes)\n", len(ciphertext))

	// Base64 encode the ciphertext
	signature := base64.StdEncoding.EncodeToString(ciphertext)
	fmt.Printf("[CASHFREE_PROVIDER] Base64 signature length: %d chars\n", len(signature))
	fmt.Printf("[CASHFREE_PROVIDER] X-Cf-Signature header value (first 100 chars): %s...\n", signature[:minInt(100, len(signature))])
	fmt.Printf("[CASHFREE_PROVIDER] RSA signature generated successfully (PKCS#1 v1.5): clientID=%s, timestamp=%d\n", p.cfg.ClientID, timestamp)
	return signature, timestamp, nil
}

// authorize calls Cashfree /payout/v1/authorize to obtain a short-lived payout token.
func (p *CashfreeProvider) authorize(ctx context.Context) (string, error) {
	// Generate RSA signature for the authorize call
	sig, timestamp, err := p.generateRSASignature()
	if err != nil {
		return "", err
	}

	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/payout/v1/authorize"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create authorize request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Client-Id", p.cfg.ClientID)
	httpReq.Header.Set("X-Client-Secret", p.cfg.ClientSecret)
	httpReq.Header.Set("X-Cf-Signature", sig)
	httpReq.Header.Set("X-Cf-Timestamp", strconv.FormatInt(timestamp, 10))

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("authorize API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read authorize response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("authorize API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	// Try to extract token from common fields
	var payload map[string]interface{}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return "", fmt.Errorf("failed to parse authorize response: %w", err)
	}

	token := firstNonEmpty(
		lookupString(payload, "token"),
		lookupString(payload, "auth_token"),
		lookupString(payload, "access_token"),
		lookupNestedString(payload, "data", "token"),
		lookupNestedString(payload, "data", "auth_token"),
		lookupNestedString(payload, "data", "access_token"),
	)

	if token == "" {
		return "", fmt.Errorf("no token found in authorize response: %s", string(respBody))
	}

	fmt.Printf("[CASHFREE] authorize succeeded, token length=%d\n", len(token))
	return token, nil
}

// getBeneficiary queries Cashfree for the provided beneId. Returns true when found.
func (p *CashfreeProvider) getBeneficiary(ctx context.Context, beneId string) (bool, error) {
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/payout/beneficiary/" + beneId
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create getBeneficiary request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-version", p.cfg.APIVersion)
	httpReq.Header.Set("x-client-id", p.cfg.ClientID)
	httpReq.Header.Set("x-client-secret", p.cfg.ClientSecret)

	// Attach bearer token if available
	if strings.TrimSpace(p.cfg.BearerToken) != "" {
		httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(p.cfg.BearerToken))
	}
	// include signature for extra safety
	sig, ts, sigErr := p.generateRSASignature()
	if sigErr == nil {
		httpReq.Header.Set("x-cf-signature", sig)
		httpReq.Header.Set("x-cf-timestamp", strconv.FormatInt(ts, 10))
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("getBeneficiary API call failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("[CASHFREE] getBeneficiary response: status=%d, body=%s\n", resp.StatusCode, string(body))

	if resp.StatusCode == 404 {
		fmt.Printf("[CASHFREE] Beneficiary not found: %s\n", string(body))
		return false, fmt.Errorf("beneficiary not found: %s", string(body))
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("getBeneficiary API error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	// Parse JSON response and ensure beneficiary status is VERIFIED
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, fmt.Errorf("failed to parse getBeneficiary response: %w", err)
	}

	// Expected response shape: check both top-level and nested data.beneficiary_status
	dataStatus := firstNonEmpty(
		lookupString(payload, "beneficiary_status"),
		lookupNestedString(payload, "data", "beneficiary_status"),
	)
	fmt.Printf("[CASHFREE] Beneficiary status: %s\n", dataStatus)

	if strings.TrimSpace(strings.ToUpper(dataStatus)) == "VERIFIED" {
		fmt.Printf("[CASHFREE] Beneficiary is VERIFIED\n")
		return true, nil
	}

	// If status present but not VERIFIED, return an error so caller can decide
	if dataStatus != "" {
		return false, fmt.Errorf("beneficiary exists but not verified: %s", dataStatus)
	}

	// If no beneficiary_status, treat as found but unknown state
	fmt.Printf("[CASHFREE] Beneficiary status unknown, treating as found\n")
	return true, nil
}

// addBeneficiary registers a beneficiary with Cashfree using provided transfer request details.
// Uses Cashfree Payouts API v2 format.
func (p *CashfreeProvider) addBeneficiary(ctx context.Context, beneId string, req TransferRequest) error {
	url := strings.TrimRight(p.cfg.BaseURL, "/") + "/payout/beneficiary"

	beneficiaryName := strings.TrimSpace(req.BeneficiaryName)
	if beneficiaryName == "" {
		beneficiaryName = "MySlotMate Host"
	}

	fmt.Printf("[CASHFREE] addBeneficiary: methodType=%s, beneId=%s, name=%s, account=%s, ifsc=%s, upi=%s\n",
		req.MethodType, beneId, beneficiaryName, req.AccountNumber, req.IFSC, req.UPIID)

	// Build v2 request structure
	reqPayload := &cashfreeV2BeneficiaryRequest{
		BeneficiaryID:   beneId,
		BeneficiaryName: beneficiaryName,
		BeneficiaryInstrumentDetails: cashfreeV2InstrumentDetails{
			BankAccountNumber: req.AccountNumber,
			BankIFSC:          req.IFSC,
			VPA:               req.UPIID,
		},
		BeneficiaryContactDetails: &cashfreeV2ContactDetails{
			// We can leave contact details mostly empty since we only have essential bank/UPI info
		},
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal addBeneficiary payload: %w", err)
	}

	fmt.Printf("[CASHFREE] addBeneficiary request payload: %s\n", string(bodyBytes))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create addBeneficiary request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-version", p.cfg.APIVersion)
	httpReq.Header.Set("x-client-id", p.cfg.ClientID)
	httpReq.Header.Set("x-client-secret", p.cfg.ClientSecret)

	// Attach bearer token if available
	if strings.TrimSpace(p.cfg.BearerToken) != "" {
		httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(p.cfg.BearerToken))
	}
	// include signature headers for added safety
	sig, ts, sigErr := p.generateRSASignature()
	if sigErr == nil {
		httpReq.Header.Set("x-cf-signature", sig)
		httpReq.Header.Set("x-cf-timestamp", strconv.FormatInt(ts, 10))
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("addBeneficiary API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	fmt.Printf("[CASHFREE] addBeneficiary response: status=%d, body=%s\n", resp.StatusCode, string(respBody))

	if resp.StatusCode >= 400 {
		return fmt.Errorf("addBeneficiary API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	// Parse response to verify beneficiary was created
	var addResp map[string]interface{}
	if err := json.Unmarshal(respBody, &addResp); err != nil {
		fmt.Printf("[CASHFREE] Warning: could not parse addBeneficiary response: %v\n", err)
		// But don't fail - assume it worked if status was < 400
	}

	fmt.Printf("[CASHFREE] addBeneficiary completed: beneId=%s, response=%+v\n", beneId, addResp)
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func buildCashfreeTransferRequest(req TransferRequest) (*cashfreeTransferReq, error) {
	payoutReq := &cashfreeTransferReq{
		TransferID:       req.PaymentID.String(),
		TransferAmount:   fmt.Sprintf("%.2f", float64(req.AmountCents)/100.0),
		TransferCurrency: "INR",
		// Use a concise, provider-friendly remark for payouts
		TransferRemarks: "payout withdrawal",
		BeneficiaryDetails: cashfreerV2BeneficiaryDetails{
			BeneficiaryID: "", // Will be set in InitiateTransfer
		},
	}

	switch strings.ToLower(req.MethodType) {
	case "bank":
		if req.AccountNumber == "" || req.IFSC == "" {
			return nil, fmt.Errorf("bank payout requires account number and IFSC")
		}
		payoutReq.TransferMode = "banktransfer"

	case "upi":
		if req.UPIID == "" {
			return nil, fmt.Errorf("upi payout requires upi id")
		}
		payoutReq.TransferMode = "upi"

	default:
		return nil, fmt.Errorf("unsupported payout method type: %s", req.MethodType)
	}

	return payoutReq, nil
}

func parseCashfreeTransferResponse(respBody []byte) (*TransferResponse, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, err
	}

	providerRefID := firstNonEmpty(
		lookupString(payload, "cf_transfer_id"),
		lookupString(payload, "transfer_id"),
		lookupString(payload, "id"),
		lookupNestedString(payload, "data", "cf_transfer_id"),
		lookupNestedString(payload, "data", "transfer_id"),
		lookupNestedString(payload, "data", "id"),
		lookupNestedString(payload, "transfer", "cf_transfer_id"),
		lookupNestedString(payload, "transfer", "transfer_id"),
	)

	statusRaw := firstNonEmpty(
		lookupString(payload, "transfer_status"),
		lookupString(payload, "status"),
		lookupString(payload, "event"),
		lookupNestedString(payload, "data", "transfer_status"),
		lookupNestedString(payload, "data", "status"),
		lookupNestedString(payload, "data", "event"),
		lookupNestedString(payload, "transfer", "status"),
		lookupNestedString(payload, "transfer", "transfer_status"),
	)

	errMsg := firstNonEmpty(
		lookupString(payload, "message"),
		lookupString(payload, "reason"),
		lookupString(payload, "error"),
		lookupString(payload, "error_message"),
		lookupNestedString(payload, "data", "message"),
		lookupNestedString(payload, "data", "reason"),
		lookupNestedString(payload, "data", "error"),
		lookupNestedString(payload, "transfer", "reason"),
	)

	return &TransferResponse{
		ProviderRefID: providerRefID,
		Status:        mapCashfreeStatus(statusRaw),
		Error:         errMsg,
	}, nil
}

func mapCashfreeStatus(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "SUCCESS", "COMPLETED", "PROCESSED", "TRANSFER_SUCCESS":
		return "completed"
	case "FAILED", "REJECTED", "CANCELLED", "CANCELED", "TRANSFER_FAILED":
		return "failed"
	case "REVERSED", "RETURNED", "TRANSFER_REVERSED":
		return "reversed"
	default:
		return "processing"
	}
}

func lookupString(values map[string]interface{}, key string) string {
	v, ok := values[key]
	if !ok {
		return ""
	}
	return toString(v)
}

func lookupNestedString(values map[string]interface{}, path ...string) string {
	if len(path) == 0 {
		return ""
	}

	var current interface{} = values
	for _, part := range path {
		asMap, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current, ok = asMap[part]
		if !ok {
			return ""
		}
	}

	return toString(current)
}

func toString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case fmt.Stringer:
		return strings.TrimSpace(t.String())
	case float64:
		return fmt.Sprintf("%.0f", t)
	case int64:
		return fmt.Sprintf("%d", t)
	case int:
		return fmt.Sprintf("%d", t)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
