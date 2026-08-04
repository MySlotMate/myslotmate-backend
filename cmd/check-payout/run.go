// One-off: query Cashfree for a transfer's live status by our transfer_id
// (= the payment ID). Usage: go run ./cmd/check-payout <payment-id>
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"myslotmate-backend/internal/config"
	"myslotmate-backend/internal/lib/payout"

	"github.com/joho/godotenv"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: go run ./cmd/check-payout <payment-id/transfer-id>")
	}
	transferID := os.Args[1]

	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	provider := payout.NewCashfreeProvider(payout.CashfreeConfig{
		BaseURL:       cfg.Cashfree.BaseURL,
		ClientID:      cfg.Cashfree.ClientID,
		ClientSecret:  cfg.Cashfree.ClientSecret,
		PublicKey:     cfg.Cashfree.PublicKey,
		WebhookSecret: cfg.Cashfree.WebhookSecret,
		APIVersion:    cfg.Cashfree.APIVersion,
	})

	fmt.Printf("Cashfree base=%s api_version=%s client_id_set=%t public_key_set=%t\n",
		cfg.Cashfree.BaseURL, cfg.Cashfree.APIVersion, cfg.Cashfree.ClientID != "", cfg.Cashfree.PublicKey != "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := provider.CheckStatus(ctx, transferID)
	if err != nil {
		log.Fatalf("CheckStatus error: %v", err)
	}
	fmt.Printf("\n=== RESULT ===\ntransfer_id=%s\nmapped_status=%s\nprovider_ref=%s\nerror/reason=%s\n",
		transferID, resp.Status, resp.ProviderRefID, resp.Error)
}
