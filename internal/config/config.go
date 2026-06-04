package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type FirebaseConfig struct {
	CredentialsFile string
	ProjectID       string
}

// S3Config holds AWS S3 settings for file uploads.
type S3Config struct {
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

// TwilioConfig holds Twilio credentials for SMS and WhatsApp messaging.
type TwilioConfig struct {
	AccountSID       string
	AuthToken        string
	PhoneNumber      string // Twilio phone number for SMS
	WhatsappNumber   string // Twilio WhatsApp number (usually +1... number)
	TemplateEventSID string // Event reminder template SID (optional)
}

// SMTPConfig holds SMTP server settings for sending emails.
type SMTPConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	FromName string // Sender name (e.g., "MySlotMate")
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	URL string // DATABASE_URL, Postgres connection string
}

type SetuConfig struct {
	BaseURL           string
	ClientID          string
	ClientSecret      string
	ProductInstanceID string
}

// AadharConfig controls which KYC strategy implementation to use.
type AadharConfig struct {
	Provider string // setu | mock
	MockOTP  string
	MockName string
}

// RazorpayConfig holds Razorpay Standard payment collection credentials.
type RazorpayConfig struct {
	KeyID                string
	KeySecret            string
	WebhookSecret        string // legacy/shared webhook secret fallback
	PaymentWebhookSecret string // payment collection webhook secret
}

// CashfreeConfig holds Cashfree payout credentials.
type CashfreeConfig struct {
	BaseURL       string
	ClientID      string
	ClientSecret  string
	PublicKey     string // PEM-encoded RSA public key for signature generation
	WebhookSecret string
	APIVersion    string
}

// GeminiConfig holds Gemini API credentials for the RAG chatbot.
type GeminiConfig struct {
	APIKey             string
	EmbeddingDimension int
}

// PineconeConfig holds Pinecone vector database credentials
type PineconeConfig struct {
	APIKey      string
	Host        string
	IndexName   string
	Environment string
}

// AdminAuthConfig holds credentials and signing settings for the static
// admin-dashboard login. The dashboard authenticates with a fixed
// username/password and receives a signed JWT used for subsequent requests.
type AdminAuthConfig struct {
	Username  string        // ADMIN_USERNAME — static login identifier (email or username)
	Password  string        // ADMIN_PASSWORD — static login password
	JWTSecret string        // ADMIN_JWT_SECRET — HMAC secret used to sign session tokens
	TokenTTL  time.Duration // session token lifetime (from ADMIN_TOKEN_TTL_HOURS)
}

type Config struct {
	HTTPPort          string
	AdminEmail        string
	RenderExternalURL string // RENDER_EXTERNAL_URL, used for self-ping keep-alive
	AdminAuth         AdminAuthConfig
	Firebase          FirebaseConfig
	Database          DatabaseConfig
	Aadhar            AadharConfig
	Setu              SetuConfig
	Razorpay          RazorpayConfig
	Cashfree          CashfreeConfig
	S3                S3Config
	Twilio            TwilioConfig
	SMTP              SMTPConfig
	Gemini            GeminiConfig
	Pinecone          PineconeConfig
}

