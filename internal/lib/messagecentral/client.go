package messagecentral

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const baseURL = "https://cpaas.messagecentral.com"

// Client defines the interface for OTP operations via Message Central VerifyNow.
type Client interface {
	SendOTP(ctx context.Context, phone string) (string, error)
	VerifyOTP(ctx context.Context, verificationID string, otp string) (bool, error)
}

type mcClient struct {
	customerID string
	encodedKey string // base64-encoded password
	httpClient *http.Client

	mu        sync.RWMutex
	authToken string
	tokenExp  time.Time
}

// NewClient creates a new Message Central VerifyNow client.
// If customerID is empty or "mock", the client operates in developer mock mode.
func NewClient(customerID, password string) Client {
	return &mcClient{
		customerID: customerID,
		encodedKey: base64.StdEncoding.EncodeToString([]byte(password)),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// --- API response types ---

type mcResponse struct {
	ResponseCode int    `json:"responseCode"`
	Message      string `json:"message"`
	Token        string `json:"token"`
	Data         mcData `json:"data"`
}

type mcData struct {
	VerificationID     json.Number `json:"verificationId"`
	MobileNumber       string      `json:"mobileNumber"`
	ResponseCode       json.Number `json:"responseCode"`
	ErrorMessage       string      `json:"errorMessage"`
	VerificationStatus string      `json:"verificationStatus"`
	AuthToken          string      `json:"authToken"`
	TransactionID      string      `json:"transactionId"`
	Timeout            json.Number `json:"timeout"`
}

// isMock returns true when the client should operate in developer mock mode.
func (c *mcClient) isMock() bool {
	return c.customerID == "" || c.customerID == "mock"
}

// getToken returns a cached auth token or fetches a new one from Message Central.
func (c *mcClient) getToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	if c.authToken != "" && time.Now().Before(c.tokenExp) {
		tok := c.authToken
		c.mu.RUnlock()
		return tok, nil
	}
	c.mu.RUnlock()

	// Fetch a new token.
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock.
	if c.authToken != "" && time.Now().Before(c.tokenExp) {
		return c.authToken, nil
	}

	params := url.Values{}
	params.Set("customerId", c.customerID)
	params.Set("key", c.encodedKey)
	params.Set("scope", "NEW")
	params.Set("country", "91")

	tokenURL := baseURL + "/auth/v1/authentication/token?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("messagecentral: failed to create token request: %w", err)
	}
	req.Header.Set("accept", "*/*")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("messagecentral: token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("messagecentral: failed to read token response body: %w", err)
	}

	fmt.Printf("[MSGCENTRAL] Token API HTTP %d, body: %s\n", resp.StatusCode, string(body))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("messagecentral: token API returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var res mcResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("messagecentral: failed to decode token response: %w", err)
	}

	// The auth token may be in data.authToken or at the top-level token field.
	authToken := res.Data.AuthToken
	if authToken == "" {
		authToken = res.Token
	}
	if authToken == "" {
		return "", fmt.Errorf("messagecentral: empty auth token (code=%d, msg=%s)", res.ResponseCode, res.Message)
	}

	c.authToken = authToken
	c.tokenExp = time.Now().Add(20 * time.Minute) // refresh well before the ~25 min expiry
	fmt.Printf("[MSGCENTRAL] Auth token refreshed successfully\n")

	return c.authToken, nil
}

// stripPhone extracts the country code and bare mobile number from "+91XXXXXXXXXX".
func stripPhone(phone string) (countryCode string, mobile string) {
	phone = strings.TrimSpace(phone)
	phone = strings.TrimPrefix(phone, "+")
	if strings.HasPrefix(phone, "91") && len(phone) > 10 {
		return "91", phone[2:]
	}
	// Fallback: assume India, use the whole string as mobile.
	return "91", phone
}

// SendOTP sends an OTP to the given phone number and returns a verificationID.
func (c *mcClient) SendOTP(ctx context.Context, phone string) (string, error) {
	if c.isMock() {
		fmt.Printf("[MSGCENTRAL MOCK] OTP requested for phone %s. Use verification code '123456'.\n", phone)
		return "mock-session-id-" + phone, nil
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return "", err
	}

	cc, mobile := stripPhone(phone)
	sendURL := fmt.Sprintf("%s/verification/v3/send?countryCode=%s&flowType=SMS&mobileNumber=%s",
		baseURL, cc, mobile)

	fmt.Printf("[MSGCENTRAL] Sending OTP to %s via SMS\n", phone)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sendURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("authToken", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		fmt.Printf("[MSGCENTRAL] Send OTP request failed: %v\n", err)
		return "", err
	}
	defer resp.Body.Close()

	var res mcResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		fmt.Printf("[MSGCENTRAL] Failed to decode send response: %v\n", err)
		return "", err
	}

	fmt.Printf("[MSGCENTRAL] Send response: code=%d, verificationId=%v, msg=%s\n",
		res.ResponseCode, res.Data.VerificationID, res.Message)

	if res.ResponseCode != 200 {
		errMsg := res.Data.ErrorMessage
		if errMsg == "" {
			errMsg = res.Message
		}
		return "", fmt.Errorf("messagecentral: send OTP failed: %s", errMsg)
	}

	return res.Data.VerificationID.String(), nil
}

// VerifyOTP validates the OTP code against the given verificationID.
func (c *mcClient) VerifyOTP(ctx context.Context, verificationID string, otp string) (bool, error) {
	if c.isMock() {
		return otp == "123456", nil
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return false, err
	}

	verifyURL := fmt.Sprintf("%s/verification/v3/validateOtp?verificationId=%s&code=%s",
		baseURL, verificationID, otp)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("authToken", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var res mcResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		// Non-200 HTTP status with undecodable body → treat as invalid OTP.
		if resp.StatusCode != http.StatusOK {
			return false, nil
		}
		return false, err
	}

	fmt.Printf("[MSGCENTRAL] Verify response: code=%d, status=%s\n",
		res.ResponseCode, res.Data.VerificationStatus)

	if res.ResponseCode == 200 && res.Data.VerificationStatus == "VERIFICATION_COMPLETED" {
		return true, nil
	}

	// Known error codes: 702=wrong OTP, 703=already verified, 705=expired
	return false, nil
}
