package twofactor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Client defines the interface for communicating with 2Factor.in
type Client interface {
	SendOTP(ctx context.Context, phone string) (string, error)
	VerifyOTP(ctx context.Context, sessionID string, otp string) (bool, error)
}

type twoFactorClient struct {
	apiKey       string
	templateName string
	client       *http.Client
}

// NewClient creates a new 2Factor API client
func NewClient(apiKey string, templateName string) Client {
	return &twoFactorClient{
		apiKey:       apiKey,
		templateName: templateName,
		client:       &http.Client{Timeout: 10 * time.Second},
	}
}

type twoFactorResponse struct {
	Status  string `json:"Status"`
	Details string `json:"Details"`
}

func (c *twoFactorClient) SendOTP(ctx context.Context, phone string) (string, error) {
	// Developer mock mode if key is empty, "mock", or dev default
	if c.apiKey == "" || c.apiKey == "mock" || c.apiKey == "dev-insecure-admin-secret-change-me" {
		fmt.Printf("[2FACTOR MOCK] OTP requested for phone %s. Use verification code '123456'.\n", phone)
		return "mock-session-id-" + phone, nil
	}

	apiURL := fmt.Sprintf("https://2factor.in/API/V1/%s/SMS/%s/AUTOGEN", url.PathEscape(c.apiKey), url.PathEscape(phone))
	if c.templateName != "" {
		apiURL = fmt.Sprintf("https://2factor.in/API/V1/%s/SMS/%s/AUTOGEN/%s", url.PathEscape(c.apiKey), url.PathEscape(phone), url.PathEscape(c.templateName))
	}
	fmt.Printf("[2FACTOR] Sending OTP via URL: %s\n", apiURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		fmt.Printf("[2FACTOR] Request failed: %v\n", err)
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[2FACTOR] Non-200 response: status=%d\n", resp.StatusCode)
		return "", fmt.Errorf("2factor API returned status %d", resp.StatusCode)
	}

	var res twoFactorResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		fmt.Printf("[2FACTOR] Failed to decode response: %v\n", err)
		return "", err
	}

	fmt.Printf("[2FACTOR] Response status: %s, details: %s\n", res.Status, res.Details)
	if res.Status != "Success" {
		return "", fmt.Errorf("2factor API error: %s", res.Details)
	}

	return res.Details, nil // The session ID is in the Details field on Success
}

func (c *twoFactorClient) VerifyOTP(ctx context.Context, sessionID string, otp string) (bool, error) {
	if c.apiKey == "" || c.apiKey == "mock" || c.apiKey == "dev-insecure-admin-secret-change-me" {
		if otp == "123456" {
			return true, nil
		}
		return false, nil
	}

	apiURL := fmt.Sprintf("https://2factor.in/API/V1/%s/SMS/VERIFY/%s/%s", url.PathEscape(c.apiKey), url.PathEscape(sessionID), url.PathEscape(otp))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return false, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Attempt to read error detail from 2Factor response (like wrong OTP)
		var res twoFactorResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err == nil {
			if res.Status == "Error" && (res.Details == "OTP Mismatch" || res.Details == "OTP Expired") {
				return false, nil
			}
		}
		return false, fmt.Errorf("2factor verification API returned status %d", resp.StatusCode)
	}

	var res twoFactorResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return false, err
	}

	if res.Status == "Success" && res.Details == "OTP Verified" {
		return true, nil
	}

	return false, nil
}
