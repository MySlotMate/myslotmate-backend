package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// KapsoClient handles integration with Kapso AI for WhatsApp notifications
type KapsoClient struct {
	apiKey        string
	phoneNumberID string
	httpClient    *http.Client
}

// NewKapsoClient creates and returns a new KapsoClient
func NewKapsoClient(apiKey, phoneNumberID string) *KapsoClient {
	return &KapsoClient{
		apiKey:        apiKey,
		phoneNumberID: phoneNumberID,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// formatKapsoPhone formats a phone number for the Kapso WhatsApp API.
// It removes any non-digit character, strips a leading 0, and adds 91 (for India) if the length is 10.
func formatKapsoPhone(phone string) string {
	cleaned := ""
	for _, ch := range phone {
		if ch >= '0' && ch <= '9' {
			cleaned += string(ch)
		}
	}

	// Remove leading zero if present
	if strings.HasPrefix(cleaned, "0") {
		cleaned = cleaned[1:]
	}

	// If length is 10, prefix with 91 (India)
	if len(cleaned) == 10 {
		cleaned = "91" + cleaned
	}

	return cleaned
}

func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(s)
}

// UploadMedia uploads media (like a PDF ticket) to Kapso meta API.
// Returns the uploaded media ID.
func (c *KapsoClient) UploadMedia(ctx context.Context, fileName string, fileBytes []byte) (string, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add messaging_product field
	if err := writer.WriteField("messaging_product", "whatsapp"); err != nil {
		return "", fmt.Errorf("failed to write form field: %w", err)
	}

	// Add file field with explicit Content-Type
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(fileName)))
	h.Set("Content-Type", "application/pdf")
	part, err := writer.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := part.Write(fileBytes); err != nil {
		return "", fmt.Errorf("failed to write file to part: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("failed to close writer: %w", err)
	}

	url := fmt.Sprintf("https://api.kapso.ai/meta/whatsapp/v24.0/%s/media", c.phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("upload media returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &res); err != nil {
		return "", fmt.Errorf("failed to parse upload response: %w", err)
	}

	if res.ID == "" {
		return "", fmt.Errorf("empty media ID in response")
	}

	return res.ID, nil
}

// SendDocumentMessage sends a document message to a user
func (c *KapsoClient) SendDocumentMessage(ctx context.Context, to string, mediaID string, filename string, caption string) error {
	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                formatKapsoPhone(to),
		"type":              "document",
		"document": map[string]string{
			"id":       mediaID,
			"caption":  caption,
			"filename": filename,
		},
	}

	return c.sendPayload(ctx, payload)
}

// SendTextMessage sends a text message to a user
func (c *KapsoClient) SendTextMessage(ctx context.Context, to string, body string) error {
	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                formatKapsoPhone(to),
		"type":              "text",
		"text": map[string]string{
			"body": body,
		},
	}

	return c.sendPayload(ctx, payload)
}

// TemplateComponent represents a component in a WhatsApp template message
type TemplateComponent struct {
	Type       string              `json:"type"` // "header", "body", "button"
	SubType    string              `json:"sub_type,omitempty"`
	Index      string              `json:"index,omitempty"`
	Parameters []TemplateParameter `json:"parameters"`
}

// TemplateParameter represents a parameter inside a TemplateComponent
type TemplateParameter struct {
	Type          string            `json:"type"` // "text", "currency", "date_time", "image", "document", "video"
	Text          string            `json:"text,omitempty"`
	ParameterName string            `json:"parameter_name,omitempty"` // For templates that use named parameters
	Currency      *TemplateCurrency `json:"currency,omitempty"`
	DateTime      *TemplateDateTime `json:"date_time,omitempty"`
	Image         *TemplateMedia    `json:"image,omitempty"`
	Document      *TemplateMedia    `json:"document,omitempty"`
	Video         *TemplateMedia    `json:"video,omitempty"`
}

// TemplateCurrency represents currency parameter structure
type TemplateCurrency struct {
	FallbackValue string `json:"fallback_value"`
	Code          string `json:"code"`
	Amount1000    int    `json:"amount_1000"`
}

// TemplateDateTime represents datetime parameter structure
type TemplateDateTime struct {
	FallbackValue string `json:"fallback_value"`
}

// TemplateMedia represents media parameters structure (ID or external link)
type TemplateMedia struct {
	ID       string `json:"id,omitempty"`
	Link     string `json:"link,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// SendTemplateMessage sends a WhatsApp template message
func (c *KapsoClient) SendTemplateMessage(ctx context.Context, to string, templateName string, languageCode string, components []TemplateComponent) error {
	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                formatKapsoPhone(to),
		"type":              "template",
		"template": map[string]interface{}{
			"name": templateName,
			"language": map[string]string{
				"code": languageCode,
			},
			"components": components,
		},
	}

	return c.sendPayload(ctx, payload)
}

// sendPayload sends JSON payload to Kapso meta messages API
func (c *KapsoClient) sendPayload(ctx context.Context, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("https://api.kapso.ai/meta/whatsapp/v24.0/%s/messages", c.phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("send message returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// SendTicketTemplateMessage sends a booking ticket template message
func (c *KapsoClient) SendTicketTemplateMessage(ctx context.Context, to string, mediaID string, filename string, templateName string, languageCode string, userName string, eventName string) error {
	components := []TemplateComponent{
		{
			Type: "header",
			Parameters: []TemplateParameter{
				{
					Type: "document",
					Document: &TemplateMedia{
						ID:       mediaID,
						Filename: filename,
					},
				},
			},
		},
		{
			Type: "body",
			Parameters: []TemplateParameter{
				{
					Type:          "text",
					ParameterName: "name",
					Text:          userName,
				},
				{
					Type:          "text",
					ParameterName: "event_name",
					Text:          eventName,
				},
			},
		},
	}

	return c.SendTemplateMessage(ctx, to, templateName, languageCode, components)
}

// SendHostPendingAlertTemplateMessage sends a host application pending template message
func (c *KapsoClient) SendHostPendingAlertTemplateMessage(ctx context.Context, to string, templateName string, languageCode string, hostName string, hostCity string, hostPhone string) error {
	components := []TemplateComponent{
		{
			Type: "body",
			Parameters: []TemplateParameter{
				{
					Type:          "text",
					ParameterName: "name",
					Text:          hostName,
				},
				{
					Type:          "text",
					ParameterName: "city",
					Text:          hostCity,
				},
				{
					Type:          "text",
					ParameterName: "phone",
					Text:          hostPhone,
				},
			},
		},
	}

	return c.SendTemplateMessage(ctx, to, templateName, languageCode, components)
}

// SendTwoParamTemplateMessage sends a body-only template with two named text
// parameters. Both RSVP templates have that shape — "{{name}} asked to join
// {{event_name}}" and "{{name}}, you're approved for {{event_name}}" — so one
// sender covers them and any future template of the same form.
func (c *KapsoClient) SendTwoParamTemplateMessage(
	ctx context.Context,
	to, templateName, languageCode string,
	firstName, firstValue string,
	secondName, secondValue string,
) error {
	components := []TemplateComponent{
		{
			Type: "body",
			Parameters: []TemplateParameter{
				{Type: "text", ParameterName: firstName, Text: firstValue},
				{Type: "text", ParameterName: secondName, Text: secondValue},
			},
		},
	}
	return c.SendTemplateMessage(ctx, to, templateName, languageCode, components)
}