// Load reads configuration from environment variables (optionally via .env).
func Load() (*Config, error) {
	// Load .env if present; ignore errors because it's optional.
	_ = godotenv.Load()

	cfg := &Config{
		HTTPPort:          getEnv("HTTP_PORT", "8080"),
		AdminEmail:        getEnv("ADMIN_EMAIL", ""),
		RenderExternalURL: getEnv("RENDER_EXTERNAL_URL", ""),
		AdminAuth: AdminAuthConfig{
			Username:  getEnv("ADMIN_USERNAME", "admin@myslotmate.com"),
			Password:  getEnv("ADMIN_PASSWORD", "Admin@12345"),
			JWTSecret: getEnv("ADMIN_JWT_SECRET", "dev-insecure-admin-secret-change-me"),
			TokenTTL:  time.Duration(parseEnvInt("ADMIN_TOKEN_TTL_HOURS", 12)) * time.Hour,
		},
		Firebase: FirebaseConfig{
			CredentialsFile: getEnv("FIREBASE_CREDENTIALS_FILE", "config/firebase-service-account.json"),
			ProjectID:       getEnv("FIREBASE_PROJECT_ID", "myslotmate-25994"),
		},
		S3: S3Config{
			Bucket:    getEnv("AWS_S3_BUCKET", ""),
			Region:    getEnv("AWS_S3_REGION", "ap-south-1"),
			AccessKey: getEnv("AWS_ACCESS_KEY_ID", ""),
			SecretKey: getEnv("AWS_SECRET_ACCESS_KEY", ""),
		},
		Database: DatabaseConfig{
			URL: getEnv("DATABASE_URL", ""),
		},
		Aadhar: AadharConfig{
			Provider: getEnv("AADHAR_PROVIDER", "setu"),
			MockOTP:  getEnv("AADHAR_MOCK_OTP", "123456"),
			MockName: getEnv("AADHAR_MOCK_NAME", "Test User"),
		},
		Setu: SetuConfig{
			BaseURL:           getEnv("SETU_BASE_URL", "https://uat.setu.co"),
			ClientID:          getEnv("SETU_CLIENT_ID", ""),
			ClientSecret:      getEnv("SETU_CLIENT_SECRET", ""),
			ProductInstanceID: getEnv("SETU_PRODUCT_INSTANCE_ID", ""),
		},
		Razorpay: RazorpayConfig{
			KeyID:                getEnv("RAZORPAY_KEY_ID", ""),
			KeySecret:            getEnv("RAZORPAY_KEY_SECRET", ""),
			WebhookSecret:        getEnv("RAZORPAY_WEBHOOK_SECRET", ""),
			PaymentWebhookSecret: getEnv("RAZORPAY_PAYMENT_WEBHOOK_SECRET", ""),
		},
		Cashfree: CashfreeConfig{
			// Prefer the explicit payout API base URL; fall back to legacy CASHFREE_BASE_URL.
			BaseURL: func() string {
				if v := getEnv("CASHFREE_PAYOUT_BASE_URL", ""); v != "" {
					fmt.Printf("[CONFIG] Using CASHFREE_PAYOUT_BASE_URL: %s\n", v)
					return v
				}
				return getEnv("CASHFREE_BASE_URL", "https://payout-api.cashfree.com")
			}(),
			ClientID:      getEnv("CASHFREE_CLIENT_ID", ""),
			ClientSecret:  getEnv("CASHFREE_CLIENT_SECRET", ""),
			WebhookSecret: getEnv("CASHFREE_WEBHOOK_SECRET", ""),
			APIVersion:    getEnv("CASHFREE_API_VERSION", "2024-01-01"),
			PublicKey:     loadPublicKey(getEnv("CASHFREE_PUBLIC_KEY_FILE", "config/cashfree-public-key.pem")),
		},
		Twilio: TwilioConfig{
			AccountSID:       getEnv("TWILIO_ACCOUNT_SID", ""),
			AuthToken:        getEnv("TWILIO_AUTH_TOKEN", ""),
			PhoneNumber:      getEnv("TWILIO_PHONE_NUMBER", ""),
			WhatsappNumber:   getEnv("TWILIO_WHATSAPP_NUMBER", ""),
			TemplateEventSID: getEnv("TWILIO_TEMPLATE_EVENT_SID", ""),
		},
		SMTP: SMTPConfig{
			Host:     getEnv("SMTP_HOST", ""),
			Port:     parseEnvInt("SMTP_PORT", 587),
			User:     getEnv("SMTP_USER", ""),
			Password: getEnv("SMTP_PASSWORD", ""),
			FromName: getEnv("SMTP_FROM_NAME", "MySlotMate"),
		},
		Gemini: GeminiConfig{
			APIKey:             getEnv("GEMINI_API_KEY", ""),
			EmbeddingDimension: parseEnvInt("GEMINI_EMBEDDING_DIMENSION", 1536),
		},
		Pinecone: PineconeConfig{
			APIKey:      getEnv("PINECONE_API_KEY", ""),
			Host:        getEnv("PINECONE_HOST", ""),
			IndexName:   getEnv("PINECONE_INDEX_NAME", "myslotmate-rag"),
			Environment: getEnv("PINECONE_ENVIRONMENT", "us-east-1"),
		},
	}

	// Log Cashfree public key status
	if cfg.Cashfree.PublicKey == "" {
		fmt.Printf("[CONFIG] ERROR: Cashfree public key is EMPTY!\n")
	} else {
		fmt.Printf("[CONFIG] Cashfree public key loaded successfully (length=%d)\n", len(cfg.Cashfree.PublicKey))
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func parseEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// loadPublicKey loads the Cashfree public key from an environment variable or PEM file.
// If the file doesn't exist or can't be read, returns an empty string.
func loadPublicKey(filePath string) string {
	// First, try to load from environment variable directly (for production/containerized deployments)
	if pubKey := os.Getenv("CASHFREE_PUBLIC_KEY"); pubKey != "" {
		fmt.Printf("[CONFIG] Loaded Cashfree public key from CASHFREE_PUBLIC_KEY environment variable (length=%d)\n", len(pubKey))
		// Handle escaped newlines from dotenv
		pubKey = strings.ReplaceAll(pubKey, "\\n", "\n")
		fmt.Printf("[CONFIG] After newline processing: length=%d\n", len(pubKey))
		return pubKey
	}

	// Try to read from the specified file path
	data, err := os.ReadFile(filePath)
	if err == nil {
		fmt.Printf("[CONFIG] Loaded Cashfree public key from file: %s\n", filePath)
		return string(data)
	}

	// Try alternative paths in case working directory is different
	alternativePaths := []string{
		filePath,
		"./config/cashfree-public-key.pem",
		"../config/cashfree-public-key.pem",
		"../../config/cashfree-public-key.pem",
		"/app/config/cashfree-public-key.pem",
	}

	for _, path := range alternativePaths {
		if path == filePath {
			continue // Skip the one we already tried
		}
		data, err := os.ReadFile(path)
		if err == nil {
			fmt.Printf("[CONFIG] Loaded Cashfree public key from alternative path: %s\n", path)
			return string(data)
		}
	}

	fmt.Printf("[CONFIG] Warning: could not load Cashfree public key from %s or alternative paths\n", filePath)
	return ""
}
